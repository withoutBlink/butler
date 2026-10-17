package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"butler/internal/ids"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

type Store struct {
	db *sql.DB
}

// Agent is a registered agent instance: a (kind, hostname, workdir) triple
// identified by a server-generated name, plus an optional client_session
// discriminator that splits one app instance into per-client-session
// identities (e.g. one butler agent per OpenCode session). Multiple instance
// tokens may be active at once (one per registration).
type Agent struct {
	ID            string `json:"id"`
	DisplayName   string `json:"display_name"`
	Kind          string `json:"kind"`
	Hostname      string `json:"hostname"`
	Workdir       string `json:"workdir"`
	ClientSession string `json:"client_session,omitempty"`
	CreatedAt     int64  `json:"created_at"`
	LastSeenAt    *int64 `json:"last_seen_at,omitempty"`
	// RemovedAt is set when the agent is kicked (evicted): it keeps its row
	// — messages and sessions reference it — but listings, participants and
	// mentions skip it until it re-registers, which clears the flag.
	RemovedAt *int64 `json:"removed_at,omitempty"`
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
	Seq       int64    `json:"seq"`
	ID        string   `json:"id"`
	SessionID string   `json:"session_id"`
	SenderID  string   `json:"sender_id"`
	Kind      string   `json:"kind"`
	Content   string   `json:"content"`
	Mentions  []string `json:"mentions,omitempty"`
	CreatedAt int64    `json:"created_at"`
}

// MentionsAgent reports whether the message @-mentions the given agent.
// Mentions are resolved to agent ids at insert time (exact id, kind, or
// @all), so a plain id comparison suffices.
func (m Message) MentionsAgent(agentID string) bool {
	for _, id := range m.Mentions {
		if id == agentID {
			return true
		}
	}
	return false
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
	if err := s.ensureSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// ensureSchema applies schema.sql. There is no migration history: butler is
// pre-release and the schema file is the single source of truth. Every
// statement is CREATE ... IF NOT EXISTS, so opening an existing database is a
// no-op; a database from an older schema is NOT upgraded in place — recreate
// it (`butler reset`).
func (s *Store) ensureSchema() error {
	for _, stmt := range splitSQL(schemaSQL) {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
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

func (s *Store) CreateAgent(ctx context.Context, name, displayName, kind, hostname, workdir, clientSession string) (*Agent, error) {
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO agents (id, display_name, kind, hostname, workdir, client_session, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		name, displayName, kind, hostname, workdir, clientSession, now); err != nil {
		if isUniqueErr(err) {
			return nil, fmt.Errorf("%w: agent %q", ErrConflict, name)
		}
		return nil, err
	}
	return &Agent{ID: name, DisplayName: displayName, Kind: kind, Hostname: hostname, Workdir: workdir, ClientSession: clientSession, CreatedAt: now}, nil
}

const agentCols = `id, display_name, kind, hostname, workdir, client_session, created_at, last_seen_at, removed_at`

func (s *Store) GetAgent(ctx context.Context, name string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id = ?`, name))
}

// GetVisibleAgent resolves an agent by id with the same visibility rules as
// ListAgents and Participants: evicted (kicked) agents and hidden agent
// clients (app-level identities with live roles) do not resolve.
func (s *Store) GetVisibleAgent(ctx context.Context, id string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentCols+`
		FROM agents WHERE id = ? AND removed_at IS NULL
		AND NOT (`+clientHidden("agents")+`)`, id))
}

// GetAgentByTokenHash resolves an instance token to its agent. Removed
// (kicked) agents have no tokens, so they never resolve here.
func (s *Store) GetAgentByTokenHash(ctx context.Context, tokenHash string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT a.id, a.display_name, a.kind, a.hostname, a.workdir, a.client_session, a.created_at, a.last_seen_at, a.removed_at
		FROM agents a JOIN agent_tokens t ON t.agent_id = a.id WHERE t.token_hash = ?`, tokenHash))
}

// FindInstance returns the agent registered for (kind, hostname, workdir,
// clientSession). clientSession "" is the app-level identity; a non-empty
// discriminator selects the per-client-session sibling of that identity.
func (s *Store) FindInstance(ctx context.Context, kind, hostname, workdir, clientSession string) (*Agent, error) {
	return scanAgent(s.db.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE kind = ? AND hostname = ? AND workdir = ? AND client_session = ?`,
		kind, hostname, workdir, clientSession))
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

// UpdateAgentDisplayName refreshes an agent's display name (e.g. a changed
// client-session title re-registered by its owner). The id never changes.
func (s *Store) UpdateAgentDisplayName(ctx context.Context, agentID, displayName string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET display_name = ? WHERE id = ?`, displayName, agentID)
	return err
}

// RevokeAgentTokens invalidates all instance tokens of an agent.
func (s *Store) RevokeAgentTokens(ctx context.Context, agentID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM agent_tokens WHERE agent_id = ?`, agentID)
	return err
}

// RemoveAgent evicts an agent: its tokens are revoked and it is marked
// removed — hidden from listings, session participants and mention
// resolution. The row itself survives (messages and sessions reference it);
// re-registering the identity revives it (ClearAgentRemoved). Participants
// and read marks are left in place so a revive restores them untouched.
func (s *Store) RemoveAgent(ctx context.Context, agentID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE agents SET removed_at = ? WHERE id = ? AND removed_at IS NULL`, time.Now().Unix(), agentID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_tokens WHERE agent_id = ?`, agentID); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearAgentRemoved revives an evicted agent (called when the identity
// re-registers — a live process coming back).
func (s *Store) ClearAgentRemoved(ctx context.Context, agentID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET removed_at = NULL WHERE id = ?`, agentID)
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

func scanAgent(row *sql.Row) (*Agent, error) {
	var a Agent
	var lastSeen, removed sql.NullInt64
	if err := row.Scan(&a.ID, &a.DisplayName, &a.Kind, &a.Hostname, &a.Workdir, &a.ClientSession, &a.CreatedAt, &lastSeen, &removed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if lastSeen.Valid {
		v := lastSeen.Int64
		a.LastSeenAt = &v
	}
	if removed.Valid {
		v := removed.Int64
		a.RemovedAt = &v
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
		var lastSeen, removed sql.NullInt64
		if err := rows.Scan(&a.ID, &a.DisplayName, &a.Kind, &a.Hostname, &a.Workdir, &a.ClientSession, &a.CreatedAt, &lastSeen, &removed); err != nil {
			return nil, err
		}
		if lastSeen.Valid {
			v := lastSeen.Int64
			a.LastSeenAt = &v
		}
		if removed.Valid {
			v := removed.Int64
			a.RemovedAt = &v
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// clientHidden matches app-level identities ("agent clients",
// client_session empty) that have at least one live role: once the client is
// represented by its per-session roles it becomes backend plumbing — the
// panel, participants and mention resolution skip it, while its token,
// wait loop and registration path keep working (auth and delivery queries
// never filter on this).
func clientHidden(alias string) string {
	return alias + `.client_session = '' AND EXISTS (
	SELECT 1 FROM agents r
	WHERE r.kind = ` + alias + `.kind AND r.hostname = ` + alias + `.hostname
	  AND r.workdir = ` + alias + `.workdir AND r.client_session <> ''
	  AND r.removed_at IS NULL)`
}

// ListAgents returns the visible (non-removed, non-hidden-client) agents,
// ordered by display name (case-insensitive) so panel lists stay alphabetical
// — ids are UUIDs now, so id order would look random.
// Kicked agents stay hidden until they re-register; agent clients with live
// roles are hidden — the roles represent them.
func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	return s.queryAgents(ctx, `SELECT `+agentCols+`
		FROM agents
		WHERE removed_at IS NULL AND NOT (`+clientHidden("agents")+`)
		ORDER BY display_name COLLATE NOCASE, id`)
}

func (s *Store) TouchAgent(ctx context.Context, id string) {
	_, _ = s.db.ExecContext(ctx, `UPDATE agents SET last_seen_at = ? WHERE id = ?`, time.Now().Unix(), id)
}

// GlobalSessionID is the fixed id of the default session every agent joins
// automatically: created at server bootstrap, joined on registration.
const GlobalSessionID = "global"

// GlobalSessionTitle is the display title of the global session.
const GlobalSessionTitle = "Global"

// EnsureGlobalSession creates the global session (the default room shared by
// every agent) if it does not exist yet and adds creatorID — normally the
// panel's "admin" agent — as its first participant. Idempotent: safe to call at
// every server start. Returns whether the session was newly created.
func (s *Store) EnsureGlobalSession(ctx context.Context, creatorID string) (bool, error) {
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	created := false
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO sessions (id, title, status, created_by, created_at) VALUES (?, ?, 'active', ?, ?)`,
		GlobalSessionID, GlobalSessionTitle, creatorID, now)
	if err != nil {
		if isFKErr(err) {
			return false, fmt.Errorf("%w: creator %q", ErrNotFound, creatorID)
		}
		return false, err
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		created = true
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO participants (session_id, agent_id, joined_at) VALUES (?, ?, ?)`,
		GlobalSessionID, creatorID, now); err != nil {
		if isFKErr(err) {
			return false, fmt.Errorf("%w: creator %q", ErrNotFound, creatorID)
		}
		return false, err
	}
	if created {
		// Messages carry millisecond timestamps (the panel formats them with
		// UnixMilli); sessions/participants above keep second granularity.
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages (id, session_id, sender_id, kind, content, created_at) VALUES (?, ?, ?, 'system', ?, ?)`,
			ids.New(), GlobalSessionID, creatorID, creatorID+" created the global session — every agent joins it automatically", time.Now().UnixMilli()); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return created, nil
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
	if status != "active" && status != "archived" {
		return fmt.Errorf("%w: status must be active or archived", ErrConflict)
	}
	// The global session is the fixed default room: every agent joins it, and
	// archiving it would hide the one session that is always there.
	if id == GlobalSessionID {
		return fmt.Errorf("%w: the global session is fixed", ErrConflict)
	}
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

// DeleteSession removes a session and everything scoped to it (messages,
// participants, context items, read cursors) in one transaction, returning
// the ids of the participants it had — the caller wakes their wait loops (an
// open session simply stops appearing in their next batch). Deletion is
// permanent: unlike a kick there is no soft-remove row, and message history
// goes with it. The global session is refused for the same reason it cannot
// be archived.
func (s *Store) DeleteSession(ctx context.Context, id string) ([]string, error) {
	if id == GlobalSessionID {
		return nil, fmt.Errorf("%w: the global session is fixed", ErrConflict)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT agent_id FROM participants WHERE session_id = ?`, id)
	if err != nil {
		return nil, err
	}
	participants := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return nil, err
		}
		participants = append(participants, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	// messages.session_id carries no cascade (it is history) and read_marks
	// has no FK at all, so both are deleted explicitly; participants and
	// context_items cascade.
	for _, stmt := range []string{
		`DELETE FROM messages WHERE session_id = ?`,
		`DELETE FROM read_marks WHERE session_id = ?`,
		`DELETE FROM participants WHERE session_id = ?`,
		`DELETE FROM context_items WHERE session_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return nil, err
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("%w: session %q", ErrNotFound, id)
	}
	return participants, tx.Commit()
}

func (s *Store) JoinSession(ctx context.Context, sessionID, agentID string) (bool, error) {
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// Only an active session accepts participants: an archived room is frozen
	// — its history stays readable, but it gains no members until it is
	// reopened. Enforced here (not just in the panel) so every path — the API,
	// the panel picker, sibling registration's auto-join, the CLI, tests —
	// obeys the same rule.
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM sessions WHERE id = ?`, sessionID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("%w: session %q", ErrNotFound, sessionID)
		}
		return false, err
	}
	if status != "active" {
		return false, fmt.Errorf("%w: session %q is %s", ErrConflict, sessionID, status)
	}
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
	// The notice names the agent the way humans see it — its display title;
	// untitled identities (app-level ones are labeled with their own id by
	// design) fall back to the id. Resolved at insert time, exactly like
	// mentions: a later rename does not rewrite a stored notice.
	label := noticeLabelTx(ctx, tx, agentID)
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages (id, session_id, sender_id, kind, content, created_at) VALUES (?, ?, ?, 'system', ?, ?)`,
		ids.New(), sessionID, agentID, label+" joined the session", now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// RemoveParticipant drops one agent from a session: the panel's participants
// list, or an agent leaving by itself. It is the mirror of JoinSession,
// including its rule — an archived session is frozen and refuses membership
// changes either way (ErrConflict; reopen first). The participant row, its
// read cursor (a non-participant has none, so a re-join starts unread) and a
// system notice naming the actor ("X left the session" / "X was removed from
// the session by admin") change in one transaction. The agent itself is
// untouched: still registered, still in its other sessions, re-addable with
// the picker. ErrNotFound means the session or the membership is not there.
func (s *Store) RemoveParticipant(ctx context.Context, sessionID, agentID, actorID string) error {
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM sessions WHERE id = ?`, sessionID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: session %q", ErrNotFound, sessionID)
		}
		return err
	}
	if status != "active" {
		return fmt.Errorf("%w: session %q is %s", ErrConflict, sessionID, status)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM participants WHERE session_id = ? AND agent_id = ?`, sessionID, agentID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %q is not a participant of %q", ErrNotFound, agentID, sessionID)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM read_marks WHERE session_id = ? AND agent_id = ?`, sessionID, agentID); err != nil {
		return err
	}
	who := noticeLabelTx(ctx, tx, agentID)
	content := who + " left the session"
	if actorID != "" && actorID != agentID {
		content = who + " was removed from the session by " + noticeLabelTx(ctx, tx, actorID)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages (id, session_id, sender_id, kind, content, created_at) VALUES (?, ?, ?, 'system', ?, ?)`,
		ids.New(), sessionID, agentID, content, now); err != nil {
		return err
	}
	return tx.Commit()
}

// noticeLabelTx resolves how a server-generated notice names an agent inside a
// write transaction: its display title, or the id for an untitled identity
// (app-level identities are labeled with their own id by design, so for them
// both forms are the same string).
func noticeLabelTx(ctx context.Context, tx *sql.Tx, agentID string) string {
	label := agentID
	var display string
	if err := tx.QueryRowContext(ctx, `SELECT display_name FROM agents WHERE id = ?`, agentID).Scan(&display); err == nil && display != "" {
		label = display
	}
	return label
}

// NoticeLabel is how server-generated notices (join, eviction, replacement)
// name an agent: its display title, or the id for an untitled identity.
// App-level identities carry their own id as display name by design, so for
// them this is the same string either way.
func NoticeLabel(a *Agent) string {
	if a == nil {
		return ""
	}
	if a.DisplayName != "" {
		return a.DisplayName
	}
	return a.ID
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

// Participants lists a session's visible participants ordered by display name
// (case-insensitive) — what the panel's participants sidebar renders.
func (s *Store) Participants(ctx context.Context, sessionID string) ([]Agent, error) {
	return s.queryAgents(ctx, `SELECT `+agentCols+`
		FROM agents a JOIN participants p ON p.agent_id = a.id
		WHERE p.session_id = ? AND a.removed_at IS NULL
		  AND NOT (`+clientHidden("a")+`)
		ORDER BY a.display_name COLLATE NOCASE, a.id`, sessionID)
}

// AgentsForIdentity returns every agent row of one agent client's identity
// key (kind, hostname, workdir): the client itself plus all its roles,
// removed ones included — the caller decides what to show or re-register.
func (s *Store) AgentsForIdentity(ctx context.Context, kind, hostname, workdir string) ([]Agent, error) {
	return s.queryAgents(ctx, `SELECT `+agentCols+`
		FROM agents
		WHERE kind = ? AND hostname = ? AND workdir = ?
		ORDER BY id`, kind, hostname, workdir)
}

// InsertMessage stores a chat message. @mentions in the content are resolved
// against the session's participants (exact agent id, agent kind, or
// @all/@everyone) and stored with the message, so consumers can tell "this
// message addresses me" (reply expected) from FYI. Message ids are
// client-supplied and idempotent: re-inserting the same id is a no-op that
// returns the stored message.
func (s *Store) InsertMessage(ctx context.Context, m *Message) (*Message, error) {
	if m.ID == "" {
		m.ID = ids.New()
	}
	if m.CreatedAt == 0 {
		m.CreatedAt = time.Now().UnixMilli()
	}
	if m.Mentions == nil {
		m.Mentions = s.resolveMentions(ctx, m.SessionID, m.SenderID, m.Content)
	}
	mentionsJSON := "[]"
	if len(m.Mentions) > 0 {
		b, err := json.Marshal(m.Mentions)
		if err != nil {
			return nil, err
		}
		mentionsJSON = string(b)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO messages (id, session_id, sender_id, kind, content, mentions, created_at) VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		m.ID, m.SessionID, m.SenderID, m.Kind, m.Content, mentionsJSON, m.CreatedAt)
	if err != nil {
		if isFKErr(err) {
			return nil, fmt.Errorf("%w: session or agent referenced by message", ErrNotFound)
		}
		return nil, err
	}
	return s.GetMessage(ctx, m.ID)
}

// resolveMentions maps the @tokens in content to the participant agents they
// address: "@<agent id>" (exact), "@<kind>" (every participant of that kind),
// "@<display name>" (every participant labeled with that name — the readable
// form the panel's autocomplete inserts) and "@all"/"@everyone"/"@here" (all
// participants). Names match case-insensitively and may contain spaces; at an
// "@" the longest matching name wins, so "Beta 2" does not also trigger
// "Beta". The author is never mentioned by its own message — mentions mean
// "agents that should reply", and a message is not addressed to its writer
// (a role quoting its own title or id, or any "@all" post, would otherwise
// self-resolve and show up in its own wait output as "(mentioned you)").
// A backslash-escaped "\@" is literal, so a message can quote the syntax
// ("say \@all to address the room") without addressing anyone. Tokens that
// match no participant resolve to nothing — mentioning only reaches agents
// already in the session.
func (s *Store) resolveMentions(ctx context.Context, sessionID, senderID, content string) []string {
	tokens := mentionTokens(content)
	participants, err := s.Participants(ctx, sessionID)
	if err != nil {
		// Fail open: store the message without mentions rather than
		// dropping it.
		return nil
	}
	var mentions []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && id != senderID && !seen[id] {
			seen[id] = true
			mentions = append(mentions, id)
		}
	}
	for _, tok := range tokens {
		switch tok {
		case "all", "everyone", "here":
			for _, p := range participants {
				add(p.ID)
			}
		default:
			for _, p := range participants {
				if p.ID == tok || p.Kind == tok {
					add(p.ID)
				}
			}
		}
	}
	for _, name := range mentionedNames(content, participants) {
		for _, p := range participants {
			if strings.EqualFold(p.DisplayName, name) {
				add(p.ID)
			}
		}
	}
	return mentions
}

// mentionedNames returns the participant display names mentioned as
// "@<name>" in content: case-insensitive, spaces allowed. At each "@" the
// longest matching name wins, so a name that merely prefixes a longer one
// ("Beta" vs "Beta 2") does not fire on the longer name's mention. Untitled
// identities (display name == id) are excluded — their id path already
// covers them.
func mentionedNames(content string, participants []Agent) []string {
	lower := strings.ToLower(content)
	var names []string
	seen := map[string]bool{}
	for i := 0; i < len(lower); i++ {
		if lower[i] != '@' {
			continue
		}
		// The "@" must open a token: not mid-word (so a@b is no mention)
		// and not backslash-escaped ("\@all" is prose about mentions).
		if i > 0 && (isMentionChar(lower[i-1]) || isEscapedAt(lower, i)) {
			continue
		}
		rest := lower[i+1:]
		best := ""
		for _, p := range participants {
			name := strings.ToLower(p.DisplayName)
			if name == "" || strings.EqualFold(name, p.ID) || len(name) <= len(best) {
				continue
			}
			if !strings.HasPrefix(rest, name) {
				continue
			}
			if after := rest[len(name):]; after != "" && isMentionChar(after[0]) {
				continue // part of a longer word ("@Alpha2")
			}
			best = name
		}
		if best != "" && !seen[best] {
			seen[best] = true
			names = append(names, best)
		}
	}
	return names
}

// mentionTokens extracts the distinct @tokens in content: an "@" followed by
// agent-id characters (letters, digits, ":", "_", "-"). Emails and other @
// uses only match when they happen to equal a participant id or kind. A
// backslash-escaped "\@" is literal — it lets a message discuss the mention
// syntax itself ("quote \@all in a doc") without addressing the room.
func mentionTokens(content string) []string {
	var tokens []string
	seen := map[string]bool{}
	for i := 0; i < len(content); i++ {
		if content[i] != '@' {
			continue
		}
		if isEscapedAt(content, i) {
			continue
		}
		j := i + 1
		for j < len(content) && isMentionChar(content[j]) {
			j++
		}
		if j > i+1 {
			tok := content[i+1 : j]
			if !seen[tok] {
				seen[tok] = true
				tokens = append(tokens, tok)
			}
			i = j - 1
		}
	}
	return tokens
}

func isMentionChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == ':' || c == '-'
}

// isEscapedAt reports whether the "@" at index i is backslash-escaped
// ("\@"): an even number of preceding backslashes escapes nothing, an odd
// number escapes the "@" — so "\\@" is a literal backslash followed by a
// real mention.
func isEscapedAt(content string, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && content[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}

// scanMentions decodes the mentions column; undecodable values (never
// produced by this code) read as no mentions.
func scanMentions(raw string) []string {
	if raw == "" || raw == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func (s *Store) GetMessage(ctx context.Context, id string) (*Message, error) {
	var m Message
	var mentionsJSON string
	err := s.db.QueryRowContext(ctx, `SELECT global_seq, id, session_id, sender_id, kind, content, mentions, created_at FROM messages WHERE id = ?`, id).
		Scan(&m.Seq, &m.ID, &m.SessionID, &m.SenderID, &m.Kind, &m.Content, &mentionsJSON, &m.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	m.Mentions = scanMentions(mentionsJSON)
	return &m, nil
}

func (s *Store) MessagesSince(ctx context.Context, agentID, sessionID string, after int64, limit int) ([]Message, error) {
	q := `SELECT global_seq, id, session_id, sender_id, kind, content, mentions, created_at FROM messages`
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
		var mentionsJSON string
		if err := rows.Scan(&m.Seq, &m.ID, &m.SessionID, &m.SenderID, &m.Kind, &m.Content, &mentionsJSON, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.Mentions = scanMentions(mentionsJSON)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (s *Store) LatestSeq(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(global_seq), 0) FROM messages`).Scan(&n)
	return n, err
}

// MarkRead advances the per-session read cursor for an agent: the message
// seq up to which the agent has received the session's messages (a wait
// long-poll batch, a message fetch, or the agent's own post). Cursors are
// monotonic — a lower seq never moves one back. The panel renders "already
// read" marks from these cursors.
func (s *Store) MarkRead(ctx context.Context, sessionID, agentID string, seq int64) error {
	if seq <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO read_marks (session_id, agent_id, last_seq) VALUES (?, ?, ?)
		ON CONFLICT (session_id, agent_id) DO UPDATE SET last_seq = MAX(last_seq, excluded.last_seq)`,
		sessionID, agentID, seq)
	return err
}

// ReadMarks returns the per-agent read cursors for a session: a map of agent
// id to the highest seq that agent has received. Agents absent from the map
// have read nothing in the session.
func (s *Store) ReadMarks(ctx context.Context, sessionID string) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT agent_id, last_seq FROM read_marks WHERE session_id = ?`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	marks := map[string]int64{}
	for rows.Next() {
		var id string
		var seq int64
		if err := rows.Scan(&id, &seq); err != nil {
			return nil, err
		}
		marks[id] = seq
	}
	return marks, rows.Err()
}

func (s *Store) SessionLatestMessages(ctx context.Context, sessionID string, n int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT global_seq, id, session_id, sender_id, kind, content, mentions, created_at
		FROM messages WHERE session_id = ? ORDER BY global_seq DESC LIMIT ?`, sessionID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	msgs := []Message{}
	for rows.Next() {
		var m Message
		var mentionsJSON string
		if err := rows.Scan(&m.Seq, &m.ID, &m.SessionID, &m.SenderID, &m.Kind, &m.Content, &mentionsJSON, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.Mentions = scanMentions(mentionsJSON)
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
