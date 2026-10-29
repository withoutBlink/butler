package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrateTaskColumns opens a database created with the task-board schema
// from before handoff columns existed and checks that Open adds them without
// disturbing the row. A second Open must be a no-op.
func TestMigrateTaskColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "butler.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	old := `
CREATE TABLE agents (
    id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    kind TEXT NOT NULL,
    hostname TEXT NOT NULL DEFAULT '',
    workdir TEXT NOT NULL DEFAULT '',
    client_session TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
CREATE TABLE tasks (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'todo',
    priority TEXT NOT NULL DEFAULT 'normal',
    assignee_id TEXT REFERENCES agents (id),
    creator_id TEXT NOT NULL REFERENCES agents (id),
    session_id TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);`
	if _, err := db.Exec(old); err != nil {
		t.Fatalf("old schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO agents (id, display_name, kind, created_at) VALUES ('ada', 'Ada', 'test', 1)`); err != nil {
		t.Fatalf("agent: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO tasks (id, title, description, status, priority, assignee_id, creator_id, created_at, updated_at) VALUES ('t1', 'Keep me', 'body', 'in_progress', 'high', 'ada', 'ada', 10, 11)`); err != nil {
		t.Fatalf("task: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("migrate open: %v", err)
	}
	got, err := s.GetTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "Keep me" || got.Status != TaskInProgress || got.AssigneeID != "ada" || got.Priority != PriorityHigh || got.Description != "body" {
		t.Fatalf("row changed by migration: %+v", got)
	}
	if got.ReviewerID != "" || got.ReviewFromID != "" || len(got.Chain) != 0 {
		t.Fatalf("new columns should default empty: %+v", got)
	}
	cols, err := s.tableColumns("tasks")
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	for _, name := range []string{"reviewer_id", "handoff_chain", "review_from_id"} {
		if !cols[name] {
			t.Fatalf("missing column %s after migrate: %v", name, cols)
		}
	}
	chain := []string{"ada"}
	if _, err := s.UpdateTask(context.Background(), "t1", TaskPatch{Chain: &chain}); err != nil {
		t.Fatalf("update chain: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s2.Close()
	got, err = s2.GetTask(context.Background(), "t1")
	if err != nil {
		t.Fatalf("get after second open: %v", err)
	}
	if len(got.Chain) != 1 || got.Chain[0] != "ada" || got.Title != "Keep me" {
		t.Fatalf("chain did not survive the second open: %+v", got)
	}
}
