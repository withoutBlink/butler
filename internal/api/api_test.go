package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"butler/internal/api"
	"butler/internal/auth"
	"butler/internal/hub"
	"butler/internal/metrics"
	"butler/internal/store"
)

const adminTok = "test-admin-token-0123456789"

func newTestAPI(t *testing.T) (*httptest.Server, string, map[string]string) {
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
	a := &api.API{
		Store:      st,
		Hub:        hub.New(),
		AdminToken: adminTok,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:    &metrics.Metrics{},
		Done:       done,
	}
	tokens := map[string]string{}
	for _, id := range []string{"alpha", "beta", "gamma"} {
		tok := auth.NewToken()
		if _, err := st.CreateAgent(context.Background(), id, id, "test", auth.Hash(tok)); err != nil {
			t.Fatalf("create agent %s: %v", id, err)
		}
		tokens[id] = tok
	}
	srv := httptest.NewServer(a.Routes())
	t.Cleanup(srv.Close)
	return srv, adminTok, tokens
}

func req(t *testing.T, method, url, token string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	httpReq, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	m := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal %q: %v", raw, err)
		}
	}
	return resp, m
}

func TestSessionMessageFlow(t *testing.T) {
	srv, _, tokens := newTestAPI(t)
	base := srv.URL

	resp, body := req(t, "POST", base+"/api/v1/sessions", tokens["alpha"], map[string]any{
		"title":        "build feature",
		"participants": []string{"beta"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create session: %d %v", resp.StatusCode, body)
	}
	sid := body["id"].(string)

	resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/messages", tokens["alpha"], map[string]any{"content": "hello beta"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post message: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "GET", base+"/api/v1/wait?after=0&timeout=2", tokens["beta"], nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wait: %d %v", resp.StatusCode, body)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %v", msgs)
	}
	first := msgs[0].(map[string]any)
	if first["content"] != "hello beta" || first["sender_id"] != "alpha" || first["kind"] != "chat" {
		t.Fatalf("bad message: %v", first)
	}
	latest := int64(body["latest_seq"].(float64))

	start := time.Now()
	resp, body = req(t, "GET", base+"/api/v1/wait?after="+strconv.FormatInt(latest, 10)+"&timeout=1", tokens["beta"], nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("timed wait: %d %v", resp.StatusCode, body)
	}
	if len(body["messages"].([]any)) != 0 {
		t.Fatalf("expected empty messages, got %v", body["messages"])
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatalf("wait returned too fast: %v", time.Since(start))
	}

	resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/messages", tokens["gamma"], map[string]any{"content": "hi"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("gamma should not be able to post: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/join", tokens["gamma"], nil)
	if resp.StatusCode != http.StatusOK || body["joined"] != true {
		t.Fatalf("join: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/join", tokens["gamma"], nil)
	if resp.StatusCode != http.StatusOK || body["joined"] != false {
		t.Fatalf("second join: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "GET", base+"/api/v1/wait?after=0&timeout=2", tokens["gamma"], nil)
	msgs = body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages for gamma, got %d", len(msgs))
	}
	sys := msgs[1].(map[string]any)
	if sys["kind"] != "system" || sys["sender_id"] != "gamma" {
		t.Fatalf("bad system message: %v", sys)
	}

	resp, body = req(t, "PUT", base+"/api/v1/sessions/"+sid+"/context/deploy_url", tokens["beta"], map[string]any{"value": "http://x"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put context: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "GET", base+"/api/v1/sessions/"+sid+"/context/deploy_url", tokens["beta"], nil)
	if resp.StatusCode != http.StatusOK || body["value"] != "http://x" {
		t.Fatalf("get context: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "GET", base+"/api/v1/messages?after=0", tokens["beta"], nil)
	msgs = body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("catchup expected 3 messages (chat, join, context), got %d", len(msgs))
	}
}

func TestAuthAndAdmin(t *testing.T) {
	srv, adminTok, tokens := newTestAPI(t)
	base := srv.URL

	resp, _ := req(t, "GET", base+"/api/v1/sessions", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	resp, _ = req(t, "GET", base+"/api/v1/sessions", "bogus", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", resp.StatusCode)
	}

	resp, body := req(t, "POST", base+"/api/v1/agents", tokens["alpha"], map[string]any{"id": "evil"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("agent create by non-admin: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "POST", base+"/api/v1/agents", adminTok, map[string]any{"id": "codex", "kind": "codex"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("agent create: %d %v", resp.StatusCode, body)
	}
	newTok, _ := body["token"].(string)
	if newTok == "" {
		t.Fatalf("missing token: %v", body)
	}

	resp, body = req(t, "GET", base+"/api/v1/agents", newTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list agents: %d", resp.StatusCode)
	}
	found := false
	for _, a := range body["agents"].([]any) {
		if a.(map[string]any)["id"] == "codex" {
			found = true
		}
	}
	if !found {
		t.Fatalf("codex missing from list: %v", body)
	}

	resp, body = req(t, "POST", base+"/api/v1/agents/codex/rotate", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate: %d %v", resp.StatusCode, body)
	}
	rotated, _ := body["token"].(string)
	resp, _ = req(t, "GET", base+"/api/v1/sessions", newTok, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old token should fail: %d", resp.StatusCode)
	}
	resp, _ = req(t, "GET", base+"/api/v1/sessions", rotated, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotated token should work: %d", resp.StatusCode)
	}

	resp, _ = req(t, "GET", base+"/api/v1/wait?timeout=1", adminTok, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("admin wait: %d", resp.StatusCode)
	}

	resp, _ = req(t, "GET", base+"/metrics", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("metrics unauth: %d", resp.StatusCode)
	}
	httpReq, err := http.NewRequest("GET", base+"/metrics", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+adminTok)
	metricsResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	metricsBody, err := io.ReadAll(metricsResp.Body)
	metricsResp.Body.Close()
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	if metricsResp.StatusCode != http.StatusOK {
		t.Fatalf("metrics: %d", metricsResp.StatusCode)
	}
	if !bytes.Contains(metricsBody, []byte("butler_http_requests_total")) {
		t.Fatalf("unexpected metrics body: %q", metricsBody)
	}

	resp, _ = req(t, "GET", base+"/healthz", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
}

func TestIdempotentMessagePost(t *testing.T) {
	srv, _, tokens := newTestAPI(t)
	base := srv.URL
	_, body := req(t, "POST", base+"/api/v1/sessions", tokens["alpha"], map[string]any{"title": "t"})
	sid := body["id"].(string)
	msgBody := map[string]any{"id": "fixed-msg-id", "content": "same"}
	resp, first := req(t, "POST", base+"/api/v1/sessions/"+sid+"/messages", tokens["alpha"], msgBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first post: %d %v", resp.StatusCode, first)
	}
	resp, second := req(t, "POST", base+"/api/v1/sessions/"+sid+"/messages", tokens["alpha"], msgBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("retry post: %d %v", resp.StatusCode, second)
	}
	if first["seq"].(float64) != second["seq"].(float64) {
		t.Fatalf("seq mismatch: %v vs %v", first["seq"], second["seq"])
	}
	_, body = req(t, "GET", base+"/api/v1/sessions/"+sid+"/messages", tokens["alpha"], nil)
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
}

func TestArchiveBlocksPosting(t *testing.T) {
	srv, _, tokens := newTestAPI(t)
	base := srv.URL
	_, body := req(t, "POST", base+"/api/v1/sessions", tokens["alpha"], map[string]any{"title": "t"})
	sid := body["id"].(string)
	resp, body := req(t, "PATCH", base+"/api/v1/sessions/"+sid, tokens["alpha"], map[string]any{"status": "archived"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("archive: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/messages", tokens["alpha"], map[string]any{"content": "hi"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("post to archived: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "PATCH", base+"/api/v1/sessions/"+sid, tokens["beta"], map[string]any{"status": "active"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-creator patch: %d %v", resp.StatusCode, body)
	}
}
