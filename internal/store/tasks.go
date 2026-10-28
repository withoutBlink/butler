package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"butler/internal/ids"
)

// Task statuses, in the order the board renders them.
const (
	TaskTodo       = "todo"
	TaskInProgress = "in_progress"
	TaskBlocked    = "blocked"
	TaskReview     = "review"
	TaskDone       = "done"
)

// Task priorities. Normal is the default.
const (
	PriorityLow    = "low"
	PriorityNormal = "normal"
	PriorityHigh   = "high"
	PriorityUrgent = "urgent"
)

// TaskStatuses is the legal status set, board column order.
var TaskStatuses = []string{TaskTodo, TaskInProgress, TaskBlocked, TaskReview, TaskDone}

// TaskPriorities is the legal priority set, most urgent last in the list
// (the board sorts the other way: urgent first).
var TaskPriorities = []string{PriorityLow, PriorityNormal, PriorityHigh, PriorityUrgent}

// ValidTaskStatus reports whether s is a board column.
func ValidTaskStatus(s string) bool {
	for _, v := range TaskStatuses {
		if s == v {
			return true
		}
	}
	return false
}

// ValidTaskPriority reports whether s is a known priority.
func ValidTaskPriority(s string) bool {
	for _, v := range TaskPriorities {
		if s == v {
			return true
		}
	}
	return false
}

// Task is one card on the board. AssigneeID and SessionID are empty when
// unset (stored as NULL). Times are unix milliseconds, same as messages.
type Task struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Priority    string `json:"priority"`
	AssigneeID  string `json:"assignee_id"`
	CreatorID   string `json:"creator_id"`
	SessionID   string `json:"session_id"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

// TaskComment is one entry in a task's progress log.
type TaskComment struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	AuthorID  string `json:"author_id"`
	Body      string `json:"body"`
	CreatedAt int64  `json:"created_at"`
}

// TaskFilter selects tasks. Empty fields are ignored. Unassigned restricts
// to tasks with no assignee and ignores AssigneeID.
type TaskFilter struct {
	Status     string
	AssigneeID string
	Unassigned bool
	SessionID  string
	CreatorID  string
}

// TaskPatch applies a partial update. A nil pointer leaves the field alone.
// A pointer to "" clears assignee_id or session_id.
type TaskPatch struct {
	Title       *string
	Description *string
	Status      *string
	Priority    *string
	AssigneeID  *string
	SessionID   *string
}

// Webhook is an outbound URL for one agent or one kind.
type Webhook struct {
	ID        string `json:"id"`
	AgentID   string `json:"agent_id,omitempty"`
	Kind      string `json:"kind,omitempty"`
	URL       string `json:"url"`
	CreatedAt int64  `json:"created_at"`
}

const taskCols = `id, title, description, status, priority, COALESCE(assignee_id, ''), creator_id, COALESCE(session_id, ''), created_at, updated_at`

func scanTask(row interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.Title, &t.Description, &t.Status, &t.Priority, &t.AssigneeID, &t.CreatorID, &t.SessionID, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &t, nil
}

// CreateTask inserts a task. An empty ID is filled with a UUID. An empty
// status or priority gets the default. Assignee and session are stored as
// NULL when empty.
func (s *Store) CreateTask(ctx context.Context, t *Task) (*Task, error) {
	if t.ID == "" {
		t.ID = ids.UUID()
	}
	now := time.Now().UnixMilli()
	if t.CreatedAt == 0 {
		t.CreatedAt = now
	}
	if t.UpdatedAt == 0 {
		t.UpdatedAt = t.CreatedAt
	}
	if t.Status == "" {
		t.Status = TaskTodo
	}
	if t.Priority == "" {
		t.Priority = PriorityNormal
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tasks (id, title, description, status, priority, assignee_id, creator_id, session_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Title, t.Description, t.Status, t.Priority, nullStr(t.AssigneeID), t.CreatorID, nullStr(t.SessionID), t.CreatedAt, t.UpdatedAt)
	if err != nil {
		if isUniqueErr(err) {
			return nil, fmt.Errorf("%w: task %q", ErrConflict, t.ID)
		}
		if isFKErr(err) {
			return nil, fmt.Errorf("%w: task references a missing agent or session", ErrNotFound)
		}
		return nil, err
	}
	return s.GetTask(ctx, t.ID)
}

func (s *Store) GetTask(ctx context.Context, id string) (*Task, error) {
	return scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, id))
}

// ListTasks returns tasks matching f, ordered by column, then priority
// (urgent first), then most recently updated.
func (s *Store) ListTasks(ctx context.Context, f TaskFilter) ([]Task, error) {
	q := `SELECT ` + taskCols + ` FROM tasks WHERE 1=1`
	var args []any
	if f.Status != "" {
		q += ` AND status = ?`
		args = append(args, f.Status)
	}
	if f.Unassigned {
		q += ` AND assignee_id IS NULL`
	} else if f.AssigneeID != "" {
		q += ` AND assignee_id = ?`
		args = append(args, f.AssigneeID)
	}
	if f.SessionID != "" {
		q += ` AND session_id = ?`
		args = append(args, f.SessionID)
	}
	if f.CreatorID != "" {
		q += ` AND creator_id = ?`
		args = append(args, f.CreatorID)
	}
	q += ` ORDER BY CASE status
		WHEN 'todo' THEN 0 WHEN 'in_progress' THEN 1 WHEN 'blocked' THEN 2
		WHEN 'review' THEN 3 WHEN 'done' THEN 4 ELSE 5 END,
		CASE priority WHEN 'urgent' THEN 0 WHEN 'high' THEN 1 WHEN 'normal' THEN 2 WHEN 'low' THEN 3 ELSE 4 END,
		updated_at DESC, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// UpdateTask applies p to an existing task and bumps updated_at.
func (s *Store) UpdateTask(ctx context.Context, id string, p TaskPatch) (*Task, error) {
	cur, err := s.GetTask(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.Title != nil {
		cur.Title = *p.Title
	}
	if p.Description != nil {
		cur.Description = *p.Description
	}
	if p.Status != nil {
		cur.Status = *p.Status
	}
	if p.Priority != nil {
		cur.Priority = *p.Priority
	}
	if p.AssigneeID != nil {
		cur.AssigneeID = *p.AssigneeID
	}
	if p.SessionID != nil {
		cur.SessionID = *p.SessionID
	}
	cur.UpdatedAt = time.Now().UnixMilli()
	_, err = s.db.ExecContext(ctx, `UPDATE tasks SET title = ?, description = ?, status = ?, priority = ?, assignee_id = ?, session_id = ?, updated_at = ? WHERE id = ?`,
		cur.Title, cur.Description, cur.Status, cur.Priority, nullStr(cur.AssigneeID), nullStr(cur.SessionID), cur.UpdatedAt, id)
	if err != nil {
		if isFKErr(err) {
			return nil, fmt.Errorf("%w: task references a missing agent or session", ErrNotFound)
		}
		return nil, err
	}
	return s.GetTask(ctx, id)
}

// AddTaskComment appends a progress entry and bumps the task's updated_at
// so the board reorders it. The task must exist.
func (s *Store) AddTaskComment(ctx context.Context, c *TaskComment) (*TaskComment, error) {
	if c.ID == "" {
		c.ID = ids.UUID()
	}
	if c.CreatedAt == 0 {
		c.CreatedAt = time.Now().UnixMilli()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE id = ?`, c.CreatedAt, c.TaskID)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("%w: task %q", ErrNotFound, c.TaskID)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_comments (id, task_id, author_id, body, created_at) VALUES (?, ?, ?, ?, ?)`,
		c.ID, c.TaskID, c.AuthorID, c.Body, c.CreatedAt); err != nil {
		if isFKErr(err) {
			return nil, fmt.Errorf("%w: comment author", ErrNotFound)
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return c, nil
}

// ListTaskComments returns the progress log oldest first.
func (s *Store) ListTaskComments(ctx context.Context, taskID string) ([]TaskComment, error) {
	// rowid breaks ties when two comments land in the same millisecond,
	// which is the common case for a burst of notes in one process.
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_id, author_id, body, created_at FROM task_comments WHERE task_id = ? ORDER BY created_at, rowid`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskComment{}
	for rows.Next() {
		var c TaskComment
		if err := rows.Scan(&c.ID, &c.TaskID, &c.AuthorID, &c.Body, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ResolveAssignee maps a mention-style token to exactly one visible agent.
// The token may carry a leading "@". Resolution order matches @mentions:
// exact id, then exact display name (case-insensitive, spaces allowed),
// then kind. A kind or a name that matches more than one agent is
// ErrConflict — an assignee is one agent, so the caller has to be specific.
// Hidden agent clients and evicted agents are not candidates (same
// visibility as ListAgents).
func (s *Store) ResolveAssignee(ctx context.Context, token string) (*Agent, error) {
	token = strings.TrimSpace(token)
	token = strings.TrimPrefix(token, "@")
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("%w: empty assignee", ErrNotFound)
	}
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	for i := range agents {
		if agents[i].ID == token {
			return &agents[i], nil
		}
	}
	var byName []Agent
	for _, a := range agents {
		if strings.EqualFold(a.DisplayName, token) {
			byName = append(byName, a)
		}
	}
	switch len(byName) {
	case 1:
		return &byName[0], nil
	case 0:
	default:
		return nil, fmt.Errorf("%w: assignee %q matches %d agents by name; use an id", ErrConflict, token, len(byName))
	}
	var byKind []Agent
	for _, a := range agents {
		if a.Kind == token {
			byKind = append(byKind, a)
		}
	}
	switch len(byKind) {
	case 1:
		return &byKind[0], nil
	case 0:
		return nil, fmt.Errorf("%w: assignee %q", ErrNotFound, token)
	default:
		return nil, fmt.Errorf("%w: assignee %q matches %d agents of that kind; use an id or a display name", ErrConflict, token, len(byKind))
	}
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// SetAgentWebhook stores the outbound URL for one agent, replacing any
// previous URL for that agent.
func (s *Store) SetAgentWebhook(ctx context.Context, agentID, url string) (*Webhook, error) {
	if _, err := s.GetAgent(ctx, agentID); err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	id := ids.UUID()
	_, err := s.db.ExecContext(ctx, `INSERT INTO webhooks (id, agent_id, url, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET url = excluded.url, created_at = excluded.created_at`,
		id, agentID, url, now)
	if err != nil {
		return nil, err
	}
	return s.webhookByAgent(ctx, agentID)
}

// SetKindWebhook stores the outbound URL for every agent of a kind that
// does not have its own URL.
func (s *Store) SetKindWebhook(ctx context.Context, kind, url string) (*Webhook, error) {
	if _, err := s.GetKind(ctx, kind); err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, `INSERT INTO webhooks (id, kind, url, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(kind) DO UPDATE SET url = excluded.url, created_at = excluded.created_at`,
		ids.UUID(), kind, url, now)
	if err != nil {
		return nil, err
	}
	return s.webhookByKind(ctx, kind)
}

func (s *Store) DeleteAgentWebhook(ctx context.Context, agentID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM webhooks WHERE agent_id = ?`, agentID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: webhook for agent %q", ErrNotFound, agentID)
	}
	return nil
}

func (s *Store) DeleteKindWebhook(ctx context.Context, kind string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM webhooks WHERE kind = ?`, kind)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: webhook for kind %q", ErrNotFound, kind)
	}
	return nil
}

// ListWebhooks returns every configured URL, agent rows first.
func (s *Store) ListWebhooks(ctx context.Context) ([]Webhook, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(agent_id, ''), COALESCE(kind, ''), url, created_at FROM webhooks ORDER BY kind, agent_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Webhook{}
	for rows.Next() {
		var w Webhook
		if err := rows.Scan(&w.ID, &w.AgentID, &w.Kind, &w.URL, &w.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) webhookByAgent(ctx context.Context, agentID string) (*Webhook, error) {
	var w Webhook
	err := s.db.QueryRowContext(ctx, `SELECT id, COALESCE(agent_id, ''), COALESCE(kind, ''), url, created_at FROM webhooks WHERE agent_id = ?`, agentID).
		Scan(&w.ID, &w.AgentID, &w.Kind, &w.URL, &w.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &w, nil
}

func (s *Store) webhookByKind(ctx context.Context, kind string) (*Webhook, error) {
	var w Webhook
	err := s.db.QueryRowContext(ctx, `SELECT id, COALESCE(agent_id, ''), COALESCE(kind, ''), url, created_at FROM webhooks WHERE kind = ?`, kind).
		Scan(&w.ID, &w.AgentID, &w.Kind, &w.URL, &w.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &w, nil
}

// WebhookURL returns the URL to POST for this agent: the agent's own URL
// if one is set, otherwise the URL for its kind. An empty string means
// nothing is configured (not an error).
func (s *Store) WebhookURL(ctx context.Context, agentID string) (string, error) {
	if w, err := s.webhookByAgent(ctx, agentID); err == nil {
		return w.URL, nil
	} else if !errors.Is(err, ErrNotFound) {
		return "", err
	}
	var kind string
	err := s.db.QueryRowContext(ctx, `SELECT kind FROM agents WHERE id = ?`, agentID).Scan(&kind)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	w, err := s.webhookByKind(ctx, kind)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return w.URL, nil
}
