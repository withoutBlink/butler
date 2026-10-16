package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"butler/internal/auth"
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
	if _, err := s.CreateAgent(context.Background(), id, id, "test", "", "", ""); err != nil {
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

	a, err := s.CreateAgent(ctx, "codex-laptop", "codex-laptop", "codex", "laptop.local", "/home/x/proj", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.Hostname != "laptop.local" || a.Workdir != "/home/x/proj" {
		t.Fatalf("bad agent: %+v", a)
	}
	if _, err := s.CreateAgent(ctx, "codex-laptop", "again", "codex", "laptop.local", "/home/x/proj", ""); !errors.Is(err, ErrConflict) {
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
	inst, err := s.FindInstance(ctx, "codex", "laptop.local", "/home/x/proj", "")
	if err != nil || inst.ID != "codex-laptop" {
		t.Fatalf("find instance: %v %+v", err, inst)
	}
	if _, err := s.FindInstance(ctx, "codex", "laptop.local", "/elsewhere", ""); !errors.Is(err, ErrNotFound) {
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

	// kind:hostname:basename, ":"-joined; long host parts truncate.
	if got := s.UniqueAgentName(ctx, "codex", "Andrews-MacBook-Pro.local", "/Users/andrew/work/butler"); got != "codex:andrews-macbook-pro:butler" {
		t.Fatalf("got %q", got)
	}
	first := s.UniqueAgentName(ctx, "codex", "host", "/tmp/proj")
	if _, err := s.CreateAgent(ctx, first, first, "codex", "host", "/tmp/proj", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Different directory, same basename: distinct identity, suffixed name.
	if got := s.UniqueAgentName(ctx, "codex", "host", "/other/proj"); got != "codex:host:proj-2" {
		t.Fatalf("expected suffix, got %q", got)
	}
	// Different directories on one host get distinct names, no suffix needed.
	if got := s.UniqueAgentName(ctx, "codex", "host", "/tmp/other"); got != "codex:host:other" {
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

func TestMessageMentions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mustAgent(t, s, "admin")
	for _, a := range []struct{ id, kind string }{
		{"codex:alpha:proj", "codex"},
		{"codex:beta:proj", "codex"},
		{"opencode:host:repo", "opencode"},
	} {
		if _, err := s.CreateAgent(ctx, a.id, a.id, a.kind, "h", "/w", ""); err != nil {
			t.Fatalf("create agent %s: %v", a.id, err)
		}
	}
	if _, err := s.CreateSession(ctx, "s1", "room", "admin", []string{"codex:alpha:proj", "codex:beta:proj", "opencode:host:repo"}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	post := func(content string) *Message {
		t.Helper()
		m, err := s.InsertMessage(ctx, &Message{SessionID: "s1", SenderID: "admin", Kind: "chat", Content: content})
		if err != nil {
			t.Fatalf("insert %q: %v", content, err)
		}
		return m
	}
	wantMentions := func(m *Message, want ...string) {
		t.Helper()
		if len(m.Mentions) != len(want) {
			t.Fatalf("mentions for %q = %v, want %v", m.Content, m.Mentions, want)
		}
		for i, id := range want {
			if m.Mentions[i] != id {
				t.Fatalf("mentions for %q = %v, want %v", m.Content, m.Mentions, want)
			}
		}
	}

	// Exact id, kind, and @all all resolve to participants.
	wantMentions(post("@codex:alpha:proj please review"), "codex:alpha:proj")
	wantMentions(post("@codex stand up"), "codex:alpha:proj", "codex:beta:proj")
	// @all includes every participant (id order), the human ("admin") included.
	wantMentions(post("@all heads up"), "admin", "codex:alpha:proj", "codex:beta:proj", "opencode:host:repo")
	// A kind only matches participants of that kind.
	wantMentions(post("@opencode ping"), "opencode:host:repo")
	// Non-participants and unknown tokens resolve to nothing.
	wantMentions(post("@ghost hello"))
	// Mixed, deduped in order of first appearance.
	wantMentions(post("@all @codex @opencode:host:repo check @codex again"),
		"admin", "codex:alpha:proj", "codex:beta:proj", "opencode:host:repo")
	// No @ at all.
	wantMentions(post("plain message"))

	// Mentions persist and come back on read.
	msgs, err := s.MessagesSince(ctx, "", "s1", 0, 100)
	if err != nil {
		t.Fatalf("messages since: %v", err)
	}
	if len(msgs) != 7 {
		t.Fatalf("expected 7 messages, got %d", len(msgs))
	}
	// msgs[0] is the exact-id mention; msgs[1] the kind mention.
	if !msgs[0].MentionsAgent("codex:alpha:proj") || msgs[0].MentionsAgent("codex:beta:proj") {
		t.Fatalf("MentionsAgent on exact mention: %v", msgs[0].Mentions)
	}
	if !msgs[1].MentionsAgent("codex:alpha:proj") || !msgs[1].MentionsAgent("codex:beta:proj") {
		t.Fatalf("MentionsAgent on kind mention: %v", msgs[1].Mentions)
	}
	if msgs[6].MentionsAgent("codex:alpha:proj") {
		t.Fatalf("plain message should mention nobody: %v", msgs[6].Mentions)
	}

	// System notices (join announcements) carry no mentions.
	mustAgent(t, s, "late:joiner:dir")
	if _, err := s.JoinSession(ctx, "s1", "late:joiner:dir"); err != nil {
		t.Fatalf("join: %v", err)
	}
	msgs, err = s.SessionLatestMessages(ctx, "s1", 1)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("latest: %v %d", err, len(msgs))
	}
	if msgs[0].Kind != "system" || len(msgs[0].Mentions) != 0 {
		t.Fatalf("join notice should be a mention-less system message: %+v", msgs[0])
	}
}

func TestGlobalSession(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mustAgent(t, s, "admin")
	mustAgent(t, s, "codex:alpha:proj")

	created, err := s.EnsureGlobalSession(ctx, "admin")
	if err != nil || !created {
		t.Fatalf("ensure: %v created=%v", err, created)
	}
	sess, err := s.GetSession(ctx, GlobalSessionID)
	if err != nil || sess.Title != GlobalSessionTitle || sess.CreatedBy != "admin" {
		t.Fatalf("global session: %v %+v", err, sess)
	}
	// Idempotent: second call changes nothing (no duplicate, no new message).
	created, err = s.EnsureGlobalSession(ctx, "admin")
	if err != nil || created {
		t.Fatalf("second ensure should be a no-op: %v created=%v", err, created)
	}

	// Agents join like in any other session.
	joined, err := s.JoinSession(ctx, GlobalSessionID, "codex:alpha:proj")
	if err != nil || !joined {
		t.Fatalf("join global: %v joined=%v", err, joined)
	}
	joined, err = s.JoinSession(ctx, GlobalSessionID, "codex:alpha:proj")
	if err != nil || joined {
		t.Fatalf("re-join global: %v joined=%v", err, joined)
	}
	sids, err := s.SessionsForAgent(ctx, "codex:alpha:proj")
	if err != nil || len(sids) != 1 || sids[0] != GlobalSessionID {
		t.Fatalf("sessions for agent: %v %v", err, sids)
	}
	if ok, err := s.IsParticipant(ctx, GlobalSessionID, "admin"); err != nil || !ok {
		t.Fatalf("you should be a participant: %v %v", err, ok)
	}

	// An unknown creator is a not-found error, not a crash.
	if _, err := s.EnsureGlobalSession(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found for unknown creator, got %v", err)
	}
}

func TestMarkRead(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	mustAgent(t, s, "a1")
	mustAgent(t, s, "a2")
	if _, err := s.CreateSession(ctx, "s1", "marks", "a1", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := s.CreateSession(ctx, "s2", "other", "a1", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Cursors are monotonic: a lower seq never moves one back.
	if err := s.MarkRead(ctx, "s1", "a1", 5); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if err := s.MarkRead(ctx, "s1", "a1", 3); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	marks, err := s.ReadMarks(ctx, "s1")
	if err != nil {
		t.Fatalf("read marks: %v", err)
	}
	if len(marks) != 1 || marks["a1"] != 5 {
		t.Fatalf("expected {a1:5}, got %v", marks)
	}

	// Cursors are per agent and per session.
	if err := s.MarkRead(ctx, "s1", "a2", 2); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if err := s.MarkRead(ctx, "s2", "a1", 7); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if err := s.MarkRead(ctx, "s1", "a1", 9); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	marks, err = s.ReadMarks(ctx, "s1")
	if err != nil {
		t.Fatalf("read marks: %v", err)
	}
	if marks["a1"] != 9 || marks["a2"] != 2 {
		t.Fatalf("expected {a1:9, a2:2}, got %v", marks)
	}
	marks, err = s.ReadMarks(ctx, "s2")
	if err != nil {
		t.Fatalf("read marks: %v", err)
	}
	if len(marks) != 1 || marks["a1"] != 7 {
		t.Fatalf("expected {a1:7}, got %v", marks)
	}

	// Zero seqs are a no-op (nothing delivered yet).
	if err := s.MarkRead(ctx, "s2", "a2", 0); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	marks, err = s.ReadMarks(ctx, "s2")
	if err != nil {
		t.Fatalf("read marks: %v", err)
	}
	if _, ok := marks["a2"]; ok {
		t.Fatalf("zero seq should not create a cursor, got %v", marks)
	}
}

// TestMigration004AdminAgent builds a pre-004 database with a "you" agent
// holding messages, participants, mentions and read cursors, then opens it
// with the store and verifies the rename to "admin" reached every table.
func TestMigration004AdminAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "butler.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	ctx := context.Background()
	for _, name := range []string{"001_init.sql", "002_mentions.sql", "003_read_marks.sql"} {
		script, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if _, err := db.Exec(string(script)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (1, 0), (2, 0), (3, 0)`,
		`INSERT INTO agents (id, display_name, kind, created_at) VALUES ('you', 'You', 'human', 0)`,
		`INSERT INTO sessions (id, title, created_by, created_at) VALUES ('global', 'Global', 'you', 0)`,
		`INSERT INTO participants (session_id, agent_id, joined_at) VALUES ('global', 'you', 0)`,
		`INSERT INTO messages (id, session_id, sender_id, kind, content, mentions, created_at) VALUES ('m1', 'global', 'you', 'chat', 'hi there', '["you"]', 0)`,
		`INSERT INTO read_marks (session_id, agent_id, last_seq) VALUES ('global', 'you', 1)`,
		`INSERT INTO context_items (session_id, key, value, author_id, updated_at) VALUES ('global', 'note', 'v', 'you', 0)`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open store (applies 004): %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if _, err := s.GetAgent(ctx, "you"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old you agent must be gone, got %v", err)
	}
	ag, err := s.GetAgent(ctx, "admin")
	if err != nil || ag.DisplayName != "Admin" || ag.Kind != "human" {
		t.Fatalf("admin agent: %v %+v", err, ag)
	}
	sess, err := s.GetSession(ctx, "global")
	if err != nil || sess.CreatedBy != "admin" {
		t.Fatalf("global session creator: %v %+v", err, sess)
	}
	if ok, err := s.IsParticipant(ctx, "global", "admin"); err != nil || !ok {
		t.Fatalf("admin participant: %v %v", ok, err)
	}
	msg, err := s.GetMessage(ctx, "m1")
	if err != nil || msg.SenderID != "admin" {
		t.Fatalf("message sender: %v %+v", err, msg)
	}
	if !msg.MentionsAgent("admin") || msg.MentionsAgent("you") {
		t.Fatalf("mentions should follow the rename: %+v", msg.Mentions)
	}
	marks, err := s.ReadMarks(ctx, "global")
	if err != nil || marks["admin"] != 1 {
		t.Fatalf("read marks: %v %v", err, marks)
	}
	item, err := s.GetContextItem(ctx, "global", "note")
	if err != nil || item.AuthorID != "admin" {
		t.Fatalf("context author: %v %+v", err, item)
	}

	// The rename is idempotent from the store's perspective: re-opening
	// applies nothing new and everything still resolves.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	if _, err := s2.GetAgent(ctx, "admin"); err != nil {
		t.Fatalf("admin after reopen: %v", err)
	}
}

// TestUniqueSessionAgentName: per-client-session names are minted from the
// kind and a slug of the client session's title, with the workdir basename
// as fallback and counter suffixes on collision.
func TestUniqueSessionAgentName(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if got := s.UniqueSessionAgentName(ctx, "opencode", "Fix read marks!", "/w/butler"); got != "opencode:fix-read-marks" {
		t.Fatalf("got %q", got)
	}
	first := s.UniqueSessionAgentName(ctx, "opencode", "Fix read marks!", "/w/butler")
	if _, err := s.CreateAgent(ctx, first, "Fix read marks!", "opencode", "h", "/w/butler", "ses_one"); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Same title, different session: suffix.
	if got := s.UniqueSessionAgentName(ctx, "opencode", "Fix read marks", "/w/butler"); got != "opencode:fix-read-marks-2" {
		t.Fatalf("expected suffix, got %q", got)
	}
	// A title with no usable characters falls back to the workdir basename.
	if got := s.UniqueSessionAgentName(ctx, "opencode", "???", "/w/butler"); got != "opencode:butler" {
		t.Fatalf("expected workdir fallback, got %q", got)
	}
	// Very long titles truncate within the total cap.
	long := s.UniqueSessionAgentName(ctx, "opencode", strings.Repeat("word ", 20), "/w/butler")
	if len(long) > 48 {
		t.Fatalf("expected truncation, got %q", long)
	}
}

// TestClientSessionIdentity: FindInstance discriminates the app-level agent
// (client_session "") from its per-session siblings, and sibling identity is
// stable across re-registration.
func TestClientSessionIdentity(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	app, err := s.CreateAgent(ctx, "opencode:host:butler", "opencode:host:butler", "opencode", "host", "/w/butler", "")
	if err != nil {
		t.Fatalf("create app-level: %v", err)
	}
	sib, err := s.CreateAgent(ctx, "opencode:fix-read-marks", "Fix read marks", "opencode", "host", "/w/butler", "ses_one")
	if err != nil {
		t.Fatalf("create sibling: %v", err)
	}

	for _, tc := range []struct {
		clientSession string
		want          *Agent
	}{
		{"", app},
		{"ses_one", sib},
		{"ses_two", nil},
	} {
		got, err := s.FindInstance(ctx, "opencode", "host", "/w/butler", tc.clientSession)
		if tc.want == nil {
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("client_session %q: expected not found, got %v", tc.clientSession, err)
			}
			continue
		}
		if err != nil || got.ID != tc.want.ID {
			t.Fatalf("client_session %q: expected %q, got %v (%v)", tc.clientSession, tc.want.ID, got, err)
		}
		if got.ClientSession != tc.clientSession {
			t.Fatalf("client_session roundtrip: %+v", got)
		}
	}

	// Token resolution and display-name updates work on siblings too.
	tok := ids.New()
	if err := s.ReplaceAgentToken(ctx, sib.ID, auth.Hash(tok)); err != nil {
		t.Fatalf("replace token: %v", err)
	}
	byTok, err := s.GetAgentByTokenHash(ctx, auth.Hash(tok))
	if err != nil || byTok.ID != sib.ID || byTok.ClientSession != "ses_one" {
		t.Fatalf("token hash lookup: %v %+v", err, byTok)
	}
	if err := s.UpdateAgentDisplayName(ctx, sib.ID, "Fix read marks (renamed)"); err != nil {
		t.Fatalf("update display name: %v", err)
	}
	got, err := s.GetAgent(ctx, sib.ID)
	if err != nil || got.DisplayName != "Fix read marks (renamed)" {
		t.Fatalf("display name update: %v %+v", err, got)
	}
}

// TestRemoveAgent: eviction revokes tokens and hides the agent from
// listings and session participants while the row survives (history
// references it); FindInstance still resolves it and ClearAgentRemoved
// revives it (a live process re-registering).
func TestRemoveAgent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	a, err := s.CreateAgent(ctx, "opencode:fix-read-marks", "Fix read marks", "opencode", "host", "/w/butler", "ses_one")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	tok := ids.New()
	if err := s.ReplaceAgentToken(ctx, a.ID, auth.Hash(tok)); err != nil {
		t.Fatalf("replace token: %v", err)
	}
	if _, err := s.CreateSession(ctx, "s1", "kick", "admin", []string{a.ID}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := s.RemoveAgent(ctx, a.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// Double eviction is a not-found (nothing left to kick).
	if err := s.RemoveAgent(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double remove: %v", err)
	}

	// Hidden from listings and participants.
	agents, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, ag := range agents {
		if ag.ID == a.ID {
			t.Fatalf("evicted agent listed: %+v", ag)
		}
	}
	parts, err := s.Participants(ctx, "s1")
	if err != nil {
		t.Fatalf("participants: %v", err)
	}
	for _, p := range parts {
		if p.ID == a.ID {
			t.Fatalf("evicted agent still a participant: %+v", p)
		}
	}

	// Token gone; row survives with removed_at set; still resolvable by
	// identity key for the revive path.
	if _, err := s.GetAgentByTokenHash(ctx, auth.Hash(tok)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("evicted token should not resolve: %v", err)
	}
	stored, err := s.GetAgent(ctx, a.ID)
	if err != nil || stored.RemovedAt == nil {
		t.Fatalf("row should survive eviction: %v %+v", err, stored)
	}
	fi, err := s.FindInstance(ctx, "opencode", "host", "/w/butler", "ses_one")
	if err != nil || fi.ID != a.ID || fi.RemovedAt == nil {
		t.Fatalf("FindInstance on evicted: %v %+v", err, fi)
	}

	// Revive: participant again, listed again.
	if err := s.ClearAgentRemoved(ctx, a.ID); err != nil {
		t.Fatalf("clear removed: %v", err)
	}
	agents, err = s.ListAgents(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, ag := range agents {
		if ag.ID == a.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("revived agent not listed")
	}
	parts, err = s.Participants(ctx, "s1")
	if err != nil {
		t.Fatalf("participants: %v", err)
	}
	found = false
	for _, p := range parts {
		if p.ID == a.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("revived agent not a participant")
	}
}

// TestAgentClientHiddenByRoles: an app-level identity ("agent client") is
// backend plumbing once it has a live role — listings and participants hide
// it (the role represents it); with no roles, or after every role is
// evicted, it is the visible fallback again.
func TestAgentClientHiddenByRoles(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	client, err := s.CreateAgent(ctx, "opencode:host:butler", "opencode:host:butler", "opencode", "host", "/w", "")
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	listed := func(id string) bool {
		agents, err := s.ListAgents(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, a := range agents {
			if a.ID == id {
				return true
			}
		}
		return false
	}

	if !listed(client.ID) {
		t.Fatal("client without roles should be listed (it is the only presence)")
	}

	role, err := s.CreateAgent(ctx, "opencode:fix-read-marks", "Fix read marks", "opencode", "host", "/w", "ses_one")
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	if _, err := s.CreateSession(ctx, "s1", "meeting", "admin", []string{client.ID, role.ID}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if listed(client.ID) {
		t.Fatal("client with a live role should be hidden from listings")
	}
	if !listed(role.ID) {
		t.Fatal("role should be listed")
	}
	parts, err := s.Participants(ctx, "s1")
	if err != nil {
		t.Fatalf("participants: %v", err)
	}
	for _, p := range parts {
		if p.ID == client.ID {
			t.Fatal("client should be hidden from participants")
		}
	}
	found := false
	for _, p := range parts {
		if p.ID == role.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("role should be a visible participant")
	}

	// Evicting the only role: the client becomes the visible fallback again.
	if err := s.RemoveAgent(ctx, role.ID); err != nil {
		t.Fatalf("remove role: %v", err)
	}
	if !listed(client.ID) {
		t.Fatal("client should be visible once every role is evicted")
	}
}
