package store

import (
	"context"
	"errors"
	"testing"
)

func TestTaskCRUDAndOrder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	mustAgent(t, s, "ada")
	mustAgent(t, s, "gus")

	low, err := s.CreateTask(ctx, &Task{Title: "later", Status: TaskTodo, Priority: PriorityLow, CreatorID: "ada"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if low.Status != TaskTodo || low.Priority != PriorityLow || low.AssigneeID != "" || low.ID == "" {
		t.Fatalf("defaults: %+v", low)
	}
	high, err := s.CreateTask(ctx, &Task{Title: "now", Priority: PriorityUrgent, AssigneeID: "gus", CreatorID: "ada", Description: "do it"})
	if err != nil {
		t.Fatalf("create high: %v", err)
	}
	got, err := s.GetTask(ctx, high.ID)
	if err != nil || got.Title != "now" || got.AssigneeID != "gus" || got.Description != "do it" {
		t.Fatalf("get: %+v %v", got, err)
	}
	tasks, err := s.ListTasks(ctx, TaskFilter{})
	if err != nil || len(tasks) != 2 || tasks[0].ID != high.ID {
		t.Fatalf("order: %+v %v", tasks, err)
	}
	mine, err := s.ListTasks(ctx, TaskFilter{AssigneeID: "gus"})
	if err != nil || len(mine) != 1 || mine[0].ID != high.ID {
		t.Fatalf("filter assignee: %+v %v", mine, err)
	}
	free, err := s.ListTasks(ctx, TaskFilter{Unassigned: true})
	if err != nil || len(free) != 1 || free[0].ID != low.ID {
		t.Fatalf("filter unassigned: %+v %v", free, err)
	}
	status := TaskInProgress
	updated, err := s.UpdateTask(ctx, high.ID, TaskPatch{Status: &status})
	if err != nil || updated.Status != TaskInProgress || updated.AssigneeID != "gus" {
		t.Fatalf("update: %+v %v", updated, err)
	}
	only, err := s.ListTasks(ctx, TaskFilter{Status: TaskInProgress})
	if err != nil || len(only) != 1 {
		t.Fatalf("filter status: %+v %v", only, err)
	}
	c1, err := s.AddTaskComment(ctx, &TaskComment{TaskID: high.ID, AuthorID: "gus", Body: "started"})
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	if _, err := s.AddTaskComment(ctx, &TaskComment{TaskID: high.ID, AuthorID: "ada", Body: "ack"}); err != nil {
		t.Fatalf("comment 2: %v", err)
	}
	notes, err := s.ListTaskComments(ctx, high.ID)
	if err != nil || len(notes) != 2 || notes[0].ID != c1.ID || notes[0].Body != "started" || notes[1].Body != "ack" {
		t.Fatalf("comments: %+v %v", notes, err)
	}
	if _, err := s.AddTaskComment(ctx, &TaskComment{TaskID: "missing", AuthorID: "ada", Body: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing task comment: %v", err)
	}
	if _, err := s.GetTask(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing get: %v", err)
	}
}

func TestTaskSessionClearedOnDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	mustAgent(t, s, "ada")
	if _, err := s.CreateSession(ctx, "room", "Room", "ada", nil); err != nil {
		t.Fatalf("session: %v", err)
	}
	task, err := s.CreateTask(ctx, &Task{Title: "linked", CreatorID: "ada", SessionID: "room"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.DeleteSession(ctx, "room"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SessionID != "" {
		t.Fatalf("session link should clear, got %q", got.SessionID)
	}
}

func TestResolveAssignee(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateAgent(ctx, "ada", "Ada Lovelace", "solo", "h", "/w", ""); err != nil {
		t.Fatalf("ada: %v", err)
	}
	if _, err := s.CreateAgent(ctx, "gus", "Gus", "pair", "h", "/w", ""); err != nil {
		t.Fatalf("gus: %v", err)
	}
	if _, err := s.CreateAgent(ctx, "bea", "Bea", "pair", "h", "/w2", ""); err != nil {
		t.Fatalf("bea: %v", err)
	}
	// Hidden app client: a live role of the same kind/host/workdir hides it.
	if _, err := s.CreateAgent(ctx, "client", "client", "opencode", "box", "/proj", ""); err != nil {
		t.Fatalf("client: %v", err)
	}
	if _, err := s.CreateAgent(ctx, "role", "Fix read marks", "opencode", "box", "/proj", "sess-1"); err != nil {
		t.Fatalf("role: %v", err)
	}
	if _, err := s.CreateAgent(ctx, "gone", "Gone", "solo", "h", "/old", ""); err != nil {
		t.Fatalf("gone: %v", err)
	}
	if err := s.RemoveAgent(ctx, "gone"); err != nil {
		t.Fatalf("remove: %v", err)
	}

	ada, err := s.ResolveAssignee(ctx, "ada")
	if err != nil || ada.ID != "ada" {
		t.Fatalf("id: %+v %v", ada, err)
	}
	ada, err = s.ResolveAssignee(ctx, "@Ada Lovelace")
	if err != nil || ada.ID != "ada" {
		t.Fatalf("name: %+v %v", ada, err)
	}
	ada, err = s.ResolveAssignee(ctx, "solo")
	if err != nil || ada.ID != "ada" {
		t.Fatalf("kind: %+v %v", ada, err)
	}
	role, err := s.ResolveAssignee(ctx, "Fix read marks")
	if err != nil || role.ID != "role" {
		t.Fatalf("role name: %+v %v", role, err)
	}
	if _, err := s.ResolveAssignee(ctx, "client"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hidden client id should not resolve: %v", err)
	}
	if _, err := s.ResolveAssignee(ctx, "pair"); !errors.Is(err, ErrConflict) {
		t.Fatalf("ambiguous kind: %v", err)
	}
	if _, err := s.ResolveAssignee(ctx, "gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("evicted: %v", err)
	}
	if _, err := s.ResolveAssignee(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestWebhooksAgentWinsOverKind(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	mustAgent(t, s, "ada")
	if _, err := s.CreateKind(ctx, "test", "Test", "hash"); err != nil {
		t.Fatalf("kind: %v", err)
	}
	// ada's kind column is "test" from mustAgent. Point a kind webhook at it.
	if _, err := s.SetKindWebhook(ctx, "test", "https://kind.example/hook"); err != nil {
		t.Fatalf("kind hook: %v", err)
	}
	url, err := s.WebhookURL(ctx, "ada")
	if err != nil || url != "https://kind.example/hook" {
		t.Fatalf("kind fallback: %q %v", url, err)
	}
	if _, err := s.SetAgentWebhook(ctx, "ada", "https://ada.example/hook"); err != nil {
		t.Fatalf("agent hook: %v", err)
	}
	url, err = s.WebhookURL(ctx, "ada")
	if err != nil || url != "https://ada.example/hook" {
		t.Fatalf("agent wins: %q %v", url, err)
	}
	hooks, err := s.ListWebhooks(ctx)
	if err != nil || len(hooks) != 2 {
		t.Fatalf("list: %+v %v", hooks, err)
	}
	if err := s.DeleteAgentWebhook(ctx, "ada"); err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	url, err = s.WebhookURL(ctx, "ada")
	if err != nil || url != "https://kind.example/hook" {
		t.Fatalf("fell back: %q %v", url, err)
	}
	if err := s.DeleteKindWebhook(ctx, "test"); err != nil {
		t.Fatalf("delete kind: %v", err)
	}
	url, err = s.WebhookURL(ctx, "ada")
	if err != nil || url != "" {
		t.Fatalf("none: %q %v", url, err)
	}
	if _, err := s.SetAgentWebhook(ctx, "missing", "https://x.example"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing agent: %v", err)
	}
	if _, err := s.SetKindWebhook(ctx, "missing", "https://x.example"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing kind: %v", err)
	}
}
