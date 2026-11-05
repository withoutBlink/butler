package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildActivity(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ago := func(d time.Duration) *int64 {
		v := now.Add(-d).Unix()
		return &v
	}
	agent := func(id string, seen, action *int64, state, note string, at *int64) Agent {
		return Agent{ID: id, LastSeenAt: seen, LastActionAt: action, ActivityState: state, ActivityNote: note, ActivityAt: at}
	}
	task := func(id, assignee, status string) Task {
		return Task{ID: id, AssigneeID: assignee, Status: status}
	}
	cases := []struct {
		name    string
		agents  []Agent
		tasks   []Task
		waiting map[string]bool
		always  map[string]bool
		id      string
		state   string
		note    string
		stalled string
	}{
		{name: "never seen", agents: []Agent{{ID: "a"}}, id: "a", state: ActivityOffline},
		{name: "recent presence is idle", agents: []Agent{agent("a", ago(10*time.Second), nil, "", "", nil)}, id: "a", state: ActivityIdle},
		{name: "presence past the window is offline", agents: []Agent{agent("a", ago(2*time.Minute), nil, "", "", nil)}, id: "a", state: ActivityOffline},
		{name: "recent action is working", agents: []Agent{agent("a", ago(time.Second), ago(10*time.Second), "", "", nil)}, id: "a", state: ActivityWorking},
		{name: "action older than the working window falls back to idle", agents: []Agent{agent("a", ago(10*time.Second), ago(2*time.Minute), "", "", nil)}, id: "a", state: ActivityIdle},
		{name: "fresh explicit working", agents: []Agent{agent("a", ago(time.Second), nil, ActivityWorking, "editing the parser", ago(20*time.Second))}, id: "a", state: ActivityWorking, note: "editing the parser"},
		{name: "fresh explicit idle beats an older action", agents: []Agent{agent("a", ago(time.Second), ago(30*time.Second), ActivityIdle, "", ago(5*time.Second))}, id: "a", state: ActivityIdle},
		{name: "newer action beats an older idle report", agents: []Agent{agent("a", ago(time.Second), ago(10*time.Second), ActivityIdle, "", ago(30*time.Second))}, id: "a", state: ActivityWorking},
		{name: "expired report does not stick", agents: []Agent{agent("a", ago(10*time.Second), nil, ActivityWorking, "old", ago(3*time.Minute))}, id: "a", state: ActivityIdle},
		{
			name:   "claimed in_progress and still around is working",
			agents: []Agent{agent("a", ago(10*time.Second), nil, "", "", nil)},
			tasks:  []Task{task("t1", "a", TaskInProgress)},
			id:     "a", state: ActivityWorking,
		},
		{
			name:   "quiet in_progress owner is stalled",
			agents: []Agent{agent("a", ago(10*time.Minute), ago(10*time.Minute), "", "", nil)},
			tasks:  []Task{task("t1", "a", TaskInProgress)},
			id:     "a", state: ActivityStalled, stalled: "t1",
		},
		{
			name:    "held long-poll keeps a quiet clock from stalling",
			agents:  []Agent{agent("a", ago(10*time.Minute), nil, "", "", nil)},
			tasks:   []Task{task("t1", "a", TaskInProgress)},
			waiting: map[string]bool{"a": true},
			id:      "a", state: ActivityWorking,
		},
		{
			name:   "missing owner stalls the task",
			tasks:  []Task{task("t1", "gone", TaskInProgress)},
			agents: []Agent{{ID: "other"}},
			id:     "other", state: ActivityOffline, stalled: "t1",
		},
		{
			name:   "blocked task while online",
			agents: []Agent{agent("a", ago(10*time.Second), nil, "", "", nil)},
			tasks:  []Task{task("t1", "a", TaskBlocked)},
			id:     "a", state: ActivityBlocked, note: "task blocked",
		},
		{
			name:   "review assignee is waiting",
			agents: []Agent{agent("a", ago(10*time.Second), nil, "", "", nil)},
			tasks:  []Task{task("t1", "a", TaskReview)},
			id:     "a", state: ActivityBlocked, note: "waiting on review",
		},
		{
			name:   "blocked task while offline is just offline",
			agents: []Agent{agent("a", ago(10*time.Minute), nil, "", "", nil)},
			tasks:  []Task{task("t1", "a", TaskBlocked)},
			id:     "a", state: ActivityOffline,
		},
		{
			name:   "quiet todo is not stalled",
			agents: []Agent{agent("a", ago(10*time.Minute), nil, "", "", nil)},
			tasks:  []Task{task("t1", "a", TaskTodo)},
			id:     "a", state: ActivityOffline,
		},
		{
			name:   "fresh blocked report wins over in_progress",
			agents: []Agent{agent("a", ago(time.Second), nil, ActivityBlocked, "waiting on the human", ago(15*time.Second))},
			tasks:  []Task{task("t1", "a", TaskInProgress)},
			id:     "a", state: ActivityBlocked, note: "waiting on the human",
		},
		{
			name:   "panel human with in_progress stays idle and the task is not stalled",
			agents: []Agent{{ID: "admin"}},
			tasks:  []Task{task("t1", "admin", TaskInProgress)},
			always: map[string]bool{"admin": true},
			id:     "admin", state: ActivityIdle,
		},
		{
			name:   "panel human who just posted is working",
			agents: []Agent{agent("admin", nil, ago(10*time.Second), "", "", nil)},
			always: map[string]bool{"admin": true},
			id:     "admin", state: ActivityWorking,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := BuildActivity(tc.agents, tc.tasks, ActivityInput{Now: now, Waiting: tc.waiting, AlwaysPresent: tc.always})
			got := snap.ByAgent[tc.id]
			if got.State != tc.state || got.Note != tc.note {
				t.Fatalf("activity = %+v, want state %q note %q", got, tc.state, tc.note)
			}
			if tc.stalled == "" {
				if len(snap.Stalled) != 0 {
					t.Fatalf("unexpected stalled tasks: %v", snap.Stalled)
				}
				return
			}
			if !snap.Stalled[tc.stalled] {
				t.Fatalf("task %s should be stalled, got %v", tc.stalled, snap.Stalled)
			}
		})
	}
}

func TestNormalizeActivity(t *testing.T) {
	state, note, err := NormalizeActivity("  Working ", "  on it  ")
	if err != nil || state != ActivityWorking || note != "on it" {
		t.Fatalf("normalize: %q %q %v", state, note, err)
	}
	if _, _, err := NormalizeActivity("stalled", ""); err == nil {
		t.Fatal("stalled is derived, not a report")
	}
	if _, _, err := NormalizeActivity(ActivityIdle, strings.Repeat("x", ActivityNoteMax+1)); err == nil {
		t.Fatal("over-long note should fail")
	}
	if _, _, err := NormalizeActivity(ActivityBlocked, "line\nbreak"); err == nil {
		t.Fatal("control character should fail")
	}
}

func openActivityStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "butler.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSetActivityRoundTrip(t *testing.T) {
	s := openActivityStore(t)
	ctx := context.Background()
	if _, err := s.CreateAgent(ctx, "ada", "Ada", "codex", "h", "/w", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetActivity(ctx, "ada", ActivityBlocked, "waiting on review"); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := s.GetAgent(ctx, "ada")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ActivityState != ActivityBlocked || got.ActivityNote != "waiting on review" || got.ActivityAt == nil || got.LastSeenAt == nil {
		t.Fatalf("stored report: %+v", got)
	}
	if got.LastActionAt != nil {
		t.Fatal("a self-report is not a substantive action")
	}
	s.TouchAction(ctx, "ada")
	got, err = s.GetAgent(ctx, "ada")
	if err != nil || got.LastActionAt == nil {
		t.Fatalf("touch action: %+v %v", got, err)
	}
	if err := s.SetActivity(ctx, "missing", ActivityIdle, ""); err != ErrNotFound {
		t.Fatalf("missing agent: %v", err)
	}
}

func TestMigrateActivityColumns(t *testing.T) {
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
    created_at INTEGER NOT NULL,
    last_seen_at INTEGER,
    removed_at INTEGER
);`
	if _, err := db.Exec(old); err != nil {
		t.Fatalf("old schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO agents (id, display_name, kind, hostname, workdir, client_session, created_at) VALUES ('ada', 'Ada', 'codex', 'h', '/w', '', 10)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.SetActivity(context.Background(), "ada", ActivityWorking, "ported"); err != nil {
		t.Fatalf("set after migrate: %v", err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	got, err := s.GetAgent(context.Background(), "ada")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ActivityState != ActivityWorking || got.ActivityNote != "ported" {
		t.Fatalf("report did not survive reopen: %+v", got)
	}
}
