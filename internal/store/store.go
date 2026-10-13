package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"butler/internal/ids"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

type Store struct {
	db *sql.DB
}

// Agent is a registered agent instance: a (kind, hostname, workdir) triple
// identified by a server-generated name. Multiple instance tokens may be
// active at once (one per registration).
type Agent struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Kind        string `json:"kind"`
	Hostname    string `json:"hostname"`
	Workdir     string `json:"workdir"`
	CreatedAt   int64  `json:"created_at"`
	LastSeenAt  *int64 `json:"last_seen_at,omitempty"`
}

// Kind is an agent kind (codex, opencode, ...) with a single token shared by
// all instances of that kind for registration.
type Kind struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   int64  `json:"created_at"`
}

type Session struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	CreatedBy string `json:"created_by"`
	CreatedAt int64  `json:"created_at"`
}

type Message struct {
	Seq       int64  `json:"seq"`
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	SenderID  string `json:"sender_id"`
	Kind      string `json:"kind"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"created_at"`
}

type ContextItem struct {
	SessionID string `json:"session_id"`
	Key       string `json:"key"`
	Value     string `json:"value"`
	AuthorID  string `json:"author_id"`
	UpdatedAt int64  `json:"updated_at"`
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	var current int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		v, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("bad migration name %q", name)
		}
		if v <= current {
			continue
		}
		content, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if err := s.apply(v, string(content)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}
	return nil
}

func (s *Store) apply(version int, script string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range splitSQL(script) {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func splitSQL(script string) []string {
	lines := strings.Split(script, "\n")
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "--") {
			continue
		}
		kept = append(kept, l)
	}
	parts := strings.Split(strings.Join(kept, "\n"), ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

func isUniqueErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func isFKErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

// ---- kinds ----

func (s *Store) CreateKind(ctx context.Context, id, displayName, tokenHash string) (*Kind, error) {
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO kinds (id, display_name, token_hash, created_at) VALUES (?, ?, ?, ?)`,
		id, displayName, tokenHash, now); err != nil {
		if isUniqueErr(err) {
			return nil, fmt.Errorf("%w: kind %q", ErrConflict, id)
		}
		return nil, err
	}
	return &Kind{ID: id, DisplayName: displayName, CreatedAt: now}, nil
}

func (s *Store) GetKind(ctx context.Context, id string) (*Kind, error) {
	var k Kind
	err := s.db.QueryRowContext(ctx, `SELECT id, display_name, created_at FROM kinds WHERE id = ?`, id).
		Scan(&k.ID, &k.DisplayName, &k.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &k, nil
}

func (s *Store) GetKindByTokenHash(ctx context.Context, tokenHash string) (*Kind, error) {
	var k Kind
	err := s.db.QueryRowContext(ctx, `SELECT id, display_name, created_at FROM kinds WHERE token_hash = ?`, tokenHash).
		Scan(&k.ID, &k.DisplayName, &k.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &k, nil
}

func (s *Store) ListKinds(ctx context.Context) ([]Kind, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, display_name, created_at FROM kinds ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	kinds := []Kind{}
	for rows.Next() {
		var k Kind
		if err := rows.Scan(&k.ID, &k.DisplayName, &k.CreatedAt); err != nil {
			return nil, err
		}
		kinds = append(kinds, k)
	}
	return kinds, rows.Err()
}

func (s *Store) RotateKindToken(ctx context.Context, id, tokenHash string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE kinds SET token_hash = ? WHERE id = ?`, tokenHash, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- agent instances ----

func (s *Store) CreateAgent(ctx context.Context, name, displayName, kind, hostname, workdir string) (*Agent, error) {
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO agents (id, display_name, kind, hostname, workdir, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		name, displayName, kind, hostname, workdir, now); err != nil {
		if isUniqueErr(err) {
			return nil, fmt.Errorf("%w: agent %q", ErrConflict, name)
		}
		return nil, err
	}
	return &Agent{ID: name, DisplayName: displayName, Kind: kind, Hostname: hostname, Workdir: workdir, CreatedAt: now}, nil
}

const agentCols = `id, display_name, kind, hostname, workdir, created_at, last_seen_at`

func (s *Store) GetAgent(ctx context.Context, name string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id = ?`, name))
}

// GetAgentByTokenHash resolves an instance token to its agent.
func (s *Store) GetAgentByTokenHash(ctx context.Context, tokenHash string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT a.id, a.display_name, a.kind, a.hostname, a.workdir, a.created_at, a.last_seen_at
		FROM agents a JOIN agent_tokens t ON t.agent_id = a.id WHERE t.token_hash = ?`, tokenHash))
}

// FindInstance returns the agent registered for (kind, hostname, workdir), if any.
func (s *Store) FindInstance(ctx context.Context, kind, hostname, workdir string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE kind = ? AND hostname = ? AND workdir = ?`,
		kind, hostname, workdir))
}

// ReplaceAgentToken revokes every instance token of an agent and issues a
// single new one. Registering again with the same identity replaces the
// previous instance: its token stops authenticating on its next call. One
// live instance per identity; the last registration wins.
func (s *Store) ReplaceAgentToken(ctx context.Context, agentID, tokenHash string) error {
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_tokens WHERE agent_id = ?`, agentID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_tokens (token_hash, agent_id, created_at) VALUES (?, ?, ?)`,
		tokenHash, agentID, now); err != nil {
		if isFKErr(err) {
			return fmt.Errorf("%w: agent %q", ErrNotFound, agentID)
		}
		return err
	}
	return tx.Commit()
}

// RevokeAgentTokens invalidates all instance tokens of an agent.
func (s *Store) RevokeAgentTokens(ctx context.Context, agentID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM agent_tokens WHERE agent_id = ?`, agentID)
	return err
}

// SessionsForAgent lists the ids of the sessions the agent participates in.
func (s *Store) SessionsForAgent(ctx context.Context, agentID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id FROM participants WHERE agent_id = ? ORDER BY session_id`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sids := []string{}
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, err
		}
		sids = append(sids, sid)
	}
	return sids, rows.Err()
}

// UniqueAgentName derives a short but recognizable name from the agent kind,
// the hostname and the basename of the working directory, joined by ":" so
// the three parts stay visually distinct — e.g.
// "opencode:t3mcenter-arm:butler" — appending a counter suffix until it is
// unused. Parts are truncated (kind ≤16, hostname ≤20, dir ≤20, total ≤48)
// so the name stays readable even for long hosts and paths.
func (s *Store) UniqueAgentName(ctx context.Context, kind, hostname, workdir string) string {
	parts := []string{
		truncateSlug(sanitizeName(kind), 16),
		truncateSlug(sanitizeName(hostname), 20),
		truncateSlug(sanitizeName(path.Base(workdir)), 20),
	}
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	base := strings.Join(kept, ":")
	if base == "" {
		base = "agent"
	}
	if len(base) > 48 {
		base = strings.Trim(base[:48], "-:")
	}
	name := base
	for i := 2; ; i++ {
		if _, err := s.GetAgent(ctx, name); errors.Is(err, ErrNotFound) {
			return name
		}
		suffix := fmt.Sprintf("-%d", i)
		cand := base
		if len(cand) > 48-len(suffix) {
			cand = strings.Trim(cand[:48-len(suffix)], "-:")
		}
		name = cand + suffix
	}
}

func truncateSlug(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.Trim(s[:n], "-")
}

func sanitizeName(s string) string {
	var b strings.Builder
	lastDash := true // suppress leading dash
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func scanAgent(row *sql.Row) (*Agent, error) {
	var a Agent
	var lastSeen sql.NullInt64
	if err := row.Scan(&a.ID, &a.DisplayName, &a.Kind, &a.Hostname, &a.Workdir, &a.CreatedAt, &lastSeen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if lastSeen.Valid {
		v := lastSeen.Int64
		a.LastSeenAt = &v
	}
	return &a, nil
}

func (s *Store) queryAgents(ctx context.Context, q string, args ...any) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agents := []Agent{}
	for rows.Next() {
		var a Agent
		var lastSeen sql.NullInt64
		if err := rows.Scan(&a.ID, &a.DisplayName, &a.Kind, &a.Hostname, &a.Workdir, &a.CreatedAt, &lastSeen); err != nil {
			return nil, err
		}
		if lastSeen.Valid {
			v := lastSeen.Int64
			a.LastSeenAt = &v
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	return s.queryAgents(ctx, `SELECT `+agentCols+` FROM agents ORDER BY id`)
}

func (s *Store) TouchAgent(ctx context.Context, id string) {
	_, _ = s.db.ExecContext(ctx, `UPDATE agents SET last_seen_at = ? WHERE id = ?`, time.Now().Unix(), id)
}

func (s *Store) CreateSession(ctx context.Context, id, title, creatorID string, participants []string) (*Session, error) {
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, title, status, created_by, created_at) VALUES (?, ?, 'active', ?, ?)`,
		id, title, creatorID, now); err != nil {
		if isUniqueErr(err) {
			return nil, fmt.Errorf("%w: session %q", ErrConflict, id)
		}
		if isFKErr(err) {
			return nil, fmt.Errorf("%w: creator %q", ErrNotFound, creatorID)
		}
		return nil, err
	}
	for _, p := range append([]string{creatorID}, participants...) {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO participants (session_id, agent_id, joined_at) VALUES (?, ?, ?)`, id, p, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Session{ID: id, Title: title, Status: "active", CreatedBy: creatorID, CreatedAt: now}, nil
}

func (s *Store) GetSession(ctx context.Context, id string) (*Session, error) {
	var sess Session
	err := s.db.QueryRowContext(ctx, `SELECT id, title, status, created_by, created_at FROM sessions WHERE id = ?`, id).
		Scan(&sess.ID, &sess.Title, &sess.Status, &sess.CreatedBy, &sess.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &sess, nil
}

func (s *Store) ListSessions(ctx context.Context, joinedBy string) ([]Session, error) {
	q := `SELECT s.id, s.title, s.status, s.created_by, s.created_at FROM sessions s`
	var args []any
	if joinedBy != "" {
		q += ` WHERE s.id IN (SELECT session_id FROM participants WHERE agent_id = ?)`
		args = append(args, joinedBy)
	}
	q += ` ORDER BY s.created_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := []Session{}
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.Title, &sess.Status, &sess.CreatedBy, &sess.CreatedAt); err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

func (s *Store) SetSessionStatus(ctx context.Context, id, status string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) JoinSession(ctx context.Context, sessionID, agentID string) (bool, error) {
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO participants (session_id, agent_id, joined_at) VALUES (?, ?, ?)`, sessionID, agentID, now)
	if err != nil {
		if isFKErr(err) {
			return false, fmt.Errorf("%w: session %q or agent %q", ErrNotFound, sessionID, agentID)
		}
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages (id, session_id, sender_id, kind, content, created_at) VALUES (?, ?, ?, 'system', ?, ?)`,
		ids.New(), sessionID, agentID, agentID+" joined the session", now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) IsParticipant(ctx context.Context, sessionID, agentID string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM participants WHERE session_id = ? AND agent_id = ?`, sessionID, agentID).Scan(&one)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Store) Participants(ctx context.Context, sessionID string) ([]Agent, error) {
	return s.queryAgents(ctx, `SELECT `+agentCols+`
		FROM agents a JOIN participants p ON p.agent_id = a.id
		WHERE p.session_id = ? ORDER BY a.id`, sessionID)
}

func (s *Store) InsertMessage(ctx context.Context, m *Message) (*Message, error) {
	if m.ID == "" {
		m.ID = ids.New()
	}
	if m.CreatedAt == 0 {
		m.CreatedAt = time.Now().UnixMilli()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO messages (id, session_id, sender_id, kind, content, created_at) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		m.ID, m.SessionID, m.SenderID, m.Kind, m.Content, m.CreatedAt)
	if err != nil {
		if isFKErr(err) {
			return nil, fmt.Errorf("%w: session or agent referenced by message", ErrNotFound)
		}
		return nil, err
	}
	return s.GetMessage(ctx, m.ID)
}

func (s *Store) GetMessage(ctx context.Context, id string) (*Message, error) {
	var m Message
	err := s.db.QueryRowContext(ctx, `SELECT global_seq, id, session_id, sender_id, kind, content, created_at FROM messages WHERE id = ?`, id).
		Scan(&m.Seq, &m.ID, &m.SessionID, &m.SenderID, &m.Kind, &m.Content, &m.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &m, nil
}

func (s *Store) MessagesSince(ctx context.Context, agentID, sessionID string, after int64, limit int) ([]Message, error) {
	q := `SELECT global_seq, id, session_id, sender_id, kind, content, created_at FROM messages`
	var args []any
	if sessionID != "" {
		q += ` WHERE session_id = ? AND global_seq > ?`
		args = append(args, sessionID, after)
	} else {
		q += ` WHERE global_seq > ? AND session_id IN (SELECT session_id FROM participants WHERE agent_id = ?)`
		args = append(args, after, agentID)
	}
	q += ` ORDER BY global_seq LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	msgs := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.Seq, &m.ID, &m.SessionID, &m.SenderID, &m.Kind, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (s *Store) LatestSeq(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(global_seq), 0) FROM messages`).Scan(&n)
	return n, err
}

func (s *Store) SessionLatestMessages(ctx context.Context, sessionID string, n int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT global_seq, id, session_id, sender_id, kind, content, created_at
		FROM messages WHERE session_id = ? ORDER BY global_seq DESC LIMIT ?`, sessionID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	msgs := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.Seq, &m.ID, &m.SessionID, &m.SenderID, &m.Kind, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

func (s *Store) UpsertContext(ctx context.Context, sessionID, key, value, authorID string) (*ContextItem, int64, error) {
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO context_items (session_id, key, value, author_id, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (session_id, key) DO UPDATE SET value = excluded.value, author_id = excluded.author_id, updated_at = excluded.updated_at`,
		sessionID, key, value, authorID, now); err != nil {
		if isFKErr(err) {
			return nil, 0, fmt.Errorf("%w: session %q", ErrNotFound, sessionID)
		}
		return nil, 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages (id, session_id, sender_id, kind, content, created_at) VALUES (?, ?, ?, 'context', ?, ?)`,
		ids.New(), sessionID, authorID, "updated context: "+key, now); err != nil {
		return nil, 0, err
	}
	var notifySeq int64
	if err := tx.QueryRowContext(ctx, `SELECT last_insert_rowid()`).Scan(&notifySeq); err != nil {
		return nil, 0, err
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return &ContextItem{SessionID: sessionID, Key: key, Value: value, AuthorID: authorID, UpdatedAt: now}, notifySeq, nil
}

func (s *Store) GetContextItem(ctx context.Context, sessionID, key string) (*ContextItem, error) {
	var item ContextItem
	err := s.db.QueryRowContext(ctx, `SELECT session_id, key, value, author_id, updated_at FROM context_items WHERE session_id = ? AND key = ?`, sessionID, key).
		Scan(&item.SessionID, &item.Key, &item.Value, &item.AuthorID, &item.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &item, nil
}

func (s *Store) ListContext(ctx context.Context, sessionID string) ([]ContextItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, key, value, author_id, updated_at FROM context_items WHERE session_id = ? ORDER BY key`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ContextItem{}
	for rows.Next() {
		var item ContextItem
		if err := rows.Scan(&item.SessionID, &item.Key, &item.Value, &item.AuthorID, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
