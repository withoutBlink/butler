package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
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

type Agent struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Kind        string `json:"kind"`
	CreatedAt   int64  `json:"created_at"`
	LastSeenAt  *int64 `json:"last_seen_at,omitempty"`
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

func (s *Store) CreateAgent(ctx context.Context, id, displayName, kind, tokenHash string) (*Agent, error) {
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO agents (id, display_name, kind, token_hash, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, displayName, kind, tokenHash, now); err != nil {
		if isUniqueErr(err) {
			return nil, fmt.Errorf("%w: agent %q", ErrConflict, id)
		}
		return nil, err
	}
	return &Agent{ID: id, DisplayName: displayName, Kind: kind, CreatedAt: now}, nil
}

func (s *Store) GetAgent(ctx context.Context, id string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT id, display_name, kind, created_at, last_seen_at FROM agents WHERE id = ?`, id))
}

func (s *Store) GetAgentByTokenHash(ctx context.Context, tokenHash string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT id, display_name, kind, created_at, last_seen_at FROM agents WHERE token_hash = ?`, tokenHash))
}

func scanAgent(row *sql.Row) (*Agent, error) {
	var a Agent
	var lastSeen sql.NullInt64
	if err := row.Scan(&a.ID, &a.DisplayName, &a.Kind, &a.CreatedAt, &lastSeen); err != nil {
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

func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, display_name, kind, created_at, last_seen_at FROM agents ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agents := []Agent{}
	for rows.Next() {
		var a Agent
		var lastSeen sql.NullInt64
		if err := rows.Scan(&a.ID, &a.DisplayName, &a.Kind, &a.CreatedAt, &lastSeen); err != nil {
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

func (s *Store) RotateAgentToken(ctx context.Context, id, tokenHash string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE agents SET token_hash = ? WHERE id = ?`, tokenHash, id)
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
	rows, err := s.db.QueryContext(ctx, `SELECT a.id, a.display_name, a.kind, a.created_at, a.last_seen_at
		FROM agents a JOIN participants p ON p.agent_id = a.id
		WHERE p.session_id = ? ORDER BY a.id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agents := []Agent{}
	for rows.Next() {
		var a Agent
		var lastSeen sql.NullInt64
		if err := rows.Scan(&a.ID, &a.DisplayName, &a.Kind, &a.CreatedAt, &lastSeen); err != nil {
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

func (s *Store) UpsertContext(ctx context.Context, sessionID, key, value, authorID string) (*ContextItem, error) {
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO context_items (session_id, key, value, author_id, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (session_id, key) DO UPDATE SET value = excluded.value, author_id = excluded.author_id, updated_at = excluded.updated_at`,
		sessionID, key, value, authorID, now); err != nil {
		if isFKErr(err) {
			return nil, fmt.Errorf("%w: session %q", ErrNotFound, sessionID)
		}
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages (id, session_id, sender_id, kind, content, created_at) VALUES (?, ?, ?, 'context', ?, ?)`,
		ids.New(), sessionID, authorID, "updated context: "+key, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ContextItem{SessionID: sessionID, Key: key, Value: value, AuthorID: authorID, UpdatedAt: now}, nil
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
