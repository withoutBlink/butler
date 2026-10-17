package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
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
	mustAgent(t, s, "c")
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
	// An archived session is frozen: no new participants, and no join notice
	// written into it (the panel and the API both surface this as a 409).
	if err := s.SetSessionStatus(ctx, "s1", "archived"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if joined, err := s.JoinSession(ctx, "s1", "c"); !errors.Is(err, ErrConflict) || joined {
		t.Fatalf("joining an archived session should conflict: %v %v", joined, err)
	}
	if ok, err := s.IsParticipant(ctx, "s1", "c"); err != nil || ok {
		t.Fatalf("a refused join must not add a participant: %v %v", ok, err)
	}
	msgs, _ = s.MessagesSince(ctx, "", "s1", 0, 10)
	if len(msgs) != 1 {
		t.Fatalf("a refused join must not write a notice, got %d messages", len(msgs))
	}
	if _, err := s.JoinSession(ctx, "nope", "b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

// Server-generated notices name the agent the way humans see it: the display
// title (a role's client-session title), with the id as the fallback for an
// untitled identity — app-level identities carry their own id as label by
// design, so for them both forms are the same string.
func TestJoinNoticeUsesDisplayName(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mustAgent(t, s, "admin")
	const role = "2ad4443c-a473-4af8-a781-4b483d84d8d3"
	if _, err := s.CreateAgent(ctx, role, "Develop butler", "opencode", "h", "/w", "ses_dev"); err != nil {
		t.Fatalf("create role: %v", err)
	}
	if _, err := s.CreateAgent(ctx, "untitled", "", "opencode", "h", "/w", "ses_bare"); err != nil {
		t.Fatalf("create untitled: %v", err)
	}
	if _, err := s.CreateSession(ctx, "s1", "meeting", "admin", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	for _, id := range []string{role, "untitled"} {
		if _, err := s.JoinSession(ctx, "s1", id); err != nil {
			t.Fatalf("join %s: %v", id, err)
		}
	}
	msgs, err := s.SessionLatestMessages(ctx, "s1", 10)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(msgs) != 2 || msgs[0].SenderID != role || msgs[0].Content != "Develop butler joined the session" {
		t.Fatalf("notice should carry the display name: %+v", msgs)
	}
	if msgs[1].SenderID != "untitled" || msgs[1].Content != "untitled joined the session" {
		t.Fatalf("untitled identity should fall back to its id: %+v", msgs)
	}
}

// Deleting a session takes its messages, participants, context items and read
// cursors with it and reports who was in it (so the caller can wake those
// wait loops); the fixed global session refuses to be deleted or archived.
func TestDeleteSession(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mustAgent(t, s, "a")
	mustAgent(t, s, "b")
	if _, err := s.CreateSession(ctx, "s1", "doomed", "a", []string{"b"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	msg, err := s.InsertMessage(ctx, &Message{SessionID: "s1", SenderID: "a", Kind: "chat", Content: "hi"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, _, err := s.UpsertContext(ctx, "s1", "k", "v", "a"); err != nil {
		t.Fatalf("context: %v", err)
	}
	if err := s.MarkRead(ctx, "s1", "b", msg.Seq); err != nil {
		t.Fatalf("mark read: %v", err)
	}

	parts, err := s.DeleteSession(ctx, "s1")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("delete should report the participants, got %v", parts)
	}
	if _, err := s.GetSession(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session should be gone: %v", err)
	}
	if msgs, err := s.SessionLatestMessages(ctx, "s1", 10); err != nil || len(msgs) != 0 {
		t.Fatalf("messages should be gone: %v %v", msgs, err)
	}
	if marks, err := s.ReadMarks(ctx, "s1"); err != nil || len(marks) != 0 {
		t.Fatalf("read marks should be gone: %v %v", marks, err)
	}
	if items, err := s.ListContext(ctx, "s1"); err != nil || len(items) != 0 {
		t.Fatalf("context should be gone: %v %v", items, err)
	}
	if ps, err := s.Participants(ctx, "s1"); err != nil || len(ps) != 0 {
		t.Fatalf("participants should be gone: %v %v", ps, err)
	}
	if _, err := s.DeleteSession(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete should be not found: %v", err)
	}

	// The global session is fixed: no delete, no archive, and only the two
	// documented statuses are accepted.
	if _, err := s.EnsureGlobalSession(ctx, "a"); err != nil {
		t.Fatalf("global session: %v", err)
	}
	if _, err := s.DeleteSession(ctx, GlobalSessionID); !errors.Is(err, ErrConflict) {
		t.Fatalf("deleting the global session should be refused: %v", err)
	}
	if err := s.SetSessionStatus(ctx, GlobalSessionID, "archived"); !errors.Is(err, ErrConflict) {
		t.Fatalf("archiving the global session should be refused: %v", err)
	}
	if err := s.SetSessionStatus(ctx, "s1", "closed"); !errors.Is(err, ErrConflict) {
		t.Fatalf("an unknown status should be refused: %v", err)
	}
}

// Removing a participant is the mirror of joining: the row goes, the agent's
// read cursor for that session goes, a notice names the actor, and nothing
// else about the agent changes. Membership changes only happen on active
// sessions, and the global session is only protected from archive/delete.
func TestRemoveParticipant(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, id := range []string{"admin", "a", "b", "c"} {
		mustAgent(t, s, id)
	}
	if _, err := s.CreateSession(ctx, "s1", "one", "a", []string{"b", "c"}); err != nil {
		t.Fatalf("create s1: %v", err)
	}
	if _, err := s.CreateSession(ctx, "s2", "two", "a", []string{"b"}); err != nil {
		t.Fatalf("create s2: %v", err)
	}
	msg, err := s.InsertMessage(ctx, &Message{SessionID: "s1", SenderID: "a", Kind: "chat", Content: "hello"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.MarkRead(ctx, "s1", "b", msg.Seq); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if err := s.MarkRead(ctx, "s2", "b", msg.Seq); err != nil {
		t.Fatalf("mark read s2: %v", err)
	}

	if err := s.RemoveParticipant(ctx, "s1", "b", "admin"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if ok, err := s.IsParticipant(ctx, "s1", "b"); err != nil || ok {
		t.Fatalf("b should be out of s1: %v %v", ok, err)
	}
	if ok, err := s.IsParticipant(ctx, "s2", "b"); err != nil || !ok {
		t.Fatalf("b must keep its other sessions: %v %v", ok, err)
	}
	if marks, err := s.ReadMarks(ctx, "s1"); err != nil || len(marks) != 0 {
		t.Fatalf("the removed agent's cursor for s1 should be gone: %v %v", marks, err)
	}
	if marks, err := s.ReadMarks(ctx, "s2"); err != nil || marks["b"] != msg.Seq {
		t.Fatalf("other sessions' cursors must survive: %v %v", marks, err)
	}
	// The notice names the subject and the actor by display title (here both
	// are their ids: the test agents are untitled).
	msgs, err := s.MessagesSince(ctx, "", "s1", 0, 20)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	last := msgs[len(msgs)-1]
	if last.Kind != "system" || last.Content != "b was removed from the session by admin" || last.SenderID != "b" {
		t.Fatalf("removal notice: %+v", last)
	}

	// A self-removal reads as leaving.
	if err := s.RemoveParticipant(ctx, "s1", "c", "c"); err != nil {
		t.Fatalf("leave: %v", err)
	}
	msgs, _ = s.MessagesSince(ctx, "", "s1", 0, 20)
	if last := msgs[len(msgs)-1]; last.Content != "c left the session" {
		t.Fatalf("leave notice: %+v", last)
	}

	// Unknown membership, unknown session.
	if err := s.RemoveParticipant(ctx, "s1", "admin", "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-participant should be not found: %v", err)
	}
	if err := s.RemoveParticipant(ctx, "nope", "a", "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session should be not found: %v", err)
	}

	// Archived sessions are frozen: no membership change either way.
	if err := s.SetSessionStatus(ctx, "s2", "archived"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if err := s.RemoveParticipant(ctx, "s2", "b", "admin"); !errors.Is(err, ErrConflict) {
		t.Fatalf("removing from an archived session should conflict: %v", err)
	}
	if ok, err := s.IsParticipant(ctx, "s2", "b"); err != nil || !ok {
		t.Fatalf("the refused removal must not drop the participant: %v %v", ok, err)
	}
	if _, err := s.JoinSession(ctx, "s2", "c"); !errors.Is(err, ErrConflict) {
		t.Fatalf("joining an archived session should still conflict: %v", err)
	}

	// Only archive/delete are refused for the fixed global session — leaving
	// it is a membership change like any other (registration re-joins).
	if _, err := s.EnsureGlobalSession(ctx, "admin"); err != nil {
		t.Fatalf("global: %v", err)
	}
	if ok, err := s.JoinSession(ctx, GlobalSessionID, "c"); err != nil || !ok {
		t.Fatalf("join global: %v %v", ok, err)
	}
	if err := s.RemoveParticipant(ctx, GlobalSessionID, "c", "c"); err != nil {
		t.Fatalf("leaving the global session should work: %v", err)
	}
	if ok, err := s.IsParticipant(ctx, GlobalSessionID, "c"); err != nil || ok {
		t.Fatalf("c should be out of global: %v %v", ok, err)
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
	// @all includes every participant except the author (mentions address
	// who should reply, and the human "admin" is the one posting here).
	wantMentions(post("@all heads up"), "codex:alpha:proj", "codex:beta:proj", "opencode:host:repo")
	// A kind only matches participants of that kind.
	wantMentions(post("@opencode ping"), "opencode:host:repo")
	// Non-participants and unknown tokens resolve to nothing.
	wantMentions(post("@ghost hello"))
	// Mixed, deduped in order of first appearance.
	wantMentions(post("@all @codex @opencode:host:repo check @codex again"),
		"codex:alpha:proj", "codex:beta:proj", "opencode:host:repo")
	// No @ at all.
	wantMentions(post("plain message"))

	// The author never mentions itself: an agent quoting its own id (or
	// title, or a kind it belongs to) is not addressing itself — otherwise
	// its own wait output would flag "(mentioned you)" on its own words.
	if _, err := s.InsertMessage(ctx, &Message{SessionID: "s1", SenderID: "codex:alpha:proj", Kind: "chat",
		Content: "@codex:alpha:proj @codex self note"}); err != nil {
		t.Fatalf("insert self mention: %v", err)
	}
	self, err := s.SessionLatestMessages(ctx, "s1", 1)
	if err != nil || len(self) != 1 {
		t.Fatalf("latest: %v %d", err, len(self))
	}
	if !reflect.DeepEqual(self[0].Mentions, []string{"codex:beta:proj"}) {
		t.Fatalf("self mention resolved to %v, want [codex:beta:proj]", self[0].Mentions)
	}

	// Mentions persist and come back on read.
	msgs, err := s.MessagesSince(ctx, "", "s1", 0, 100)
	if err != nil {
		t.Fatalf("messages since: %v", err)
	}
	if len(msgs) != 8 {
		t.Fatalf("expected 8 messages, got %d", len(msgs))
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
	// The creation notice carries a millisecond timestamp like every other
	// message (the panel formats timestamps with UnixMilli; seconds would
	// render as a 1970 clock time).
	if msgs, err := s.SessionLatestMessages(ctx, GlobalSessionID, 1); err != nil || len(msgs) != 1 {
		t.Fatalf("global creation notice: %v %v", err, msgs)
	} else if msgs[0].CreatedAt < 1_000_000_000_000 {
		t.Fatalf("creation notice created_at %d is not milliseconds", msgs[0].CreatedAt)
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

// TestSessionAudience: the read-mark audience carries display names, kinds and
// the message seq each member's membership starts at. Creation-time members
// (the creator, participants passed to CreateSession) have no join notice, so
// they are present from the start (0); a member added later is pinned to its
// join notice's seq, and an evicted member drops out entirely.
func TestSessionAudience(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateAgent(ctx, "admin", "admin", "human", "", "", ""); err != nil {
		t.Fatalf("create human: %v", err)
	}
	mustAgent(t, s, "a1")
	mustAgent(t, s, "a2")
	if _, err := s.CreateSession(ctx, "s1", "audience", "admin", []string{"a1"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	// A chat and a late join: the late member's watermark is the join notice.
	if _, err := s.InsertMessage(ctx, &Message{SessionID: "s1", SenderID: "admin", Kind: "chat", Content: "hello"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := s.JoinSession(ctx, "s1", "a2"); err != nil {
		t.Fatalf("join a2: %v", err)
	}
	notice, err := s.SessionLatestMessages(ctx, "s1", 1)
	if err != nil || len(notice) != 1 {
		t.Fatalf("latest message: %v %v", err, notice)
	}
	joinedAt := notice[0].Seq

	aud, err := s.SessionAudience(ctx, "s1")
	if err != nil {
		t.Fatalf("audience: %v", err)
	}
	byID := map[string]AudienceMember{}
	for _, m := range aud {
		byID[m.ID] = m
	}
	if len(aud) != 3 {
		t.Fatalf("expected 3 members, got %v", aud)
	}
	if m := byID["admin"]; m.Kind != "human" || m.JoinedSeq != 0 {
		t.Fatalf("human should be present from the start, got %+v", m)
	}
	if m := byID["a1"]; m.JoinedSeq != 0 {
		t.Fatalf("creation-time participant should have watermark 0, got %+v", m)
	}
	if m := byID["a2"]; m.JoinedSeq != joinedAt || joinedAt < 2 {
		t.Fatalf("late joiner should be pinned to its join notice seq %d, got %+v", joinedAt, m)
	}
	// Display names, not ids, when the member is titled; order follows them
	// (a2, admin, Alpha bot — case-insensitive).
	if err := s.UpdateAgentDisplayName(ctx, "a1", "Alpha bot"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	aud, err = s.SessionAudience(ctx, "s1")
	if err != nil {
		t.Fatalf("audience: %v", err)
	}
	if len(aud) != 3 || aud[2].ID != "a1" || aud[2].DisplayName != "Alpha bot" {
		t.Fatalf("audience should carry titles and order by them, got %v", aud)
	}

	// An evicted member is not part of the audience anymore.
	if err := s.RemoveAgent(ctx, "a2"); err != nil {
		t.Fatalf("evict: %v", err)
	}
	aud, err = s.SessionAudience(ctx, "s1")
	if err != nil {
		t.Fatalf("audience: %v", err)
	}
	if len(aud) != 2 {
		t.Fatalf("evicted member should drop out, got %v", aud)
	}

	// An agent client with a live role is hidden everywhere the panel looks —
	// the audience included: the role stands for it, and the client itself has
	// no delivery of its own to report.
	if _, err := s.CreateAgent(ctx, "opencode:host:butler", "opencode:host:butler", "opencode", "host", "/w", ""); err != nil {
		t.Fatalf("create client: %v", err)
	}
	if _, err := s.CreateAgent(ctx, "role-1", "Fix read marks", "opencode", "host", "/w", "ses_1"); err != nil {
		t.Fatalf("create role: %v", err)
	}
	for _, id := range []string{"opencode:host:butler", "role-1"} {
		if _, err := s.JoinSession(ctx, "s1", id); err != nil {
			t.Fatalf("join %s: %v", id, err)
		}
	}
	aud, err = s.SessionAudience(ctx, "s1")
	if err != nil {
		t.Fatalf("audience: %v", err)
	}
	has := func(id string) bool {
		for _, m := range aud {
			if m.ID == id {
				return true
			}
		}
		return false
	}
	if len(aud) != 3 || has("opencode:host:butler") || !has("role-1") {
		t.Fatalf("a hidden client must not be in the audience, got %v", aud)
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

// TestParticipantsAlphabetical: participant and agent lists are ordered by
// display name, case-insensitively — ids are UUIDs now, so id order would
// look random in the panel.
func TestParticipantsAlphabetical(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, a := range []struct{ id, display string }{
		{"id-zeta", "zeta"},
		{"id-alpha", "Alpha"},
		{"id-beta", "beta"},
	} {
		if _, err := s.CreateAgent(ctx, a.id, a.display, "test", "", "", ""); err != nil {
			t.Fatalf("create %s: %v", a.id, err)
		}
	}
	if _, err := s.CreateSession(ctx, "s1", "order", "id-zeta", []string{"id-zeta", "id-alpha", "id-beta"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	names := func(as []Agent) []string {
		out := []string{}
		for _, a := range as {
			out = append(out, a.DisplayName)
		}
		return out
	}
	want := []string{"Alpha", "beta", "zeta"}
	parts, err := s.Participants(ctx, "s1")
	if err != nil {
		t.Fatalf("participants: %v", err)
	}
	if got := names(parts); !reflect.DeepEqual(got, want) {
		t.Fatalf("participants order: %v, want %v", got, want)
	}
	agents, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatalf("list agents: %v", err)
	}
	if got := names(agents); !reflect.DeepEqual(got, want) {
		t.Fatalf("agents order: %v, want %v", got, want)
	}
}

// TestMentionByDisplayName: the panel's @autocomplete inserts display titles
// ("@Develop butler"), so mentions resolve by display name as well —
// case-insensitively, spaces allowed, the longest name at an "@" wins, and
// only for agents that are participants.
func TestMentionByDisplayName(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	mustAgent(t, s, "admin")
	for _, r := range []struct{ id, display string }{
		{"role-a", "Alpha"},
		{"role-b", "Beta 2"},
		{"role-c", "Beta"},
		{"role-d", "Outside"},
	} {
		if _, err := s.CreateAgent(ctx, r.id, r.display, "opencode", "h", "/w", "ses_"+r.id); err != nil {
			t.Fatalf("create %s: %v", r.id, err)
		}
	}
	if _, err := s.CreateSession(ctx, "s1", "room", "admin", []string{"role-a", "role-b", "role-c"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	mentions := func(content string) []string {
		t.Helper()
		m, err := s.InsertMessage(ctx, &Message{SessionID: "s1", SenderID: "admin", Kind: "chat", Content: content})
		if err != nil {
			t.Fatalf("insert %q: %v", content, err)
		}
		return m.Mentions
	}
	cases := []struct {
		content string
		want    []string
	}{
		{"@Alpha please take a look", []string{"role-a"}},
		{"hey @alpha and @Beta hi", []string{"role-a", "role-c"}},
		{"@Beta 2 please", []string{"role-b"}}, // longest name wins
		{"@Beta hi", []string{"role-c"}},
		{"@Outside hello", nil},                     // not a participant
		{"@role-a direct", []string{"role-a"}},      // ids still work
		{"mail a@alpha.example", nil},               // mid-word "@" is no mention
		{`quote \@alpha and \@all as prose`, nil},   // escaped "@" is literal
		{`\@Alpha, then @Beta`, []string{"role-c"}}, // only the real one fires
		{`\\@alpha`, []string{"role-a"}},            // escaped backslash: real mention
	}
	for _, tc := range cases {
		if got := mentions(tc.content); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("mentions(%q) = %v, want %v", tc.content, got, tc.want)
		}
	}
}
