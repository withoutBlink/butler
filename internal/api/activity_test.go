package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"butler/internal/api"
	"butler/internal/auth"
	"butler/internal/hub"
	"butler/internal/metrics"
	"butler/internal/store"
)

func TestActivityReportAndStall(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "butler.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	h := hub.New()
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		h.Close()
		st.Close()
	})
	a := &api.API{
		Store: st, Hub: h, AdminToken: adminTok,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: &metrics.Metrics{}, Done: done,
	}
	kindTok := auth.NewToken()
	if _, err := st.CreateKind(context.Background(), "test", "Test", auth.Hash(kindTok)); err != nil {
		t.Fatalf("kind: %v", err)
	}
	srv := httptest.NewServer(a.Routes())
	t.Cleanup(srv.Close)
	id, tok := registerAgent(t, srv, kindTok, "alpha", "/tmp/alpha")

	seqBefore := latestSeq(t, srv.URL, tok)

	resp, body := req(t, "POST", srv.URL+"/api/v1/agents/activity", tok, map[string]any{
		"state": "blocked", "note": "waiting on the diff",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set activity: %d %v", resp.StatusCode, body)
	}
	act, _ := body["activity"].(map[string]any)
	if act["state"] != "blocked" || act["note"] != "waiting on the diff" {
		t.Fatalf("report: %v", body)
	}
	if latestSeq(t, srv.URL, tok) != seqBefore {
		t.Fatal("set_activity must not post a message")
	}

	resp, body = req(t, "GET", srv.URL+"/api/v1/agents", tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: %d %v", resp.StatusCode, body)
	}
	var found bool
	for _, raw := range body["agents"].([]any) {
		m := raw.(map[string]any)
		if m["id"] != id {
			continue
		}
		found = true
		got, _ := m["activity"].(map[string]any)
		if got["state"] != "blocked" || got["note"] != "waiting on the diff" {
			t.Fatalf("listed activity: %v", m)
		}
		if m["online"] != true {
			t.Fatalf("a fresh report should leave the agent online: %v", m)
		}
	}
	if !found {
		t.Fatalf("agent missing: %v", body)
	}

	resp, body = req(t, "POST", srv.URL+"/api/v1/agents/activity", tok, map[string]any{"state": "asleep"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad state: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "POST", srv.URL+"/api/v1/agents/activity", tok, map[string]any{
		"state": "idle", "note": string(make([]byte, 201)),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("long note: %d %v", resp.StatusCode, body)
	}
	resp, _ = req(t, "POST", srv.URL+"/api/v1/agents/activity", kindTok, map[string]any{"state": "idle"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("kind token: %d", resp.StatusCode)
	}
	resp, _ = req(t, "POST", srv.URL+"/api/v1/agents/activity", adminTok, map[string]any{"state": "idle"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("admin token: %d", resp.StatusCode)
	}

	resp, body = req(t, "POST", srv.URL+"/api/v1/tasks", tok, map[string]any{
		"title": "Do the thing", "assignee": id, "status": "in_progress",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create task: %d %v", resp.StatusCode, body)
	}
	taskID, _ := body["id"].(string)
	if body["stalled"] != false || body["assignee_activity"] != store.ActivityWorking {
		t.Fatalf("fresh claim should be working, not stalled: %v", body)
	}
	resp, body = req(t, "GET", srv.URL+"/api/v1/tasks/"+taskID, adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get task: %d %v", resp.StatusCode, body)
	}
	comments, _ := body["comments"].([]any)
	if len(comments) != 0 {
		t.Fatalf("activity must not comment on the task: %v", comments)
	}

	past := time.Now().Add(-10 * time.Minute).Unix()
	if err := st.SetContactTimes(context.Background(), id, &past, &past); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	// The explicit report is still fresh and would say blocked, but the owner
	// has gone quiet, so the claimed task wins as stalled.
	if err := st.SetActivity(context.Background(), id, store.ActivityWorking, "still on it"); err != nil {
		t.Fatalf("re-report: %v", err)
	}
	if err := st.SetContactTimes(context.Background(), id, &past, &past); err != nil {
		t.Fatalf("backdate again: %v", err)
	}
	resp, body = req(t, "GET", srv.URL+"/api/v1/tasks/"+taskID, adminTok, nil)
	if resp.StatusCode != http.StatusOK || body["stalled"] != true || body["assignee_activity"] != store.ActivityStalled {
		t.Fatalf("quiet in_progress task: %v", body)
	}

	// A held long-poll is a heartbeat: backdating the clock while it is open
	// must not stall the task.
	waitCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitDone := make(chan struct{})
	go func() {
		defer close(waitDone)
		req, err := http.NewRequestWithContext(waitCtx, "GET", srv.URL+"/api/v1/wait?timeout=30", nil)
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !h.WaitingAgents()[id] {
		if time.Now().After(deadline) {
			t.Fatal("wait did not subscribe")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := st.SetContactTimes(context.Background(), id, &past, &past); err != nil {
		t.Fatalf("backdate during wait: %v", err)
	}
	resp, body = req(t, "GET", srv.URL+"/api/v1/tasks/"+taskID, adminTok, nil)
	if resp.StatusCode != http.StatusOK || body["stalled"] != false || body["assignee_activity"] != store.ActivityWorking {
		t.Fatalf("held wait should keep the task moving: stalled=%v activity=%v", body["stalled"], body["assignee_activity"])
	}
	cancel()
	<-waitDone
}
