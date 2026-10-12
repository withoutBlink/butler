package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"butler/internal/ids"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "butler.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustAgent(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.CreateAgent(context.Background(), id, id, "test", "hash-"+id); err != nil {
		t.Fatalf("create agent %s: %v", id, err)
	}
}

func TestOpenIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "butler.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s1.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	s2.Close()
}

func TestAgentCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	a, err := s.CreateAgent(ctx, "a", "Alpha", "codex", "hash-a")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.ID != "a" || a.DisplayName != "Alpha" || a.Kind != "codex" {
		t.Fatalf("bad agent: %+v", a)
	}
	if _, err := s.CreateAgent(ctx, "a", "Again", "codex", "hash-b"); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	if _, err := s.GetAgentByTokenHash(ctx, "hash-a"); err != nil {
		t.Fatalf("lookup by hash: %v", err)
	}
	if _, err := s.GetAgentByTokenHash(ctx, "hash-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	if err := s.RotateAgentToken(ctx, "a", "hash-new"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := s.GetAgentByTokenHash(ctx, "hash-new"); err != nil {
		t.Fatalf("lookup new hash: %v", err)
	}
	if _, err := s.GetAgentByTokenHash(ctx, "hash-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old hash should be gone, got %v", err)
	}
	if err := s.RotateAgentToken(ctx, "ghost", "hash-x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	s.TouchAgent(ctx, "a")
	got, err := s.GetAgent(ctx, "a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastSeenAt == nil {
		t.Fatal("last_seen_at should be set after touch")
	}
	list, err := s.ListAgents(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %d", err, len(list))
	}
}

func TestMessageSeqMonotonic(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mustAgent(t, s, "a")
	if _, err := s.CreateSession(ctx, "s1", "one", "a", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	const goroutines, perGoroutine = 10, 20
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				if _, err := s.InsertMessage(ctx, &Message{ID: "m-" + ids.New(), SessionID: "s1", SenderID: "a", Kind: "chat", Content: "hello"}); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("insert: %v", err)
	}
	rows, err := s.db.Query(`SELECT global_seq FROM messages ORDER BY global_seq`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var got []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, n)
	}
	if len(got) != goroutines*perGoroutine {
		t.Fatalf("expected %d messages, got %d", goroutines*perGoroutine, len(got))
	}
	for i, n := range got {
		if n != int64(i+1) {
			t.Fatalf("seq at %d = %d, want %d", i, n, i+1)
		}
	}
}

func TestInsertMessageIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mustAgent(t, s, "a")
	if _, err := s.CreateSession(ctx, "s1", "one", "a", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	m1, err := s.InsertMessage(ctx, &Message{ID: "fixed", SessionID: "s1", SenderID: "a", Kind: "chat", Content: "one"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	m2, err := s.InsertMessage(ctx, &Message{ID: "fixed", SessionID: "s1", SenderID: "a", Kind: "chat", Content: "two"})
	if err != nil {
		t.Fatalf("insert dup: %v", err)
	}
	if m1.Seq != m2.Seq || m2.Content != "one" {
		t.Fatalf("retry should return original: %+v vs %+v", m1, m2)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row, got %d", count)
	}
}

func TestJoinSession(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mustAgent(t, s, "a")
	mustAgent(t, s, "b")
	if _, err := s.CreateSession(ctx, "s1", "one", "a", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	joined, err := s.JoinSession(ctx, "s1", "b")
	if err != nil || !joined {
		t.Fatalf("join: %v %v", joined, err)
	}
	msgs, err := s.MessagesSince(ctx, "", "s1", 0, 10)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Kind != "system" || msgs[0].Content != "b joined the session" || msgs[0].SenderID != "b" {
		t.Fatalf("bad system message: %+v", msgs)
	}
	joined, err = s.JoinSession(ctx, "s1", "b")
	if err != nil || joined {
		t.Fatalf("second join should be a no-op: %v %v", joined, err)
	}
	msgs, _ = s.MessagesSince(ctx, "", "s1", 0, 10)
	if len(msgs) != 1 {
		t.Fatalf("no extra message expected, got %d", len(msgs))
	}
	if _, err := s.JoinSession(ctx, "nope", "b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestMessagesSince(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mustAgent(t, s, "a")
	mustAgent(t, s, "b")
	if _, err := s.CreateSession(ctx, "s1", "one", "a", []string{"b"}); err != nil {
		t.Fatalf("create s1: %v", err)
	}
	if _, err := s.CreateSession(ctx, "s2", "two", "a", nil); err != nil {
		t.Fatalf("create s2: %v", err)
	}
	m1, err := s.InsertMessage(ctx, &Message{SessionID: "s1", SenderID: "a", Kind: "chat", Content: "in-s1"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := s.InsertMessage(ctx, &Message{SessionID: "s2", SenderID: "a", Kind: "chat", Content: "in-s2"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	msgs, err := s.MessagesSince(ctx, "b", "", 0, 100)
	if err != nil {
		t.Fatalf("b since: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != m1.ID {
		t.Fatalf("b should only see s1: %+v", msgs)
	}
	msgs, err = s.MessagesSince(ctx, "b", "", m1.Seq, 100)
	if err != nil || len(msgs) != 0 {
		t.Fatalf("cursor should return nothing: %v %+v", err, msgs)
	}
	msgs, err = s.MessagesSince(ctx, "", "s1", 0, 100)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("session filter: %v %+v", err, msgs)
	}
	msgs, err = s.MessagesSince(ctx, "a", "", 0, 100)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("a should see both: %v %+v", err, msgs)
	}
}

func TestContext(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mustAgent(t, s, "a")
	if _, err := s.CreateSession(ctx, "s1", "one", "a", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, seq, err := s.UpsertContext(ctx, "s1", "k", "v1", "a"); err != nil || seq != 1 {
		t.Fatalf("upsert: seq=%d err=%v", seq, err)
	}
	if _, seq, err := s.UpsertContext(ctx, "s1", "k", "v2", "a"); err != nil || seq != 2 {
		t.Fatalf("upsert2: seq=%d err=%v", seq, err)
	}
	item, err := s.GetContextItem(ctx, "s1", "k")
	if err != nil || item.Value != "v2" {
		t.Fatalf("get item: %v %+v", err, item)
	}
	if _, err := s.GetContextItem(ctx, "s1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	items, err := s.ListContext(ctx, "s1")
	if err != nil || len(items) != 1 {
		t.Fatalf("list: %v %d", err, len(items))
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE kind = 'context'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 context messages, got %d", count)
	}
}

func TestSessionListAndStatus(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mustAgent(t, s, "a")
	mustAgent(t, s, "b")
	if _, err := s.CreateSession(ctx, "s1", "one", "a", []string{"b"}); err != nil {
		t.Fatalf("create s1: %v", err)
	}
	if _, err := s.CreateSession(ctx, "s2", "two", "a", nil); err != nil {
		t.Fatalf("create s2: %v", err)
	}
	all, err := s.ListSessions(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("all: %v %d", err, len(all))
	}
	joined, err := s.ListSessions(ctx, "b")
	if err != nil || len(joined) != 1 || joined[0].ID != "s1" {
		t.Fatalf("joined: %v %+v", err, joined)
	}
	if err := s.SetSessionStatus(ctx, "s1", "archived"); err != nil {
		t.Fatalf("status: %v", err)
	}
	sess, err := s.GetSession(ctx, "s1")
	if err != nil || sess.Status != "archived" {
		t.Fatalf("archived: %v %+v", err, sess)
	}
	if err := s.SetSessionStatus(ctx, "nope", "archived"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}
