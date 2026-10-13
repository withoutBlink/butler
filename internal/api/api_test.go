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

// newTestAPI boots the API with a "test" kind and registers three agent
// instances (alpha, beta, gamma) through the real /register endpoint. The
// returned map is keyed by the server-generated agent names.
func newTestAPI(t *testing.T) (*httptest.Server, string, string, map[string]string) {
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
	kindTok := auth.NewToken()
	if _, err := st.CreateKind(context.Background(), "test", "Test", auth.Hash(kindTok)); err != nil {
		t.Fatalf("create kind: %v", err)
	}
	srv := httptest.NewServer(a.Routes())
	t.Cleanup(srv.Close)
	tokens := map[string]string{}
	for _, h := range []string{"alpha", "beta", "gamma"} {
		name, tok := registerAgent(t, srv, kindTok, h, "/tmp/"+h)
		tokens[name] = tok
	}
	return srv, adminTok, kindTok, tokens
}

func registerAgent(t *testing.T, srv *httptest.Server, kindTok, hostname, workdir string) (string, string) {
	t.Helper()
	resp, body := req(t, "POST", srv.URL+"/api/v1/register", kindTok, map[string]any{
		"hostname": hostname,
		"workdir":  workdir,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register %s: %d %v", hostname, resp.StatusCode, body)
	}
	name, _ := body["name"].(string)
	tok, _ := body["token"].(string)
	if name == "" || tok == "" {
		t.Fatalf("bad register response: %v", body)
	}
	return name, tok
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
	srv, _, _, tokens := newTestAPI(t)
	base := srv.URL

	resp, body := req(t, "POST", base+"/api/v1/sessions", tokens["test-alpha-alpha"], map[string]any{
		"title":        "build feature",
		"participants": []string{"test-beta-beta"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create session: %d %v", resp.StatusCode, body)
	}
	sid := body["id"].(string)

	resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/messages", tokens["test-alpha-alpha"], map[string]any{"content": "hello beta"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post message: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "GET", base+"/api/v1/wait?after=0&timeout=2", tokens["test-beta-beta"], nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wait: %d %v", resp.StatusCode, body)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %v", msgs)
	}
	first := msgs[0].(map[string]any)
	if first["content"] != "hello beta" || first["sender_id"] != "test-alpha-alpha" || first["kind"] != "chat" {
		t.Fatalf("bad message: %v", first)
	}
	latest := int64(body["latest_seq"].(float64))

	start := time.Now()
	resp, body = req(t, "GET", base+"/api/v1/wait?after="+strconv.FormatInt(latest, 10)+"&timeout=1", tokens["test-beta-beta"], nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("timed wait: %d %v", resp.StatusCode, body)
	}
	if len(body["messages"].([]any)) != 0 {
		t.Fatalf("expected empty messages, got %v", body["messages"])
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatalf("wait returned too fast: %v", time.Since(start))
	}

	resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/messages", tokens["test-gamma-gamma"], map[string]any{"content": "hi"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("gamma should not be able to post: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/join", tokens["test-gamma-gamma"], nil)
	if resp.StatusCode != http.StatusOK || body["joined"] != true {
		t.Fatalf("join: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/join", tokens["test-gamma-gamma"], nil)
	if resp.StatusCode != http.StatusOK || body["joined"] != false {
		t.Fatalf("second join: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "GET", base+"/api/v1/wait?after=0&timeout=2", tokens["test-gamma-gamma"], nil)
	msgs = body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages for gamma, got %d", len(msgs))
	}
	sys := msgs[1].(map[string]any)
	if sys["kind"] != "system" || sys["sender_id"] != "test-gamma-gamma" {
		t.Fatalf("bad system message: %v", sys)
	}

	resp, body = req(t, "PUT", base+"/api/v1/sessions/"+sid+"/context/deploy_url", tokens["test-beta-beta"], map[string]any{"value": "http://x"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put context: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "GET", base+"/api/v1/sessions/"+sid+"/context/deploy_url", tokens["test-beta-beta"], nil)
	if resp.StatusCode != http.StatusOK || body["value"] != "http://x" {
		t.Fatalf("get context: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "GET", base+"/api/v1/messages?after=0", tokens["test-beta-beta"], nil)
	msgs = body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("catchup expected 3 messages (chat, join, context), got %d", len(msgs))
	}
}

func TestRegistration(t *testing.T) {
	srv, adminTok, kindTok, tokens := newTestAPI(t)
	base := srv.URL

	// Re-registering the same (kind, hostname, workdir) yields the same name
	// and REPLACES the previous instance: only the newest token works.
	name1, tok1 := registerAgent(t, srv, kindTok, "alpha", "/tmp/alpha")
	if name1 != "test-alpha-alpha" {
		t.Fatalf("expected test-alpha-alpha, got %q", name1)
	}
	name2, tok2 := registerAgent(t, srv, kindTok, "alpha", "/tmp/alpha")
	if name2 != name1 {
		t.Fatalf("expected stable name, got %q vs %q", name2, name1)
	}
	resp, body := req(t, "GET", base+"/api/v1/sessions", tok1, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replaced token should fail: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "GET", base+"/api/v1/sessions", tok2, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("new instance token should work: %d %v", resp.StatusCode, body)
	}

	// Same host, different workdir: a distinct identity and a distinct name.
	name3, _ := registerAgent(t, srv, kindTok, "alpha", "/tmp/other")
	if name3 == name1 {
		t.Fatalf("different workdir should be a different agent: %q", name3)
	}
	if name3 != "test-alpha-other" {
		t.Fatalf("expected dir-derived name, got %q", name3)
	}

	// Different paths with the same basename slug collide on the derived name
	// and get a counter suffix.
	name4, _ := registerAgent(t, srv, kindTok, "alpha", "/elsewhere/alpha")
	if name4 != "test-alpha-alpha-2" {
		t.Fatalf("expected suffixed name, got %q", name4)
	}

	// Kind tokens are register-only.
	resp, body = req(t, "GET", base+"/api/v1/sessions", kindTok, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("kind token on sessions: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "GET", base+"/api/v1/wait?timeout=1", kindTok, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("kind token on wait: %d %v", resp.StatusCode, body)
	}

	// Register requires a kind token.
	resp, body = req(t, "POST", base+"/api/v1/register", tokens["test-beta-beta"], map[string]any{"hostname": "x", "workdir": "/x"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("register with instance token: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "POST", base+"/api/v1/register", adminTok, map[string]any{"hostname": "x", "workdir": "/x"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("register with admin token: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "POST", base+"/api/v1/register", "bogus", map[string]any{"hostname": "x", "workdir": "/x"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("register with bogus token: %d %v", resp.StatusCode, body)
	}

	// Validation.
	resp, body = req(t, "POST", base+"/api/v1/register", kindTok, map[string]any{"hostname": "", "workdir": "/x"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty hostname: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "POST", base+"/api/v1/register", kindTok, map[string]any{"hostname": "x", "workdir": ""})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty workdir: %d %v", resp.StatusCode, body)
	}

	// Agents list carries hostname and workdir.
	resp, body = req(t, "GET", base+"/api/v1/agents", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list agents: %d", resp.StatusCode)
	}
	found := false
	for _, a := range body["agents"].([]any) {
		m := a.(map[string]any)
		if m["id"] == "test-alpha-alpha" {
			found = true
			if m["hostname"] != "alpha" || m["workdir"] != "/tmp/alpha" {
				t.Fatalf("agent missing registration info: %v", m)
			}
		}
	}
	if !found {
		t.Fatalf("test-alpha missing from list: %v", body)
	}

	// Revoking kills the agent's live instance token.
	resp, body = req(t, "POST", base+"/api/v1/agents/test-alpha-alpha/revoke", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: %d %v", resp.StatusCode, body)
	}
	resp, _ = req(t, "GET", base+"/api/v1/sessions", tok2, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token should fail: %d", resp.StatusCode)
	}
	resp, body = req(t, "POST", base+"/api/v1/agents/ghost/revoke", adminTok, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("revoke unknown: %d %v", resp.StatusCode, body)
	}
}

// TestRegistrationReplacesInstance verifies that re-registering an identity
// kicks the previous instance: old token 401, identity stays a participant,
// and the sessions it joined get a system message.
func TestRegistrationReplacesInstance(t *testing.T) {
	srv, _, kindTok, tokens := newTestAPI(t)
	base := srv.URL

	// alpha (from the harness) creates a session it participates in.
	resp, body := req(t, "POST", base+"/api/v1/sessions", tokens["test-alpha-alpha"], map[string]any{"title": "takeover"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create session: %d %v", resp.StatusCode, body)
	}
	sid := body["id"].(string)

	// A new process registers with the same identity key.
	resp, body = req(t, "POST", base+"/api/v1/register", kindTok, map[string]any{"hostname": "alpha", "workdir": "/tmp/alpha"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("re-register: %d %v", resp.StatusCode, body)
	}
	if body["replaced"] != true {
		t.Fatalf("expected replaced=true, got %v", body)
	}
	tok, _ := body["token"].(string)

	// The previous instance's token is dead.
	resp, body = req(t, "GET", base+"/api/v1/sessions", tokens["test-alpha-alpha"], nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("previous instance token should be revoked: %d %v", resp.StatusCode, body)
	}

	// The identity is still a participant of its session.
	resp, body = req(t, "GET", base+"/api/v1/sessions/"+sid, tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session detail: %d %v", resp.StatusCode, body)
	}
	found := false
	for _, p := range body["participants"].([]any) {
		if p.(map[string]any)["id"] == "test-alpha-alpha" {
			found = true
		}
	}
	if !found {
		t.Fatalf("identity should remain a participant: %v", body)
	}

	// The replacement was announced in the session.
	resp, body = req(t, "GET", base+"/api/v1/sessions/"+sid+"/messages?after=0", tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("messages: %d", resp.StatusCode)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected the announcement message, got %v", msgs)
	}
	m := msgs[0].(map[string]any)
	if m["kind"] != "system" || m["sender_id"] != "test-alpha-alpha" || m["content"] != "test-alpha-alpha reconnected (previous instance replaced)" {
		t.Fatalf("bad announcement: %v", m)
	}

	// The new instance can post.
	resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/messages", tok, map[string]any{"content": "new instance here"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post as new instance: %d %v", resp.StatusCode, body)
	}
}

func TestAuthAndAdmin(t *testing.T) {
	srv, adminTok, _, tokens := newTestAPI(t)
	base := srv.URL

	resp, _ := req(t, "GET", base+"/api/v1/sessions", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	resp, _ = req(t, "GET", base+"/api/v1/sessions", "bogus", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", resp.StatusCode)
	}

	resp, body := req(t, "POST", base+"/api/v1/kinds", tokens["test-alpha-alpha"], map[string]any{"id": "evil"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("kind create by non-admin: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "POST", base+"/api/v1/kinds", adminTok, map[string]any{"id": "codex", "display_name": "Codex"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("kind create: %d %v", resp.StatusCode, body)
	}
	newKindTok, _ := body["token"].(string)
	if newKindTok == "" {
		t.Fatalf("missing kind token: %v", body)
	}
	resp, body = req(t, "POST", base+"/api/v1/kinds", adminTok, map[string]any{"id": "codex"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate kind: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "GET", base+"/api/v1/kinds", tokens["test-alpha-alpha"], nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list kinds: %d %v", resp.StatusCode, body)
	}
	kinds := body["kinds"].([]any)
	if len(kinds) != 2 {
		t.Fatalf("expected 2 kinds, got %v", kinds)
	}

	resp, body = req(t, "POST", base+"/api/v1/kinds/codex/rotate", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate kind: %d %v", resp.StatusCode, body)
	}
	rotated, _ := body["token"].(string)
	resp, _ = req(t, "POST", base+"/api/v1/register", newKindTok, map[string]any{"hostname": "h", "workdir": "/w"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old kind token should fail: %d", resp.StatusCode)
	}
	resp, _ = req(t, "POST", base+"/api/v1/register", rotated, map[string]any{"hostname": "h", "workdir": "/w"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("rotated kind token should register: %d", resp.StatusCode)
	}
	// Rotating the kind token leaves existing instance tokens valid.
	resp, _ = req(t, "GET", base+"/api/v1/sessions", tokens["test-alpha-alpha"], nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("instance token should survive kind rotation: %d", resp.StatusCode)
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
	srv, _, _, tokens := newTestAPI(t)
	base := srv.URL
	_, body := req(t, "POST", base+"/api/v1/sessions", tokens["test-alpha-alpha"], map[string]any{"title": "t"})
	sid := body["id"].(string)
	msgBody := map[string]any{"id": "fixed-msg-id", "content": "same"}
	resp, first := req(t, "POST", base+"/api/v1/sessions/"+sid+"/messages", tokens["test-alpha-alpha"], msgBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first post: %d %v", resp.StatusCode, first)
	}
	resp, second := req(t, "POST", base+"/api/v1/sessions/"+sid+"/messages", tokens["test-alpha-alpha"], msgBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("retry post: %d %v", resp.StatusCode, second)
	}
	if first["seq"].(float64) != second["seq"].(float64) {
		t.Fatalf("seq mismatch: %v vs %v", first["seq"], second["seq"])
	}
	_, body = req(t, "GET", base+"/api/v1/sessions/"+sid+"/messages", tokens["test-alpha-alpha"], nil)
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
}

func TestArchiveBlocksPosting(t *testing.T) {
	srv, _, _, tokens := newTestAPI(t)
	base := srv.URL
	_, body := req(t, "POST", base+"/api/v1/sessions", tokens["test-alpha-alpha"], map[string]any{"title": "t"})
	sid := body["id"].(string)
	resp, body := req(t, "PATCH", base+"/api/v1/sessions/"+sid, tokens["test-alpha-alpha"], map[string]any{"status": "archived"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("archive: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/messages", tokens["test-alpha-alpha"], map[string]any{"content": "hi"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("post to archived: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "PATCH", base+"/api/v1/sessions/"+sid, tokens["test-beta-beta"], map[string]any{"status": "active"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-creator patch: %d %v", resp.StatusCode, body)
	}
}
