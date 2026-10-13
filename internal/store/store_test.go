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
	if _, err := s.CreateAgent(context.Background(), id, id, "test", "", ""); err != nil {
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

func TestKindCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	k, err := s.CreateKind(ctx, "codex", "Codex", "hash-codex")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if k.ID != "codex" || k.DisplayName != "Codex" {
		t.Fatalf("bad kind: %+v", k)
	}
	if _, err := s.CreateKind(ctx, "codex", "Again", "hash-c2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	got, err := s.GetKind(ctx, "codex")
	if err != nil || got.ID != "codex" {
		t.Fatalf("get: %v %+v", err, got)
	}
	if _, err := s.GetKind(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	if _, err := s.GetKindByTokenHash(ctx, "hash-codex"); err != nil {
		t.Fatalf("lookup by hash: %v", err)
	}
	if _, err := s.GetKindByTokenHash(ctx, "hash-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	if err := s.RotateKindToken(ctx, "codex", "hash-new"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := s.GetKindByTokenHash(ctx, "hash-new"); err != nil {
		t.Fatalf("lookup new hash: %v", err)
	}
	if _, err := s.GetKindByTokenHash(ctx, "hash-codex"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old hash should be gone, got %v", err)
	}
	if err := s.RotateKindToken(ctx, "ghost", "hash-x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	list, err := s.ListKinds(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %d", err, len(list))
	}
}

func TestAgentInstanceTokens(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	a, err := s.CreateAgent(ctx, "codex-laptop", "codex-laptop", "codex", "laptop.local", "/home/x/proj")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.Hostname != "laptop.local" || a.Workdir != "/home/x/proj" {
		t.Fatalf("bad agent: %+v", a)
	}
	if _, err := s.CreateAgent(ctx, "codex-laptop", "again", "codex", "laptop.local", "/home/x/proj"); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	// One live instance token per identity: replacing revokes the previous.
	if err := s.ReplaceAgentToken(ctx, "codex-laptop", "hash-1"); err != nil {
		t.Fatalf("replace token: %v", err)
	}
	if got, err := s.GetAgentByTokenHash(ctx, "hash-1"); err != nil || got.ID != "codex-laptop" {
		t.Fatalf("lookup by hash-1: %v %+v", err, got)
	}
	if err := s.ReplaceAgentToken(ctx, "codex-laptop", "hash-2"); err != nil {
		t.Fatalf("replace token 2: %v", err)
	}
	if _, err := s.GetAgentByTokenHash(ctx, "hash-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replaced token should be revoked, got %v", err)
	}
	if got, err := s.GetAgentByTokenHash(ctx, "hash-2"); err != nil || got.ID != "codex-laptop" {
		t.Fatalf("lookup by hash-2: %v %+v", err, got)
	}
	if _, err := s.GetAgentByTokenHash(ctx, "hash-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	if err := s.ReplaceAgentToken(ctx, "ghost", "hash-x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("token for unknown agent should be not found, got %v", err)
	}
	// FindInstance matches on the registration triple.
	inst, err := s.FindInstance(ctx, "codex", "laptop.local", "/home/x/proj")
	if err != nil || inst.ID != "codex-laptop" {
		t.Fatalf("find instance: %v %+v", err, inst)
	}
	if _, err := s.FindInstance(ctx, "codex", "laptop.local", "/elsewhere"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	// Revoking kills the remaining token.
	if err := s.RevokeAgentTokens(ctx, "codex-laptop"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.GetAgentByTokenHash(ctx, "hash-2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("token hash-2 should be revoked, got %v", err)
	}
	s.TouchAgent(ctx, "codex-laptop")
	got, err := s.GetAgent(ctx, "codex-laptop")
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

func TestSessionsForAgent(t *testing.T) {
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
	sids, err := s.SessionsForAgent(ctx, "b")
	if err != nil || len(sids) != 1 || sids[0] != "s1" {
		t.Fatalf("sessions for b: %v %v", err, sids)
	}
	sids, err = s.SessionsForAgent(ctx, "a")
	if err != nil || len(sids) != 2 || sids[0] != "s1" || sids[1] != "s2" {
		t.Fatalf("sessions for a: %v %v", err, sids)
	}
	sids, err = s.SessionsForAgent(ctx, "ghost")
	if err != nil || len(sids) != 0 {
		t.Fatalf("sessions for ghost: %v %v", err, sids)
	}
}

func TestUniqueAgentName(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// kind + hostname + basename of the workdir; long host parts truncate.
	if got := s.UniqueAgentName(ctx, "codex", "Andrews-MacBook-Pro.local", "/Users/andrew/work/butler"); got != "codex-andrews-macbook-pro-butler" {
		t.Fatalf("got %q", got)
	}
	first := s.UniqueAgentName(ctx, "codex", "host", "/tmp/proj")
	if _, err := s.CreateAgent(ctx, first, first, "codex", "host", "/tmp/proj"); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Different directory, same basename: distinct identity, suffixed name.
	if got := s.UniqueAgentName(ctx, "codex", "host", "/other/proj"); got != "codex-host-proj-2" {
		t.Fatalf("expected suffix, got %q", got)
	}
	// Different directories on one host get distinct names, no suffix needed.
	if got := s.UniqueAgentName(ctx, "codex", "host", "/tmp/other"); got != "codex-host-other" {
		t.Fatalf("got %q", got)
	}
	// Long parts are truncated to a total cap.
	long := s.UniqueAgentName(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "/tmp/cccccccccccccccccccccccccccccccccccc")
	if len(long) > 48 {
		t.Fatalf("expected truncation, got %q", long)
	}
	if got := s.UniqueAgentName(ctx, "!!!", "???", "/???"); got != "agent" {
		t.Fatalf("expected fallback, got %q", got)
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
