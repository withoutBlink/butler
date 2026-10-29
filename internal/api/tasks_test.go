package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"butler/internal/api"
	"butler/internal/auth"
	"butler/internal/board"
	"butler/internal/hub"
	"butler/internal/metrics"
	"butler/internal/store"
)

func TestTaskAssignNotifiesAndWebhooks(t *testing.T) {
	srv, svc, st, _, agents := newTaskEnv(t)
	ctx := context.Background()
	if err := st.UpdateAgentDisplayName(ctx, agents["alpha"].ID, "Alpha One"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	var mu sync.Mutex
	var bodies []map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Errorf("webhook json: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()

	resp, body := req(t, "PUT", srv.URL+"/api/v1/webhooks/agents/"+agents["alpha"].ID, adminTok, map[string]any{"url": hook.URL})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set webhook: %d %v", resp.StatusCode, body)
	}

	// Kind "test" matches both alpha and beta, so it is not a single assignee.
	resp, body = req(t, "POST", srv.URL+"/api/v1/tasks", adminTok, map[string]any{
		"title": "Ambiguous", "assignee": "test",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("ambiguous assignee: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "POST", srv.URL+"/api/v1/tasks", adminTok, map[string]any{
		"id": "task-retry-1", "title": "Ship the board", "description": "Kanban plus webhooks",
		"assignee": "Alpha One", "priority": "high",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	if body["id"] != "task-retry-1" || body["assignee_id"] != agents["alpha"].ID || body["priority"] != "high" {
		t.Fatalf("created task: %v", body)
	}
	resp, again := req(t, "POST", srv.URL+"/api/v1/tasks", adminTok, map[string]any{
		"id": "task-retry-1", "title": "Ship the board",
	})
	if resp.StatusCode != http.StatusOK || again["id"] != "task-retry-1" {
		t.Fatalf("idempotent create: %d %v", resp.StatusCode, again)
	}
	svc.Hooks.Wait()

	mu.Lock()
	if len(bodies) != 1 || bodies[0]["event"] != "task.assigned" {
		t.Fatalf("webhook hits: %+v", bodies)
	}
	task, _ := bodies[0]["task"].(map[string]any)
	if task["id"] != "task-retry-1" || task["assignee_id"] != agents["alpha"].ID {
		t.Fatalf("webhook task: %+v", bodies[0])
	}
	mu.Unlock()

	resp, caught := req(t, "GET", srv.URL+"/api/v1/messages?after=0", agents["alpha"].Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("catchup: %d %v", resp.StatusCode, caught)
	}
	msgs, _ := caught["messages"].([]any)
	var mentioned bool
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m["kind"] != "chat" {
			continue
		}
		content, _ := m["content"].(string)
		if content == "" || !strings.Contains(content, "assigned task") || !strings.Contains(content, "Ship the board") || !strings.Contains(content, "task-retry-1") {
			continue
		}
		for _, id := range m["mentions"].([]any) {
			if id == agents["alpha"].ID {
				mentioned = true
			}
		}
	}
	if !mentioned {
		t.Fatalf("assignee was not mentioned: %v", caught)
	}

	// A status change is visible to the creator: the notice @mentions admin.
	resp, body = req(t, "PATCH", srv.URL+"/api/v1/tasks/task-retry-1", agents["alpha"].Token, map[string]any{"status": "in_progress"})
	if resp.StatusCode != http.StatusOK || body["status"] != "in_progress" {
		t.Fatalf("patch: %d %v", resp.StatusCode, body)
	}
	svc.Hooks.Wait()
	resp, sess := req(t, "GET", srv.URL+"/api/v1/sessions/global/messages?latest=20", agents["alpha"].Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session messages: %d %v", resp.StatusCode, sess)
	}
	if !mentionsAdmin(t, sess, "in_progress") {
		t.Fatalf("creator was not mentioned on status change: %v", sess)
	}

	resp, body = req(t, "POST", srv.URL+"/api/v1/tasks/task-retry-1/comments", agents["alpha"].Token, map[string]any{"body": "schema is in"})
	if resp.StatusCode != http.StatusCreated || body["body"] != "schema is in" {
		t.Fatalf("comment: %d %v", resp.StatusCode, body)
	}
	resp, got := req(t, "GET", srv.URL+"/api/v1/tasks/task-retry-1", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: %d %v", resp.StatusCode, got)
	}
	comments, _ := got["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("comments: %v", got)
	}

	resp, listed := req(t, "GET", srv.URL+"/api/v1/tasks?assignee=me&status=in_progress", agents["alpha"].Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d %v", resp.StatusCode, listed)
	}
	rows, _ := listed["tasks"].([]any)
	if len(rows) != 1 {
		t.Fatalf("list me: %v", listed)
	}
}

func TestWebhookRetryAndDoesNotBlock(t *testing.T) {
	srv, svc, _, _, agents := newTaskEnv(t)

	var n atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()
	resp, body := req(t, "PUT", srv.URL+"/api/v1/webhooks/agents/"+agents["alpha"].ID, adminTok, map[string]any{"url": hook.URL})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set webhook: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "POST", srv.URL+"/api/v1/tasks", adminTok, map[string]any{
		"title": "Retry me", "assignee": agents["alpha"].ID,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	svc.Hooks.Wait()
	if got := n.Load(); got != 2 {
		t.Fatalf("expected one failure and one retry, got %d hits", got)
	}

	// A slow webhook must not hold the request. The handler blocks until
	// release; the POST has to return first.
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer slow.Close()
	resp, body = req(t, "PUT", srv.URL+"/api/v1/webhooks/agents/"+agents["beta"].ID, agents["beta"].Token, map[string]any{"url": slow.URL})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("beta webhook: %d %v", resp.StatusCode, body)
	}
	// Someone else may not set it.
	resp, _ = req(t, "PUT", srv.URL+"/api/v1/webhooks/agents/"+agents["beta"].ID, agents["alpha"].Token, map[string]any{"url": slow.URL})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-agent webhook: %d", resp.StatusCode)
	}

	done := make(chan struct{})
	go func() {
		resp, body := req(t, "POST", srv.URL+"/api/v1/tasks", adminTok, map[string]any{
			"title": "Do not block", "assignee": agents["beta"].ID,
		})
		if resp.StatusCode != http.StatusCreated {
			t.Errorf("create: %d %v", resp.StatusCode, body)
		}
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("webhook was never called")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("task create blocked on the webhook")
	}
	close(release)
	svc.Hooks.Wait()
}

func TestWebhookTimeoutRetries(t *testing.T) {
	srv, svc, _, _, agents := newTaskEnv(t)
	svc.Hooks.Timeout = 150 * time.Millisecond
	var n atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		time.Sleep(time.Second)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()
	resp, body := req(t, "PUT", srv.URL+"/api/v1/webhooks/kinds/test", adminTok, map[string]any{"url": hook.URL})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("kind webhook: %d %v", resp.StatusCode, body)
	}
	start := time.Now()
	resp, body = req(t, "POST", srv.URL+"/api/v1/tasks", adminTok, map[string]any{
		"title": "Wake the kind", "assignee": agents["alpha"].ID,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	svc.Hooks.Wait()
	if got := n.Load(); got != 2 {
		t.Fatalf("expected two attempts, got %d", got)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("retries waited too long: %s", time.Since(start))
	}
}

func TestTaskClaimAndAuth(t *testing.T) {
	srv, _, _, kindTok, agents := newTaskEnv(t)
	resp, body := req(t, "POST", srv.URL+"/api/v1/tasks", adminTok, map[string]any{"title": "Unclaimed"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	id, _ := body["id"].(string)
	resp, body = req(t, "POST", srv.URL+"/api/v1/tasks/"+id+"/claim", agents["beta"].Token, nil)
	if resp.StatusCode != http.StatusOK || body["assignee_id"] != agents["beta"].ID || body["status"] != "in_progress" {
		t.Fatalf("claim: %d %v", resp.StatusCode, body)
	}
	resp, _ = req(t, "POST", srv.URL+"/api/v1/tasks", kindTok, map[string]any{"title": "nope"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("kind token: %d", resp.StatusCode)
	}
	resp, _ = req(t, "GET", srv.URL+"/api/v1/tasks", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	resp, _ = req(t, "GET", srv.URL+"/api/v1/tasks/missing", adminTok, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing: %d", resp.StatusCode)
	}
	resp, listed := req(t, "GET", srv.URL+"/api/v1/tasks?assignee=unassigned", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unassigned: %d %v", resp.StatusCode, listed)
	}
	rows, _ := listed["tasks"].([]any)
	if len(rows) != 0 {
		t.Fatalf("expected no unassigned tasks, got %v", listed)
	}
}

func TestHandoffChainReviewAndReject(t *testing.T) {
	srv, svc, st, _, agents := newTaskEnv(t)
	ctx := context.Background()
	if err := st.UpdateAgentDisplayName(ctx, agents["alpha"].ID, "Researcher"); err != nil {
		t.Fatalf("rename alpha: %v", err)
	}
	if err := st.UpdateAgentDisplayName(ctx, agents["beta"].ID, "Link"); err != nil {
		t.Fatalf("rename beta: %v", err)
	}
	var mu sync.Mutex
	var bodies []map[string]any
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			mu.Lock()
			bodies = append(bodies, m)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()
	resp, body := req(t, "PUT", srv.URL+"/api/v1/webhooks/agents/"+agents["beta"].ID, adminTok, map[string]any{"url": hook.URL})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "POST", srv.URL+"/api/v1/tasks", adminTok, map[string]any{
		"title":    "Walk the chain",
		"assignee": "Researcher",
		"chain":    []string{"Researcher", "Link"},
		"reviewer": "admin",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	id, _ := body["id"].(string)
	chain, _ := body["chain"].([]any)
	if body["assignee_id"] != agents["alpha"].ID || len(chain) != 2 || body["reviewer_id"] != "admin" {
		t.Fatalf("create shape: %v", body)
	}

	// Someone who does not own it cannot hand it off.
	resp, body = req(t, "POST", srv.URL+"/api/v1/tasks/"+id+"/handoff", agents["beta"].Token, map[string]any{})
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body["error"].(string), "current owner") {
		t.Fatalf("non-owner handoff: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "POST", srv.URL+"/api/v1/tasks/"+id+"/handoff", agents["alpha"].Token, map[string]any{"note": "sources are in"})
	if resp.StatusCode != http.StatusOK || body["assignee_id"] != agents["beta"].ID || body["status"] != "todo" {
		t.Fatalf("handoff to link: %d %v", resp.StatusCode, body)
	}
	svc.Hooks.Wait()
	mu.Lock()
	if len(bodies) != 1 || bodies[0]["event"] != "task.assigned" || bodies[0]["handoff"] != true {
		t.Fatalf("handoff webhook: %+v", bodies)
	}
	mu.Unlock()

	resp, got := req(t, "GET", srv.URL+"/api/v1/tasks/"+id, adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: %d %v", resp.StatusCode, got)
	}
	comments, _ := got["comments"].([]any)
	if len(comments) != 1 || !strings.Contains(comments[0].(map[string]any)["body"].(string), "Researcher handed off to Link: sources are in") {
		t.Fatalf("log: %v", got["comments"])
	}
	resp, caught := req(t, "GET", srv.URL+"/api/v1/messages?after=0", agents["beta"].Token, nil)
	if resp.StatusCode != http.StatusOK || !mentionsID(t, caught, agents["beta"].ID, "handed off") {
		t.Fatalf("link was not mentioned: %v", caught)
	}

	resp, body = req(t, "PATCH", srv.URL+"/api/v1/tasks/"+id, agents["beta"].Token, map[string]any{"status": "done"})
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body["error"].(string), "hand it off for review") {
		t.Fatalf("owner marking done: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "POST", srv.URL+"/api/v1/tasks/"+id+"/handoff", agents["beta"].Token, map[string]any{})
	if resp.StatusCode != http.StatusOK || body["status"] != "review" || body["assignee_id"] != "admin" || body["review_from_id"] != agents["beta"].ID {
		t.Fatalf("handoff to review: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "POST", srv.URL+"/api/v1/tasks/"+id+"/handoff", adminTok, map[string]any{})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body["error"].(string), "in review") {
		t.Fatalf("bare handoff during review: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "POST", srv.URL+"/api/v1/tasks/"+id+"/reject", adminTok, map[string]any{"note": "needs a source"})
	if resp.StatusCode != http.StatusOK || body["status"] != "todo" || body["assignee_id"] != agents["beta"].ID {
		t.Fatalf("reject: %d %v", resp.StatusCode, body)
	}
	resp, got = req(t, "GET", srv.URL+"/api/v1/tasks/"+id, adminTok, nil)
	comments, _ = got["comments"].([]any)
	last := comments[len(comments)-1].(map[string]any)["body"].(string)
	if !strings.Contains(last, "Admin sent this back to Link: needs a source") {
		t.Fatalf("reject log: %q", last)
	}

	resp, body = req(t, "POST", srv.URL+"/api/v1/tasks/"+id+"/handoff", agents["beta"].Token, map[string]any{})
	if resp.StatusCode != http.StatusOK || body["status"] != "review" {
		t.Fatalf("handoff back to review: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "POST", srv.URL+"/api/v1/tasks/"+id+"/approve", agents["beta"].Token, map[string]any{})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-reviewer approve: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "POST", srv.URL+"/api/v1/tasks/"+id+"/approve", adminTok, nil)
	if resp.StatusCode != http.StatusOK || body["status"] != "done" {
		t.Fatalf("approve: %d %v", resp.StatusCode, body)
	}
}

func mentionsID(t *testing.T, body map[string]any, id, needle string) bool {
	t.Helper()
	msgs, _ := body["messages"].([]any)
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		content, _ := m["content"].(string)
		if m["kind"] != "chat" || !strings.Contains(content, needle) {
			continue
		}
		ids, _ := m["mentions"].([]any)
		for _, mid := range ids {
			if mid == id {
				return true
			}
		}
	}
	return false
}

func mentionsAdmin(t *testing.T, body map[string]any, needle string) bool {
	t.Helper()
	msgs, _ := body["messages"].([]any)
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		content, _ := m["content"].(string)
		if m["kind"] != "chat" || !strings.Contains(content, needle) {
			continue
		}
		for _, id := range m["mentions"].([]any) {
			if id == "admin" {
				return true
			}
		}
	}
	return false
}

// newTaskEnv is newTestAPI plus the global session `butler serve` creates,
// and a handle on the board service so tests can wait for webhooks.
func newTaskEnv(t *testing.T) (*httptest.Server, *board.Service, *store.Store, string, map[string]testAgent) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "butler.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		st.Close()
	})
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatalf("admin: %v", err)
	}
	if _, err := st.EnsureGlobalSession(ctx, "admin"); err != nil {
		t.Fatalf("global: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := hub.New()
	svc := board.New(st, h, log)
	a := &api.API{
		Store:      st,
		Hub:        h,
		AdminToken: adminTok,
		Log:        log,
		Metrics:    &metrics.Metrics{},
		Done:       done,
		Board:      svc,
	}
	kindTok := auth.NewToken()
	if _, err := st.CreateKind(ctx, "test", "Test", auth.Hash(kindTok)); err != nil {
		t.Fatalf("create kind: %v", err)
	}
	srv := httptest.NewServer(a.Routes())
	t.Cleanup(srv.Close)
	agents := map[string]testAgent{}
	for _, host := range []string{"alpha", "beta"} {
		id, tok := registerAgent(t, srv, kindTok, host, "/tmp/"+host)
		agents[host] = testAgent{ID: id, Token: tok}
	}
	return srv, svc, st, kindTok, agents
}
