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
	"regexp"
	"strconv"
	"strings"
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
// returned map is keyed by the harness hostname.
// testAgent is a harness-registered agent: its server-minted UUID id (the
// identity, stable across re-registration) and its instance token.
type testAgent struct {
	ID    string
	Token string
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// isUUID reports whether s is a canonical version-4 UUID — what every agent
// identity is now.
func isUUID(s string) bool { return uuidRe.MatchString(s) }

func newTestAPI(t *testing.T) (*httptest.Server, string, string, map[string]testAgent) {
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
	agents := map[string]testAgent{}
	for _, h := range []string{"alpha", "beta", "gamma"} {
		id, tok := registerAgent(t, srv, kindTok, h, "/tmp/"+h)
		agents[h] = testAgent{ID: id, Token: tok}
	}
	return srv, adminTok, kindTok, agents
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
	srv, _, _, agents := newTestAPI(t)
	base := srv.URL

	resp, body := req(t, "POST", base+"/api/v1/sessions", agents["alpha"].Token, map[string]any{
		"title":        "build feature",
		"participants": []string{agents["beta"].ID},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create session: %d %v", resp.StatusCode, body)
	}
	sid := body["id"].(string)

	resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/messages", agents["alpha"].Token, map[string]any{"content": "hello beta"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post message: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "GET", base+"/api/v1/wait?after=0&timeout=2", agents["beta"].Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wait: %d %v", resp.StatusCode, body)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %v", msgs)
	}
	first := msgs[0].(map[string]any)
	if first["content"] != "hello beta" || first["sender_id"] != agents["alpha"].ID || first["kind"] != "chat" {
		t.Fatalf("bad message: %v", first)
	}
	latest := int64(body["latest_seq"].(float64))

	start := time.Now()
	resp, body = req(t, "GET", base+"/api/v1/wait?after="+strconv.FormatInt(latest, 10)+"&timeout=1", agents["beta"].Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("timed wait: %d %v", resp.StatusCode, body)
	}
	if len(body["messages"].([]any)) != 0 {
		t.Fatalf("expected empty messages, got %v", body["messages"])
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatalf("wait returned too fast: %v", time.Since(start))
	}

	resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/messages", agents["gamma"].Token, map[string]any{"content": "hi"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("gamma should not be able to post: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/join", agents["gamma"].Token, nil)
	if resp.StatusCode != http.StatusOK || body["joined"] != true {
		t.Fatalf("join: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/join", agents["gamma"].Token, nil)
	if resp.StatusCode != http.StatusOK || body["joined"] != false {
		t.Fatalf("second join: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "GET", base+"/api/v1/wait?after=0&timeout=2", agents["gamma"].Token, nil)
	msgs = body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages for gamma, got %d", len(msgs))
	}
	sys := msgs[1].(map[string]any)
	if sys["kind"] != "system" || sys["sender_id"] != agents["gamma"].ID {
		t.Fatalf("bad system message: %v", sys)
	}

	resp, body = req(t, "PUT", base+"/api/v1/sessions/"+sid+"/context/deploy_url", agents["beta"].Token, map[string]any{"value": "http://x"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put context: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "GET", base+"/api/v1/sessions/"+sid+"/context/deploy_url", agents["beta"].Token, nil)
	if resp.StatusCode != http.StatusOK || body["value"] != "http://x" {
		t.Fatalf("get context: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "GET", base+"/api/v1/messages?after=0", agents["beta"].Token, nil)
	msgs = body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("catchup expected 3 messages (chat, join, context), got %d", len(msgs))
	}
}

func TestRegistration(t *testing.T) {
	srv, adminTok, kindTok, agents := newTestAPI(t)
	base := srv.URL

	// Re-registering the same (kind, hostname, workdir) yields the same
	// identity (a UUID) and REPLACES the previous instance: only the newest
	// token works.
	name1, tok1 := registerAgent(t, srv, kindTok, "alpha", "/tmp/alpha")
	if name1 != agents["alpha"].ID {
		t.Fatalf("expected the harness alpha identity %q, got %q", agents["alpha"].ID, name1)
	}
	if !isUUID(name1) {
		t.Fatalf("agent id should be a UUID, got %q", name1)
	}
	name2, tok2 := registerAgent(t, srv, kindTok, "alpha", "/tmp/alpha")
	if name2 != name1 {
		t.Fatalf("expected stable identity, got %q vs %q", name2, name1)
	}
	resp, body := req(t, "GET", base+"/api/v1/sessions", tok1, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replaced token should fail: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "GET", base+"/api/v1/sessions", tok2, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("new instance token should work: %d %v", resp.StatusCode, body)
	}

	// Same host, different workdir: a distinct identity (a fresh UUID).
	name3, _ := registerAgent(t, srv, kindTok, "alpha", "/tmp/other")
	if name3 == name1 {
		t.Fatalf("different workdir should be a different agent: %q", name3)
	}
	if !isUUID(name3) {
		t.Fatalf("distinct identity should still be a UUID, got %q", name3)
	}

	// Different paths with the same basename are distinct identities too —
	// UUIDs never collide, so no counter suffix is ever minted.
	name4, _ := registerAgent(t, srv, kindTok, "alpha", "/elsewhere/alpha")
	if name4 == name1 || name4 == name3 || !isUUID(name4) {
		t.Fatalf("expected a distinct UUID identity, got %q", name4)
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

	// An instance token without a client_session is a bad sibling
	// registration (400); the admin token cannot register at all.
	resp, body = req(t, "POST", base+"/api/v1/register", agents["beta"].Token, map[string]any{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("sibling register without client_session: %d %v", resp.StatusCode, body)
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
		if m["id"] == agents["alpha"].ID {
			found = true
			if m["hostname"] != "alpha" || m["workdir"] != "/tmp/alpha" {
				t.Fatalf("agent missing registration info: %v", m)
			}
		}
	}
	if !found {
		t.Fatalf("test-alpha missing from list: %v", body)
	}

	// Revoking evicts the agent: token dead, hidden from listings, and a
	// live process can revive it by re-registering.
	resp, body = req(t, "POST", base+"/api/v1/agents/"+agents["alpha"].ID+"/revoke", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: %d %v", resp.StatusCode, body)
	}
	if body["removed"] != true {
		t.Fatalf("revoke should evict: %v", body)
	}
	resp, _ = req(t, "GET", base+"/api/v1/sessions", tok2, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token should fail: %d", resp.StatusCode)
	}
	// The evicted agent is hidden from the agents list.
	resp, body = req(t, "GET", base+"/api/v1/agents", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list agents: %d %v", resp.StatusCode, body)
	}
	for _, a := range body["agents"].([]any) {
		if m := a.(map[string]any); m["id"] == agents["alpha"].ID {
			t.Fatalf("evicted agent should be hidden: %v", m)
		}
	}
	// Re-registering the identity revives it with a fresh token.
	resp, body = req(t, "POST", base+"/api/v1/register", kindTok, map[string]any{"hostname": "alpha", "workdir": "/tmp/alpha"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("revive register: %d %v", resp.StatusCode, body)
	}
	revivedTok, _ := body["token"].(string)
	resp, body = req(t, "GET", base+"/api/v1/agents", adminTok, nil)
	found = false
	for _, a := range body["agents"].([]any) {
		if m := a.(map[string]any); m["id"] == agents["alpha"].ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("revived agent should be listed: %v", body)
	}
	resp, _ = req(t, "GET", base+"/api/v1/sessions", revivedTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revived token should work: %d", resp.StatusCode)
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
	srv, _, kindTok, agents := newTestAPI(t)
	base := srv.URL

	// alpha (from the harness) creates a session it participates in.
	resp, body := req(t, "POST", base+"/api/v1/sessions", agents["alpha"].Token, map[string]any{"title": "takeover"})
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
	resp, body = req(t, "GET", base+"/api/v1/sessions", agents["alpha"].Token, nil)
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
		if p.(map[string]any)["id"] == agents["alpha"].ID {
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
	if m["kind"] != "system" || m["sender_id"] != agents["alpha"].ID || m["content"] != agents["alpha"].ID+" reconnected (previous instance replaced)" {
		t.Fatalf("bad announcement: %v", m)
	}

	// The new instance can post.
	resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/messages", tok, map[string]any{"content": "new instance here"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post as new instance: %d %v", resp.StatusCode, body)
	}
}

func TestAuthAndAdmin(t *testing.T) {
	srv, adminTok, _, agents := newTestAPI(t)
	base := srv.URL

	resp, _ := req(t, "GET", base+"/api/v1/sessions", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	resp, _ = req(t, "GET", base+"/api/v1/sessions", "bogus", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", resp.StatusCode)
	}

	resp, body := req(t, "POST", base+"/api/v1/kinds", agents["alpha"].Token, map[string]any{"id": "evil"})
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

	resp, body = req(t, "GET", base+"/api/v1/kinds", agents["alpha"].Token, nil)
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
	resp, _ = req(t, "GET", base+"/api/v1/sessions", agents["alpha"].Token, nil)
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
	srv, _, _, agents := newTestAPI(t)
	base := srv.URL
	_, body := req(t, "POST", base+"/api/v1/sessions", agents["alpha"].Token, map[string]any{"title": "t"})
	sid := body["id"].(string)
	msgBody := map[string]any{"id": "fixed-msg-id", "content": "same"}
	resp, first := req(t, "POST", base+"/api/v1/sessions/"+sid+"/messages", agents["alpha"].Token, msgBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first post: %d %v", resp.StatusCode, first)
	}
	resp, second := req(t, "POST", base+"/api/v1/sessions/"+sid+"/messages", agents["alpha"].Token, msgBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("retry post: %d %v", resp.StatusCode, second)
	}
	if first["seq"].(float64) != second["seq"].(float64) {
		t.Fatalf("seq mismatch: %v vs %v", first["seq"], second["seq"])
	}
	_, body = req(t, "GET", base+"/api/v1/sessions/"+sid+"/messages", agents["alpha"].Token, nil)
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
}

func TestArchiveBlocksPosting(t *testing.T) {
	srv, _, _, agents := newTestAPI(t)
	base := srv.URL
	_, body := req(t, "POST", base+"/api/v1/sessions", agents["alpha"].Token, map[string]any{"title": "t"})
	sid := body["id"].(string)
	resp, body := req(t, "PATCH", base+"/api/v1/sessions/"+sid, agents["alpha"].Token, map[string]any{"status": "archived"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("archive: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/messages", agents["alpha"].Token, map[string]any{"content": "hi"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("post to archived: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "PATCH", base+"/api/v1/sessions/"+sid, agents["beta"].Token, map[string]any{"status": "active"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-creator patch: %d %v", resp.StatusCode, body)
	}
}

// newTestAPIGlobal boots the API the way `butler serve` does: a "admin" agent
// and the global session every agent joins at registration.
func newTestAPIGlobal(t *testing.T) (*httptest.Server, string, string, map[string]testAgent) {
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
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatalf("create you: %v", err)
	}
	if _, err := st.EnsureGlobalSession(ctx, "admin"); err != nil {
		t.Fatalf("ensure global: %v", err)
	}
	kindTok := auth.NewToken()
	if _, err := st.CreateKind(ctx, "test", "Test", auth.Hash(kindTok)); err != nil {
		t.Fatalf("create kind: %v", err)
	}
	srv := httptest.NewServer(a.Routes())
	t.Cleanup(srv.Close)
	agents := map[string]testAgent{}
	for _, h := range []string{"alpha", "beta"} {
		id, tok := registerAgent(t, srv, kindTok, h, "/tmp/"+h)
		agents[h] = testAgent{ID: id, Token: tok}
	}
	return srv, adminTok, kindTok, agents
}

func TestRegisterAutoJoinsGlobal(t *testing.T) {
	srv, _, _, agents := newTestAPIGlobal(t)
	base := srv.URL

	// Both registered agents (and "admin") are participants of the global session.
	resp, body := req(t, "GET", base+"/api/v1/sessions/"+store.GlobalSessionID, agents["alpha"].Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get global: %d %v", resp.StatusCode, body)
	}
	parts := body["participants"].([]any)
	if len(parts) != 3 {
		t.Fatalf("expected 3 participants (you, alpha, beta), got %v", body)
	}

	// beta sees the global join notices and can post there.
	resp, body = req(t, "GET", base+"/api/v1/messages?after=0", agents["beta"].Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("catchup: %d %v", resp.StatusCode, body)
	}
	msgs := body["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("expected join notices, got %v", msgs)
	}
	last := msgs[len(msgs)-1].(map[string]any)
	if last["kind"] != "system" || last["session_id"] != store.GlobalSessionID {
		t.Fatalf("expected a global join notice last, got %v", last)
	}

	resp, body = req(t, "POST", base+"/api/v1/sessions/"+store.GlobalSessionID+"/messages", agents["beta"].Token, map[string]any{"content": "hello everyone"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post to global: %d %v", resp.StatusCode, body)
	}

	// alpha's wait (across all joined sessions) picks the global message.
	latest := int64(body["seq"].(float64))
	resp, body = req(t, "GET", base+"/api/v1/wait?after="+strconv.FormatInt(latest-1, 10)+"&timeout=2", agents["alpha"].Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wait: %d %v", resp.StatusCode, body)
	}
	msgs = body["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "hello everyone" {
		t.Fatalf("alpha should see the global message: %v", msgs)
	}
}

func TestMentionFlow(t *testing.T) {
	srv, _, _, agents := newTestAPIGlobal(t)
	base := srv.URL

	// An exact-id mention in the global session resolves to the mentioned agent.
	resp, body := req(t, "POST", base+"/api/v1/sessions/"+store.GlobalSessionID+"/messages", agents["alpha"].Token, map[string]any{
		"content": "@" + agents["beta"].ID + " please take the failing test",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post mention: %d %v", resp.StatusCode, body)
	}
	mentions, ok := body["mentions"].([]any)
	if !ok || len(mentions) != 1 || mentions[0] != agents["beta"].ID {
		t.Fatalf("expected mentions [%s], got %v", agents["beta"].ID, body)
	}
	seq := int64(body["seq"].(float64))

	// The mentioned agent sees the mentions list on wait.
	resp, body = req(t, "GET", base+"/api/v1/wait?after="+strconv.FormatInt(seq-1, 10)+"&timeout=2", agents["beta"].Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wait: %d %v", resp.StatusCode, body)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %v", msgs)
	}
	m := msgs[0].(map[string]any)
	if m["content"] != "@"+agents["beta"].ID+" please take the failing test" {
		t.Fatalf("bad message: %v", m)
	}
	if mentions, ok := m["mentions"].([]any); !ok || len(mentions) != 1 || mentions[0] != agents["beta"].ID {
		t.Fatalf("wait response should carry mentions, got %v", m)
	}

	// Unmentioned messages (and @unknown tokens) resolve to no mentions.
	resp, body = req(t, "POST", base+"/api/v1/sessions/"+store.GlobalSessionID+"/messages", agents["alpha"].Token, map[string]any{"content": "fyi only, no mention"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post fyi: %d %v", resp.StatusCode, body)
	}
	if _, ok := body["mentions"]; ok {
		t.Fatalf("unmentioned message should omit mentions, got %v", body)
	}

	// A role's display title is what the panel's @autocomplete inserts, so a
	// title mention ("@Fix read marks") resolves to that role.
	sib := registerSibling(t, srv, agents["alpha"].Token, "ses_mention", "Fix read marks")
	roleID, _ := sib["name"].(string)
	resp, body = req(t, "POST", base+"/api/v1/sessions/"+store.GlobalSessionID+"/messages", agents["beta"].Token, map[string]any{"content": "@Fix read marks please review"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post title mention: %d %v", resp.StatusCode, body)
	}
	mentions, ok = body["mentions"].([]any)
	if !ok || len(mentions) != 1 || mentions[0] != roleID {
		t.Fatalf("expected mentions [%s], got %v", roleID, body)
	}
}

// newReadTestAPI boots a minimal API whose store is returned to the test so
// read cursors can be inspected directly.
func newReadTestAPI(t *testing.T) (*httptest.Server, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "butler.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	h := hub.New()
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		h.Close()
		st.Close()
	})
	a := &api.API{
		Store:      st,
		Hub:        h,
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
	return srv, st, kindTok
}

// TestReadMarksRecorded covers the three delivery paths that advance an
// agent's read cursor: a wait long-poll batch, a session message fetch, and
// the agent's own post.
func TestReadMarksRecorded(t *testing.T) {
	srv, st, kindTok := newReadTestAPI(t)
	ctx := context.Background()
	readerName, readerTok := registerAgent(t, srv, kindTok, "reader", "/tmp/reader")
	fetcherName, fetcherTok := registerAgent(t, srv, kindTok, "fetcher", "/tmp/fetcher")
	posterName, posterTok := registerAgent(t, srv, kindTok, "poster", "/tmp/poster")

	// A message from a fourth party (the human "admin").
	if _, err := st.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatalf("create you: %v", err)
	}
	if _, err := st.CreateSession(ctx, "s1", "read marks", "admin", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	for _, aid := range []string{readerName, fetcherName, posterName} {
		if _, err := st.JoinSession(ctx, "s1", aid); err != nil {
			t.Fatalf("join: %v", err)
		}
	}
	if _, err := st.JoinSession(ctx, "s1", "admin"); err != nil {
		t.Fatalf("join: %v", err)
	}
	msg1, err := st.InsertMessage(ctx, &store.Message{SessionID: "s1", SenderID: "admin", Kind: "chat", Content: "hello agents"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Wait delivers the message: the reader's cursor must cover it.
	waitReq, err := http.NewRequest("GET", srv.URL+"/api/v1/wait?timeout=2", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	waitReq.Header.Set("Authorization", "Bearer "+readerTok)
	resp, err := http.DefaultClient.Do(waitReq)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("hello agents")) {
		t.Fatalf("wait: status=%d body=%s", resp.StatusCode, body)
	}
	marks, err := st.ReadMarks(ctx, "s1")
	if err != nil {
		t.Fatalf("read marks: %v", err)
	}
	if marks[readerName] < msg1.Seq {
		t.Fatalf("wait should mark read up to seq %d, got %v", msg1.Seq, marks)
	}

	// A direct session message fetch also marks the fetcher's read.
	fetchReq, err := http.NewRequest("GET", srv.URL+"/api/v1/sessions/s1/messages?after=0", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	fetchReq.Header.Set("Authorization", "Bearer "+fetcherTok)
	resp, err = http.DefaultClient.Do(fetchReq)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("hello agents")) {
		t.Fatalf("fetch: status=%d body=%s", resp.StatusCode, body)
	}
	marks, err = st.ReadMarks(ctx, "s1")
	if err != nil {
		t.Fatalf("read marks: %v", err)
	}
	if marks[fetcherName] < msg1.Seq {
		t.Fatalf("fetch should mark read up to seq %d, got %v", msg1.Seq, marks)
	}

	// The poster's own post advances their cursor to it.
	postBody, _ := json.Marshal(map[string]string{"content": "poster speaking"})
	postReq, err := http.NewRequest("POST", srv.URL+"/api/v1/sessions/s1/messages", bytes.NewReader(postBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	postReq.Header.Set("Authorization", "Bearer "+posterTok)
	postReq.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(postReq)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post: status=%d body=%s", resp.StatusCode, body)
	}
	var posted store.Message
	if err := json.Unmarshal(body, &posted); err != nil {
		t.Fatalf("decode post: %v", err)
	}
	marks, err = st.ReadMarks(ctx, "s1")
	if err != nil {
		t.Fatalf("read marks: %v", err)
	}
	if marks[posterName] < posted.Seq {
		t.Fatalf("own post should mark read up to seq %d, got %v", posted.Seq, marks)
	}

	// Admin reads do not create cursors.
	adminReq, err := http.NewRequest("GET", srv.URL+"/api/v1/sessions/s1/messages?after=0", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	adminReq.Header.Set("Authorization", "Bearer "+adminTok)
	resp, err = http.DefaultClient.Do(adminReq)
	if err != nil {
		t.Fatalf("admin fetch: %v", err)
	}
	resp.Body.Close()
	if _, ok := marks["admin"]; ok {
		t.Fatalf("no cursor expected for humans/admins, got %v", marks)
	}
}

// registerSibling posts a sibling registration (instance token +
// client_session + display name) and returns the response body.
func registerSibling(t *testing.T, srv *httptest.Server, tok, clientSession, displayName string) map[string]any {
	t.Helper()
	resp, body := req(t, "POST", srv.URL+"/api/v1/register", tok, map[string]any{
		"client_session": clientSession,
		"display_name":   displayName,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("sibling register %s: %d %v", clientSession, resp.StatusCode, body)
	}
	return body
}

// TestSiblingRegistration: an instance token mints per-client-session agents
// of its own identity — named from the display name (the client session's
// title) so a human can tell them apart, joined to the global session, and
// re-registering one replaces its token without the app-level announcement.
func TestSiblingRegistration(t *testing.T) {
	srv, adminTok, kindTok, agents := newTestAPIGlobal(t)
	base := srv.URL

	appTok := agents["alpha"].Token // the app-level instance token

	// Sibling "sessions" of the alpha identity: each is a fresh UUID identity,
	// labeled with its OpenCode session title. The app-level identity keeps
	// its own id.
	b1 := registerSibling(t, srv, appTok, "ses_one", "Fix read marks")
	role1, _ := b1["name"].(string)
	if !isUUID(role1) || role1 == agents["alpha"].ID {
		t.Fatalf("expected a fresh UUID role, got %q", role1)
	}
	if b1["client_session"] != "ses_one" {
		t.Fatalf("client_session echo: %v", b1)
	}
	if b1["display_name"] != "Fix read marks" {
		t.Fatalf("display_name should be the title: %v", b1)
	}
	b2 := registerSibling(t, srv, appTok, "ses_two", "Dark theme polish")
	if b2["display_name"] != "Dark theme polish" || b2["name"] == role1 {
		t.Fatalf("second role should be distinct and titled: %v", b2)
	}
	// The same title on a different session is still a distinct identity.
	b3 := registerSibling(t, srv, appTok, "ses_three", "Fix read marks")
	if b3["name"] == b1["name"] {
		t.Fatalf("different session should be a different identity: %q", b3["name"])
	}
	// A sibling that is not titled yet is labeled with its own id.
	b4 := registerSibling(t, srv, appTok, "ses_four", "")
	role4, _ := b4["name"].(string)
	if b4["display_name"] != role4 {
		t.Fatalf("untitled sibling should be labeled with its id: %v", b4)
	}

	tok1, _ := b1["token"].(string)
	// The sibling token authenticates as its own agent.
	resp, body := req(t, "GET", base+"/api/v1/sessions?joined=1", tok1, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sibling token should work: %d %v", resp.StatusCode, body)
	}
	found := false
	for _, s := range body["sessions"].([]any) {
		if m := s.(map[string]any); m["id"] == "global" {
			found = true
		}
	}
	if !found {
		t.Fatalf("sibling should auto-join the global session: %v", body)
	}

	// Re-registering the same client_session replaces the token, keeps the
	// name, refreshes the display name, and does NOT post the app-level
	// reconnect announcement (replaced siblings are routine: their plugin
	// re-registers them on every app restart).
	before := latestSeq(t, base, appTok)
	b1b := registerSibling(t, srv, appTok, "ses_one", "Fix read marks (renamed)")
	if b1b["name"] != b1["name"] {
		t.Fatalf("sibling identity should be stable: %q vs %q", b1b["name"], b1["name"])
	}
	if b1b["replaced"] != true {
		t.Fatalf("expected replaced=true: %v", b1b)
	}
	resp, _ = req(t, "GET", base+"/api/v1/sessions?joined=1", tok1, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replaced sibling token should 401: %d", resp.StatusCode)
	}
	if latestSeq(t, base, appTok) != before {
		t.Fatalf("sibling replacement should be silent, messages appeared (seq %d -> %d)", before, latestSeq(t, base, appTok))
	}
	// The display name follows the rename; the id does not.
	resp, body = req(t, "GET", base+"/api/v1/agents", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list agents: %d", resp.StatusCode)
	}
	for _, a := range body["agents"].([]any) {
		if m := a.(map[string]any); m["id"] == b1["name"] {
			if m["display_name"] != "Fix read marks (renamed)" {
				t.Fatalf("display name should follow the rename: %v", m)
			}
		}
	}

	// An instance token cannot register an APP-LEVEL identity (no
	// client_session → 400, covered in TestRegistration) and a sibling cannot
	// escape its own identity: kind/hostname/workdir are inherited, not taken
	// from the body.
	resp, body = req(t, "POST", base+"/api/v1/register", kindTok, map[string]any{
		"hostname": "alpha", "workdir": "/tmp/alpha",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("kind token still registers app-level: %d %v", resp.StatusCode, body)
	}
	kindName, _ := body["name"].(string)
	if kindName != agents["alpha"].ID {
		t.Fatalf("app-level name unchanged: %q", kindName)
	}
}

// TestAgentRoles: an instance token lists its own agent client's roles
// (removed flagged) — the eager-activation feed the OpenCode relay plugin
// consumes at startup. Kind and admin tokens are rejected. While the client
// has a live role it is hidden from the agents list; evicting every role
// makes it the visible fallback again.
func TestAgentRoles(t *testing.T) {
	srv, adminTok, kindTok, agents := newTestAPIGlobal(t)
	base := srv.URL
	appTok := agents["alpha"].Token

	// Auth surface: instance tokens only.
	resp, _ := req(t, "GET", base+"/api/v1/agents/roles", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	resp, _ = req(t, "GET", base+"/api/v1/agents/roles", kindTok, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("kind token: %d", resp.StatusCode)
	}
	resp, _ = req(t, "GET", base+"/api/v1/agents/roles", adminTok, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("admin token: %d", resp.StatusCode)
	}

	// No roles yet: empty list, client still visible.
	resp, body := req(t, "GET", base+"/api/v1/agents/roles", appTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("roles: %d %v", resp.StatusCode, body)
	}
	if body["identity"] != agents["alpha"].ID {
		t.Fatalf("identity: %v", body)
	}
	if roles, ok := body["roles"].([]any); !ok || len(roles) != 0 {
		t.Fatalf("expected empty roles: %v", body)
	}
	agentListed := func(tok, id string) bool {
		resp, body := req(t, "GET", base+"/api/v1/agents", tok, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list agents: %d %v", resp.StatusCode, body)
		}
		for _, a := range body["agents"].([]any) {
			if m := a.(map[string]any); m["id"] == id {
				return true
			}
		}
		return false
	}
	if !agentListed(adminTok, agents["alpha"].ID) {
		t.Fatal("client without roles should be listed")
	}

	// Two roles on the alpha identity.
	b1 := registerSibling(t, srv, appTok, "ses_one", "Fix read marks")
	b2 := registerSibling(t, srv, appTok, "ses_two", "Dark theme polish")
	role1, _ := b1["name"].(string)
	role2, _ := b2["name"].(string)
	sibTok, _ := b2["token"].(string)

	// Both the client's token and a role's token see the same roles.
	for _, tok := range []string{appTok, sibTok} {
		resp, body = req(t, "GET", base+"/api/v1/agents/roles", tok, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("roles via %q: %d %v", tok, resp.StatusCode, body)
		}
		roles, _ := body["roles"].([]any)
		if len(roles) != 2 {
			t.Fatalf("expected 2 roles: %v", body)
		}
		for _, r := range roles {
			m := r.(map[string]any)
			if m["removed"] != false {
				t.Fatalf("role should not be removed: %v", m)
			}
			if m["client_session"] != "ses_one" && m["client_session"] != "ses_two" {
				t.Fatalf("unexpected role: %v", m)
			}
		}
	}

	// A live role hides the client; the roles themselves are listed.
	if agentListed(adminTok, agents["alpha"].ID) {
		t.Fatal("client with a live role should be hidden")
	}
	if !agentListed(adminTok, role1) || !agentListed(adminTok, role2) {
		t.Fatal("roles should be listed")
	}

	// Evict one role: it is flagged removed, the client stays hidden.
	resp, _ = req(t, "POST", base+"/api/v1/agents/"+role1+"/revoke", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke role1: %d", resp.StatusCode)
	}
	resp, body = req(t, "GET", base+"/api/v1/agents/roles", appTok, nil)
	removedCount := 0
	for _, r := range body["roles"].([]any) {
		m := r.(map[string]any)
		if m["agent"] == role1 && m["removed"] == true {
			removedCount++
		}
	}
	if removedCount != 1 {
		t.Fatalf("evicted role should be flagged: %v", body["roles"])
	}
	if agentListed(adminTok, agents["alpha"].ID) {
		t.Fatal("client should stay hidden while role2 is live")
	}

	// Evicting the last role makes the client the visible fallback again.
	resp, _ = req(t, "POST", base+"/api/v1/agents/"+role2+"/revoke", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke role2: %d", resp.StatusCode)
	}
	if !agentListed(adminTok, agents["alpha"].ID) {
		t.Fatal("client should be visible once every role is evicted")
	}
}

func latestSeq(t *testing.T, base, tok string) int64 {
	t.Helper()
	resp, body := req(t, "GET", base+"/api/v1/seq", tok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("seq: %d %v", resp.StatusCode, body)
	}
	n, _ := body["latest_seq"].(float64)
	return int64(n)
}

// TestRenameAgent: display titles follow client-session renames through the
// dedicated rename endpoint — an instance token renames siblings of its own
// identity keyed by client session (the plugin path: OpenCode/codex session
// title changes), the admin renames any agent by id. The agent id never
// changes; kind tokens are rejected; cross-identity renames 404; the admin
// always shows online.
func TestRenameAgent(t *testing.T) {
	srv, adminTok, kindTok, agents := newTestAPIGlobal(t)
	base := srv.URL

	appTok := agents["alpha"].Token
	betaTok := agents["beta"].Token

	b1 := registerSibling(t, srv, appTok, "ses_one", "Fix read marks")
	roleID, _ := b1["name"].(string)
	if !isUUID(roleID) {
		t.Fatalf("expected a UUID role identity, got %q", roleID)
	}
	sibTok, _ := b1["token"].(string)

	// The sibling renames itself by its session uuid.
	resp, body := req(t, "POST", base+"/api/v1/agents/rename", sibTok, map[string]any{
		"client_session": "ses_one", "display_name": "Fix read marks v2",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("self rename: %d %v", resp.StatusCode, body)
	}
	if body["id"] != roleID || body["display_name"] != "Fix read marks v2" {
		t.Fatalf("rename echo: %v", body)
	}

	// The app-level instance token renames its sibling as well (the plugin
	// holds both tokens).
	resp, body = req(t, "POST", base+"/api/v1/agents/rename", appTok, map[string]any{
		"client_session": "ses_one", "display_name": "Fix read marks v3",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sibling rename via app token: %d %v", resp.StatusCode, body)
	}

	// An instance token cannot rename another identity's session.
	resp, _ = req(t, "POST", base+"/api/v1/agents/rename", betaTok, map[string]any{
		"client_session": "ses_one", "display_name": "hijack",
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-identity rename should 404: %d", resp.StatusCode)
	}

	// Kind tokens only authorize registration.
	resp, _ = req(t, "POST", base+"/api/v1/agents/rename", kindTok, map[string]any{
		"client_session": "ses_one", "display_name": "nope",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("kind token rename should 403: %d", resp.StatusCode)
	}

	// The admin renames any agent by id.
	resp, body = req(t, "POST", base+"/api/v1/agents/rename", adminTok, map[string]any{
		"agent": roleID, "display_name": "Fix read marks v4",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin rename: %d %v", resp.StatusCode, body)
	}
	if body["id"] != roleID || body["display_name"] != "Fix read marks v4" {
		t.Fatalf("admin rename echo: %v", body)
	}

	// Validation: blank title, missing client_session (instance), missing
	// agent (admin), unknown agent.
	resp, _ = req(t, "POST", base+"/api/v1/agents/rename", appTok, map[string]any{
		"client_session": "ses_one", "display_name": "   ",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("blank display_name should 400: %d", resp.StatusCode)
	}
	resp, _ = req(t, "POST", base+"/api/v1/agents/rename", appTok, map[string]any{
		"display_name": "x",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing client_session should 400: %d", resp.StatusCode)
	}
	resp, _ = req(t, "POST", base+"/api/v1/agents/rename", adminTok, map[string]any{
		"display_name": "x",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("admin without agent should 400: %d", resp.StatusCode)
	}
	resp, _ = req(t, "POST", base+"/api/v1/agents/rename", adminTok, map[string]any{
		"agent": "ghost", "display_name": "x",
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown agent should 404: %d", resp.StatusCode)
	}

	// Persisted in the agents list; the admin is always online.
	resp, body = req(t, "GET", base+"/api/v1/agents", adminTok, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list agents: %d %v", resp.StatusCode, body)
	}
	seenRenamed, seenAdmin := false, false
	for _, a := range body["agents"].([]any) {
		m := a.(map[string]any)
		switch m["id"] {
		case roleID:
			seenRenamed = true
			if m["display_name"] != "Fix read marks v4" {
				t.Fatalf("rename persisted: %v", m)
			}
		case "admin":
			seenAdmin = true
			if m["online"] != true {
				t.Fatalf("admin should always be online: %v", m)
			}
		}
	}
	if !seenRenamed || !seenAdmin {
		t.Fatalf("agents list incomplete: %v", body)
	}
}

// A session can be deleted by its creator or admin (never by an unrelated
// agent), the fixed global session refuses both delete and archive, and the
// deletion is complete: the session, its messages and its participants go.
func TestDeleteSessionAPI(t *testing.T) {
	srv, adminTok, _, agents := newTestAPIGlobal(t)
	base := srv.URL

	resp, body := req(t, "POST", base+"/api/v1/sessions", agents["alpha"].Token, map[string]any{
		"title":        "doomed",
		"participants": []string{agents["beta"].ID},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create session: %d %v", resp.StatusCode, body)
	}
	sid := body["id"].(string)
	if resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/messages", agents["alpha"].Token, map[string]any{"content": "gone soon"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("post: %d %v", resp.StatusCode, body)
	}

	// An agent that is neither creator nor admin may not delete it.
	if resp, body = req(t, "DELETE", base+"/api/v1/sessions/"+sid, agents["beta"].Token, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-creator delete should 403: %d %v", resp.StatusCode, body)
	}
	// Unknown sessions are 404s, and the global session is fixed.
	if resp, _ = req(t, "DELETE", base+"/api/v1/sessions/nope", adminTok, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session should 404: %d", resp.StatusCode)
	}
	if resp, body = req(t, "DELETE", base+"/api/v1/sessions/global", adminTok, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("deleting global should 409: %d %v", resp.StatusCode, body)
	}
	if resp, body = req(t, "PATCH", base+"/api/v1/sessions/global", adminTok, map[string]any{"status": "archived"}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("archiving global should 409: %d %v", resp.StatusCode, body)
	}

	// The creator can, and everything scoped to it is gone.
	if resp, body = req(t, "DELETE", base+"/api/v1/sessions/"+sid, agents["alpha"].Token, nil); resp.StatusCode != http.StatusOK || body["deleted"] != true {
		t.Fatalf("delete: %d %v", resp.StatusCode, body)
	}
	if resp, _ = req(t, "GET", base+"/api/v1/sessions/"+sid, adminTok, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted session should 404: %d", resp.StatusCode)
	}
	if resp, body = req(t, "GET", base+"/api/v1/sessions/"+sid+"/messages?after=0", agents["beta"].Token, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("messages of a deleted session should 404: %d %v", resp.StatusCode, body)
	}
	if resp, body = req(t, "GET", base+"/api/v1/sessions", adminTok, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("list sessions: %d %v", resp.StatusCode, body)
	}
	for _, s := range body["sessions"].([]any) {
		if s.(map[string]any)["id"] == sid {
			t.Fatalf("deleted session still listed: %v", s)
		}
	}
}

// An archived session is frozen for joins at the API too: the store refuses
// it, so no participant row and no join notice are written (the panel checks
// this itself, but joining is the one write that used to skip the guard).
func TestJoinArchivedSessionAPI(t *testing.T) {
	srv, _, _, agents := newTestAPI(t)
	base := srv.URL

	resp, body := req(t, "POST", base+"/api/v1/sessions", agents["alpha"].Token, map[string]any{"title": "frozen"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create session: %d %v", resp.StatusCode, body)
	}
	sid := body["id"].(string)
	resp, body = req(t, "PATCH", base+"/api/v1/sessions/"+sid, agents["alpha"].Token, map[string]any{"status": "archived"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("archive: %d %v", resp.StatusCode, body)
	}

	resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/join", agents["gamma"].Token, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("joining an archived session should 409: %d %v", resp.StatusCode, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "archived") {
		t.Fatalf("conflict should say why: %v", body)
	}

	// gamma is not a participant and no notice landed in the frozen session.
	resp, body = req(t, "GET", base+"/api/v1/sessions/"+sid, agents["alpha"].Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session detail: %d %v", resp.StatusCode, body)
	}
	for _, p := range body["participants"].([]any) {
		if p.(map[string]any)["id"] == agents["gamma"].ID {
			t.Fatalf("refused join must not add a participant: %v", p)
		}
	}
	resp, body = req(t, "GET", base+"/api/v1/sessions/"+sid+"/messages?after=0", agents["alpha"].Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("messages: %d %v", resp.StatusCode, body)
	}
	if len(body["messages"].([]any)) != 0 {
		t.Fatalf("refused join must not write a notice: %v", body)
	}

	// Reopening restores joining.
	if resp, body = req(t, "PATCH", base+"/api/v1/sessions/"+sid, agents["alpha"].Token, map[string]any{"status": "active"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("reopen: %d %v", resp.StatusCode, body)
	}
	if resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/join", agents["gamma"].Token, nil); resp.StatusCode != http.StatusOK || body["joined"] != true {
		t.Fatalf("join after reopen: %d %v", resp.StatusCode, body)
	}
}

// Removing a participant: the creator (or admin) may take anyone out, an agent
// may take itself out, and nobody else may touch the membership. The removed
// agent stays registered — it just stops being in that session.
func TestRemoveParticipantAPI(t *testing.T) {
	srv, adminTok, _, agents := newTestAPI(t)
	base := srv.URL

	resp, body := req(t, "POST", base+"/api/v1/sessions", agents["alpha"].Token, map[string]any{
		"title":        "curate me",
		"participants": []string{agents["beta"].ID, agents["gamma"].ID},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create session: %d %v", resp.StatusCode, body)
	}
	sid := body["id"].(string)
	removeURL := base + "/api/v1/sessions/" + sid + "/participants/"

	// A plain participant may not remove someone else.
	if resp, body = req(t, "DELETE", removeURL+agents["alpha"].ID, agents["gamma"].Token, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-creator removal should 403: %d %v", resp.StatusCode, body)
	}
	// Unknown membership and unknown sessions are 404s.
	if resp, _ = req(t, "DELETE", removeURL+"11111111-2222-3333-4444-555555555555", agents["alpha"].Token, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown participant should 404: %d", resp.StatusCode)
	}
	if resp, _ = req(t, "DELETE", base+"/api/v1/sessions/nope/participants/"+agents["beta"].ID, adminTok, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session should 404: %d", resp.StatusCode)
	}
	// The creator removes beta.
	if resp, body = req(t, "DELETE", removeURL+agents["beta"].ID, agents["alpha"].Token, nil); resp.StatusCode != http.StatusOK || body["removed"] != true {
		t.Fatalf("remove beta: %d %v", resp.StatusCode, body)
	}
	if resp, body = req(t, "GET", base+"/api/v1/sessions/"+sid, agents["alpha"].Token, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("session detail: %d %v", resp.StatusCode, body)
	}
	for _, p := range body["participants"].([]any) {
		if p.(map[string]any)["id"] == agents["beta"].ID {
			t.Fatalf("beta should be out of the session: %v", p)
		}
	}
	// The removal notice is in the session (by display id here: untitled test
	// agents), and the removed agent no longer receives the session's traffic.
	resp, body = req(t, "GET", base+"/api/v1/sessions/"+sid+"/messages?after=0", agents["alpha"].Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("messages: %d %v", resp.StatusCode, body)
	}
	msgs := body["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["kind"] != "system" || !strings.Contains(last["content"].(string), "was removed from the session by") {
		t.Fatalf("removal notice missing: %v", last)
	}
	if resp, body = req(t, "POST", base+"/api/v1/sessions/"+sid+"/messages", agents["alpha"].Token, map[string]any{"content": "after the removal"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("post: %d %v", resp.StatusCode, body)
	}
	resp, body = req(t, "GET", base+"/api/v1/wait?after=0&timeout=1", agents["beta"].Token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("wait: %d %v", resp.StatusCode, body)
	}
	if got := body["messages"]; got != nil && len(got.([]any)) != 0 {
		t.Fatalf("a removed agent should receive nothing from that session: %v", got)
	}

	// An agent may remove itself ("leave").
	if resp, body = req(t, "DELETE", removeURL+agents["gamma"].ID, agents["gamma"].Token, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("self-removal: %d %v", resp.StatusCode, body)
	}
	// Admin may remove anyone.
	if resp, body = req(t, "DELETE", removeURL+agents["alpha"].ID, adminTok, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin removal: %d %v", resp.StatusCode, body)
	}
	// Archived sessions are frozen.
	if resp, body = req(t, "PATCH", base+"/api/v1/sessions/"+sid, adminTok, map[string]any{"status": "archived"}); resp.StatusCode != http.StatusOK {
		t.Fatalf("archive: %d %v", resp.StatusCode, body)
	}
	if resp, body = req(t, "DELETE", removeURL+agents["gamma"].ID, adminTok, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("removal from an archived session should 409: %d %v", resp.StatusCode, body)
	}
}
