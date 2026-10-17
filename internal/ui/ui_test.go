package ui_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"butler/internal/auth"
	"butler/internal/hub"
	"butler/internal/store"
	"butler/internal/ui"
)

func newPanel(t *testing.T) (*httptest.Server, *store.Store, *hub.Hub, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "butler.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	h := hub.New()
	t.Cleanup(func() {
		h.Close()
		st.Close()
	})
	const adminTok = "test-admin-token"
	panel, err := ui.New(st, h, adminTok, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new panel: %v", err)
	}
	panel.Heartbeat = 50 * time.Millisecond
	mux := http.NewServeMux()
	panel.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, st, h, adminTok
}

func newBrowser(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	return &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func login(t *testing.T, client *http.Client, base, token string) *http.Response {
	t.Helper()
	resp, err := client.PostForm(base+"/panel/login", url.Values{"token": {token}})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	return resp
}

func TestLoginFlow(t *testing.T) {
	srv, _, _, adminTok := newPanel(t)
	client := newBrowser(t)

	resp, err := client.Get(srv.URL + "/panel/")
	if err != nil {
		t.Fatalf("get dashboard: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "/panel/login") {
		t.Fatalf("unauthenticated dashboard: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	loginResp := login(t, client, srv.URL, "wrong-token")
	if loginResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad login: %d", loginResp.StatusCode)
	}

	loginResp = login(t, client, srv.URL, adminTok)
	if loginResp.StatusCode != http.StatusSeeOther || loginResp.Header.Get("Location") != "/panel/" {
		t.Fatalf("good login: %d %s", loginResp.StatusCode, loginResp.Header.Get("Location"))
	}

	resp, err = client.Get(srv.URL + "/panel/")
	if err != nil {
		t.Fatalf("get dashboard: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Sessions") {
		t.Fatalf("dashboard: %d %s", resp.StatusCode, string(body)[:min(200, len(body))])
	}
}

func TestSessionViewAndPost(t *testing.T) {
	srv, st, h, adminTok := newPanel(t)
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "alpha", "alpha", "codex", "alpha-host", "/tmp/proj", ""); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := st.CreateSession(ctx, "s1", "panel test", "alpha", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := st.InsertMessage(ctx, &store.Message{SessionID: "s1", SenderID: "alpha", Kind: "chat", Content: "existing message"}); err != nil {
		t.Fatalf("insert message: %v", err)
	}

	client := newBrowser(t)
	login(t, client, srv.URL, adminTok)

	resp, err := client.Get(srv.URL + "/panel/sessions/s1")
	if err != nil {
		t.Fatalf("session view: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session view: %d", resp.StatusCode)
	}
	for _, want := range []string{"panel test", "existing message", `data-stream="/panel/sessions/s1/stream?after=`, "alpha", "alpha-host", "/tmp/proj"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("session view missing %q in:\n%s", want, string(body))
		}
	}

	resp, err = client.PostForm(srv.URL+"/panel/sessions/s1/messages", url.Values{"content": {"hello from the panel"}})
	if err != nil {
		t.Fatalf("post message: %v", err)
	}
	frag, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(frag), "hello from the panel") || !strings.Contains(string(frag), "admin") {
		t.Fatalf("post fragment: %d %s", resp.StatusCode, string(frag))
	}

	if _, err := st.GetAgent(ctx, "admin"); err != nil {
		t.Fatalf("admin agent should be auto-created: %v", err)
	}
	ok, err := st.IsParticipant(ctx, "s1", "admin")
	if err != nil || !ok {
		t.Fatalf("admin should be auto-joined: %v %v", ok, err)
	}
	msgs, err := st.SessionLatestMessages(ctx, "s1", 10)
	if err != nil {
		t.Fatalf("latest messages: %v", err)
	}
	var hasJoin, hasPost bool
	for _, m := range msgs {
		if m.Kind == "system" && strings.Contains(m.Content, "admin joined") {
			hasJoin = true
		}
		if m.SenderID == "admin" && m.Content == "hello from the panel" {
			hasPost = true
		}
	}
	if !hasJoin || !hasPost {
		t.Fatalf("expected join system message and post, got %+v", msgs)
	}

	resp, err = client.PostForm(srv.URL+"/panel/sessions/s1/context", url.Values{"key": {"deploy"}, "value": {"panel-ok"}})
	if err != nil {
		t.Fatalf("put context: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("put context: %d", resp.StatusCode)
	}
	item, err := st.GetContextItem(ctx, "s1", "deploy")
	if err != nil || item.Value != "panel-ok" || item.AuthorID != "admin" {
		t.Fatalf("context item: %v %+v", err, item)
	}

	_ = h
}

func TestDashboardShowsRegistrationInfo(t *testing.T) {
	srv, st, _, adminTok := newPanel(t)
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "codex-laptop", "codex-laptop", "codex", "laptop.local", "/Users/andrew/proj", ""); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	// A per-session agent: display title differs from the id, and the
	// table splits them into Kind / Title columns (id shown as a sub-line).
	if _, err := st.CreateAgent(ctx, "opencode:fix-read-marks", "Fix read marks", "opencode", "laptop.local", "/Users/andrew/proj", "ses_one"); err != nil {
		t.Fatalf("create session agent: %v", err)
	}
	client := newBrowser(t)
	login(t, client, srv.URL, adminTok)
	resp, err := client.Get(srv.URL + "/panel/")
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard: %d", resp.StatusCode)
	}
	for _, want := range []string{
		"codex-laptop", "laptop.local", "/Users/andrew/proj", "Working directory",
		"<th>Kind</th>", "<th>Title</th>", "Fix read marks", "opencode:fix-read-marks",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("dashboard missing %q in:\n%s", want, string(body))
		}
	}
}

// TestPanelKickAgent: the agents-table kick button EVICTS an agent — tokens
// revoked, hidden from the dashboard and from session participants, with a
// system notice in each session it was in. The row survives in the store
// (history references it) and never applies to the admin.
func TestPanelKickAgent(t *testing.T) {
	srv, st, _, adminTok := newPanel(t)
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if _, err := st.CreateAgent(ctx, "opencode:fix-read-marks", "Fix read marks", "opencode", "h", "/w", "ses_one"); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	liveTok := auth.NewToken()
	if err := st.ReplaceAgentToken(ctx, "opencode:fix-read-marks", auth.Hash(liveTok)); err != nil {
		t.Fatalf("replace token: %v", err)
	}
	if _, err := st.CreateSession(ctx, "s1", "kick test", "admin", []string{"opencode:fix-read-marks"}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	client := newBrowser(t)
	login(t, client, srv.URL, adminTok)

	// The dashboard offers one kick button per agent row — except the admin's.
	resp, err := client.Get(srv.URL + "/panel/")
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `action="/panel/agents/opencode:fix-read-marks/revoke"`) {
		t.Fatalf("dashboard missing kick action:\n%s", string(body))
	}
	if strings.Count(string(body), "/revoke") != 1 {
		t.Fatalf("admin row must not offer a kick:\n%s", string(body))
	}

	// The session page shows it as a participant before the kick.
	resp, err = client.Get(srv.URL + "/panel/sessions/s1")
	if err != nil {
		t.Fatalf("session view: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Fix read marks") {
		t.Fatalf("session view missing participant:\n%s", string(body))
	}

	// The live token resolves before the kick.
	if _, err := st.GetAgentByTokenHash(ctx, auth.Hash(liveTok)); err != nil {
		t.Fatalf("token should resolve pre-kick: %v", err)
	}

	resp, err = client.PostForm(srv.URL+"/panel/agents/opencode:fix-read-marks/revoke", nil)
	if err != nil {
		t.Fatalf("kick: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("kick: %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/panel/" {
		t.Fatalf("kick redirect: %q", loc)
	}

	// Tokens are revoked; the agent is GONE from the dashboard (visible
	// immediately, not after the 90s presence window) and hidden from the
	// session's participants; a system notice explains the eviction.
	if _, err := st.GetAgentByTokenHash(ctx, auth.Hash(liveTok)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("kicked token should be revoked: %v", err)
	}
	resp, err = client.Get(srv.URL + "/panel/")
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	// The eviction notice in the sessions preview may name the agent; what
	// must be gone is its agents-table row (kick form) and its pick.
	if strings.Contains(string(body), `action="/panel/agents/opencode:fix-read-marks/revoke"`) {
		t.Fatalf("evicted agent still has a kick form:\n%s", string(body))
	}
	if strings.Contains(string(body), `value="opencode:fix-read-marks"`) {
		t.Fatalf("evicted agent still pickable:\n%s", string(body))
	}
	resp, err = client.Get(srv.URL + "/panel/sessions/s1")
	if err != nil {
		t.Fatalf("session view: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "Fix read marks") {
		t.Fatalf("evicted agent should not be a participant:\n%s", string(body))
	}
	if !strings.Contains(string(body), "was kicked by admin") {
		t.Fatalf("session view missing eviction notice:\n%s", string(body))
	}
	// The row survives in the store: history (messages, sessions) still
	// references the id. Reviving happens on re-registration (api tests).
	if a, err := st.GetAgent(ctx, "opencode:fix-read-marks"); err != nil || a.RemovedAt == nil {
		t.Fatalf("evicted row should survive with removed_at set: %v %+v", err, a)
	}

	// Kicking the admin is a silent no-op redirect.
	resp, err = client.PostForm(srv.URL+"/panel/agents/admin/revoke", nil)
	if err != nil {
		t.Fatalf("kick admin: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("kick admin: %d", resp.StatusCode)
	}

	// Kicking an unknown agent 404s.
	resp, err = client.PostForm(srv.URL+"/panel/agents/ghost/revoke", nil)
	if err != nil {
		t.Fatalf("kick ghost: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("kick ghost: %d", resp.StatusCode)
	}
}

// TestPanelJoinParticipant: the session view's participants area adds a role
// to an existing meeting. The picker offers visible non-participants only —
// roles, never hidden agent clients or evicted agents — the POST writes the
// participant row plus a join notice and lands back on the session, and
// duplicate/unknown/admin submits are rejected.
func TestPanelJoinParticipant(t *testing.T) {
	srv, st, _, adminTok := newPanel(t)
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	// A role in the meeting, a role to add, an evicted role, and an agent
	// client (hidden while it has a live role sibling).
	if _, err := st.CreateAgent(ctx, "opencode:fix-read-marks", "Fix read marks", "opencode", "h", "/w", "ses_one"); err != nil {
		t.Fatalf("create participant: %v", err)
	}
	if _, err := st.CreateAgent(ctx, "opencode:polish-panel", "Polish panel", "opencode", "h", "/w", "ses_two"); err != nil {
		t.Fatalf("create addable: %v", err)
	}
	if _, err := st.CreateAgent(ctx, "opencode:dead-session", "Dead session", "opencode", "h", "/w", "ses_dead"); err != nil {
		t.Fatalf("create dead: %v", err)
	}
	if err := st.RemoveAgent(ctx, "opencode:dead-session"); err != nil {
		t.Fatalf("evict dead: %v", err)
	}
	if _, err := st.CreateAgent(ctx, "opencode:h:butler", "opencode:h:butler", "opencode", "h", "/w", ""); err != nil {
		t.Fatalf("create client: %v", err)
	}
	if _, err := st.CreateSession(ctx, "s1", "meeting", "admin", []string{"opencode:fix-read-marks"}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	client := newBrowser(t)
	login(t, client, srv.URL, adminTok)

	resp, err := client.Get(srv.URL + "/panel/sessions/s1")
	if err != nil {
		t.Fatalf("session view: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `action="/panel/sessions/s1/join"`) {
		t.Fatalf("session view missing add-participant form:\n%s", string(body))
	}
	if !strings.Contains(string(body), `value="opencode:polish-panel"`) {
		t.Fatalf("picker missing the addable role:\n%s", string(body))
	}
	if strings.Contains(string(body), `value="opencode:fix-read-marks"`) {
		t.Fatalf("picker offers a current participant:\n%s", string(body))
	}
	if strings.Contains(string(body), `value="opencode:dead-session"`) {
		t.Fatalf("picker offers an evicted agent:\n%s", string(body))
	}
	if strings.Contains(string(body), `value="opencode:h:butler"`) {
		t.Fatalf("picker offers a hidden agent client:\n%s", string(body))
	}

	join := func(agent string) int {
		t.Helper()
		resp, err := client.PostForm(srv.URL+"/panel/sessions/s1/join", url.Values{"agent": {agent}})
		if err != nil {
			t.Fatalf("join %s: %v", agent, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// Rejected submits: unknown, evicted, hidden client, admin, duplicate.
	for _, agent := range []string{"opencode:nope", "opencode:dead-session", "opencode:h:butler", "admin", "opencode:fix-read-marks"} {
		if got := join(agent); got != http.StatusBadRequest {
			t.Fatalf("join %q: got %d, want 400", agent, got)
		}
	}

	// Happy path: 303 back to the session; participant row + join notice.
	resp, err = client.PostForm(srv.URL+"/panel/sessions/s1/join", url.Values{"agent": {"opencode:polish-panel"}})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("join: %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/panel/sessions/s1" {
		t.Fatalf("join redirect: %q", loc)
	}
	parts, err := st.Participants(ctx, "s1")
	if err != nil {
		t.Fatalf("participants: %v", err)
	}
	found := false
	for _, a := range parts {
		if a.ID == "opencode:polish-panel" {
			found = true
		}
	}
	if !found {
		t.Fatalf("added role is not a participant: %+v", parts)
	}
	msgs, err := st.SessionLatestMessages(ctx, "s1", 20)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	noticed := false
	for _, m := range msgs {
		if m.Kind == "system" && m.SenderID == "opencode:polish-panel" && strings.Contains(m.Content, "joined the session") {
			noticed = true
		}
	}
	if !noticed {
		t.Fatalf("missing join notice for the added role: %+v", msgs)
	}

	// The view now lists it as a participant and stops offering it.
	resp, err = client.Get(srv.URL + "/panel/sessions/s1")
	if err != nil {
		t.Fatalf("session view: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Polish panel") {
		t.Fatalf("session view missing the added participant:\n%s", string(body))
	}
	if strings.Contains(string(body), `value="opencode:polish-panel"`) {
		t.Fatalf("picker still offers a participant:\n%s", string(body))
	}
}

func TestOriginCheck(t *testing.T) {
	srv, _, _, adminTok := newPanel(t)
	client := newBrowser(t)
	login(t, client, srv.URL, adminTok)

	req, err := http.NewRequest("POST", srv.URL+"/panel/sessions/s1/messages", strings.NewReader("content=hi"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin post: %d", resp.StatusCode)
	}
}

func TestSSEStream(t *testing.T) {
	srv, st, h, adminTok := newPanel(t)
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "alpha", "alpha", "codex", "alpha-host", "/tmp/proj", ""); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := st.CreateSession(ctx, "s1", "sse test", "alpha", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	client := newBrowser(t)
	login(t, client, srv.URL, adminTok)

	streamCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(streamCtx, "GET", srv.URL+"/panel/sessions/s1/stream?after=0", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type: %s", ct)
	}

	var posted atomic.Bool
	go func() {
		time.Sleep(150 * time.Millisecond)
		if _, err := st.InsertMessage(ctx, &store.Message{SessionID: "s1", SenderID: "alpha", Kind: "chat", Content: "live via sse"}); err != nil {
			t.Errorf("insert: %v", err)
			return
		}
		h.NotifySession("s1")
		posted.Store(true)
	}()

	events := readSSE(t, resp.Body, 2*time.Second)
	var gotMessage, gotPresence bool
	for _, ev := range events {
		if ev.name == "message" && strings.Contains(ev.data, "live via sse") {
			gotMessage = true
		}
		if ev.name == "presence" && strings.Contains(ev.data, "alpha") {
			gotPresence = true
		}
	}
	if !posted.Load() {
		t.Fatal("poster goroutine did not run")
	}
	if !gotMessage {
		t.Fatalf("expected message event with live content, got %v", events)
	}
	if !gotPresence {
		t.Fatalf("expected presence event from heartbeat, got %v", events)
	}
}

type sseEvent struct {
	name string
	data string
}

func readSSE(t *testing.T, r io.Reader, timeout time.Duration) []sseEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var events []sseEvent
	type state struct {
		name string
		data strings.Builder
	}
	cur := state{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 256<<10)
	done := make(chan []sseEvent, 1)
	go func() {
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				cur.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if cur.data.Len() > 0 {
					cur.data.WriteByte('\n')
				}
				cur.data.WriteString(strings.TrimPrefix(line, "data: "))
			case line == "":
				if cur.name != "" || cur.data.Len() > 0 {
					events = append(events, sseEvent{name: cur.name, data: cur.data.String()})
				}
				cur = state{}
			}
			if time.Now().After(deadline) {
				break
			}
		}
		done <- events
	}()
	select {
	case evs := <-done:
		return evs
	case <-time.After(timeout + 500*time.Millisecond):
		return events
	}
}

// TestPanelPostMessageMultipart reproduces the browser's fetch(new FormData)
// request: multipart/form-data bodies must reach PostFormValue too. Plain
// r.ParseForm silently ignores them, which made the compose Send button fail
// with "content is required" while urlencoded tests kept passing.
func TestPanelPostMessageMultipart(t *testing.T) {
	srv, st, _, adminTok := newPanel(t)
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "alpha", "alpha", "codex", "alpha-host", "/tmp/proj", ""); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := st.CreateSession(ctx, "s1", "multipart post", "alpha", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	client := newBrowser(t)
	login(t, client, srv.URL, adminTok)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("content", "sent via multipart"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	req, err := http.NewRequest("POST", srv.URL+"/panel/sessions/s1/messages", &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("multipart post: %d %s", resp.StatusCode, string(body))
	}
	if !strings.Contains(string(body), "sent via multipart") || !strings.Contains(string(body), "admin") {
		t.Fatalf("fragment missing content: %s", string(body))
	}
	msgs, err := st.SessionLatestMessages(ctx, "s1", 10)
	if err != nil {
		t.Fatalf("latest messages: %v", err)
	}
	var found bool
	for _, m := range msgs {
		if m.SenderID == "admin" && m.Content == "sent via multipart" {
			found = true
		}
	}
	if !found {
		t.Fatalf("message not stored: %+v", msgs)
	}
}

func TestArchivedSessionRejectsPosts(t *testing.T) {
	srv, st, _, adminTok := newPanel(t)
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "alpha", "alpha", "codex", "alpha-host", "/tmp/proj", ""); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := st.CreateSession(ctx, "s1", "archived test", "alpha", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := st.SetSessionStatus(ctx, "s1", "archived"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	client := newBrowser(t)
	login(t, client, srv.URL, adminTok)
	resp, err := client.PostForm(srv.URL+"/panel/sessions/s1/messages", url.Values{"content": {"nope"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected conflict, got %d %s", resp.StatusCode, string(body))
	}
}

func TestPanelCreateSession(t *testing.T) {
	srv, st, _, adminTok := newPanel(t)
	ctx := context.Background()
	for _, a := range []string{"codex:alpha:proj", "opencode:beta:proj"} {
		if _, err := st.CreateAgent(ctx, a, a, "kind", "h", "/w", ""); err != nil {
			t.Fatalf("create agent: %v", err)
		}
	}

	client := newBrowser(t)
	login(t, client, srv.URL, adminTok)

	// The dashboard form offers every registered agent as a pick.
	resp, err := client.Get(srv.URL + "/panel/")
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"New session", `name="agents" value="codex:alpha:proj"`, `value="opencode:beta:proj"`, `action="/panel/sessions"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}

	// Create a session with both agents picked.
	form := url.Values{"title": {"ship the release"}, "agents": {"codex:alpha:proj", "opencode:beta:proj"}}
	resp, err = client.PostForm(srv.URL+"/panel/sessions", form)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create session: %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/panel/sessions/") {
		t.Fatalf("bad redirect: %q", loc)
	}
	sid := strings.TrimPrefix(loc, "/panel/sessions/")

	sess, err := st.GetSession(ctx, sid)
	if err != nil || sess.Title != "ship the release" || sess.CreatedBy != "admin" {
		t.Fatalf("session: %v %+v", err, sess)
	}
	// you + both picked agents are participants — and the picked agents got
	// join notices, so waiting agents learn they were added.
	for _, a := range []string{"admin", "codex:alpha:proj", "opencode:beta:proj"} {
		ok, err := st.IsParticipant(ctx, sid, a)
		if err != nil || !ok {
			t.Fatalf("participant %s: %v %v", a, ok, err)
		}
	}
	msgs, err := st.SessionLatestMessages(ctx, sid, 10)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	joins := 0
	for _, m := range msgs {
		if m.Kind == "system" && strings.Contains(m.Content, " joined the session") {
			joins++
		}
	}
	if joins != 2 {
		t.Fatalf("expected 2 join notices, got %d: %+v", joins, msgs)
	}

	// One agent can be in many sessions at once (global + this one).
	if _, err := st.EnsureGlobalSession(ctx, "admin"); err != nil {
		t.Fatalf("ensure global: %v", err)
	}
	if _, err := st.JoinSession(ctx, store.GlobalSessionID, "codex:alpha:proj"); err != nil {
		t.Fatalf("join global: %v", err)
	}
	sids, err := st.SessionsForAgent(ctx, "codex:alpha:proj")
	if err != nil || len(sids) != 2 {
		t.Fatalf("expected membership in 2 sessions, got %v: %v", sids, err)
	}

	// Validation: title required, unknown agent rejected.
	resp, err = client.PostForm(srv.URL+"/panel/sessions", url.Values{"title": {""}})
	if err != nil {
		t.Fatalf("empty title post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty title: %d", resp.StatusCode)
	}
	resp, err = client.PostForm(srv.URL+"/panel/sessions", url.Values{"title": {"x"}, "agents": {"ghost"}})
	if err != nil {
		t.Fatalf("ghost post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown agent: %d", resp.StatusCode)
	}
}

func TestSessionViewMentionables(t *testing.T) {
	srv, st, _, adminTok := newPanel(t)
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatalf("create you: %v", err)
	}
	if _, err := st.CreateAgent(ctx, "codex:alpha:proj", "Alpha bot", "codex", "h", "/w", ""); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := st.CreateSession(ctx, "s1", "room", "admin", []string{"codex:alpha:proj"}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	client := newBrowser(t)
	login(t, client, srv.URL, adminTok)
	resp, err := client.Get(srv.URL + "/panel/sessions/s1")
	if err != nil {
		t.Fatalf("session view: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// The composer carries the @autocomplete targets (other participants by
	// display title + all, "admin" itself excluded) as a JSON data attribute.
	// html/template escapes quotes in attribute values as &#34;.
	want := `data-mentionables="[&#34;all&#34;,&#34;Alpha bot&#34;]"`
	if !strings.Contains(string(body), want) {
		t.Fatalf("session view missing mentionables %q in:\n%s", want, string(body))
	}
	// The id is not offered when the agent has a title (the id still shows
	// up in the data-agents map, which is what read marks resolve against).
	frag := string(body)
	const attr = `data-mentionables="`
	frag = frag[strings.Index(frag, attr)+len(attr):]
	frag = frag[:strings.Index(frag, `"`)]
	if strings.Contains(frag, "codex:alpha:proj") {
		t.Fatalf("mentionables should list titles, not ids: %s", frag)
	}
}

func TestSessionViewReadMarks(t *testing.T) {
	srv, st, _, adminTok := newPanel(t)
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatalf("create you: %v", err)
	}
	if _, err := st.CreateAgent(ctx, "codex:alpha:proj", "codex:alpha:proj", "codex", "h", "/w", ""); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := st.CreateAgent(ctx, "opencode:beta:proj", "opencode:beta:proj", "opencode", "h", "/w", ""); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := st.CreateSession(ctx, "s1", "room", "admin", []string{"codex:alpha:proj", "opencode:beta:proj"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := st.InsertMessage(ctx, &store.Message{SessionID: "s1", SenderID: "admin", Kind: "chat", Content: "hello agents"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	client := newBrowser(t)
	login(t, client, srv.URL, adminTok)
	resp, err := client.Get(srv.URL + "/panel/sessions/s1")
	if err != nil {
		t.Fatalf("session view: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Unread: both agent participants could read it, none has yet.
	if !strings.Contains(string(body), `✓0/2`) {
		t.Fatalf("session view missing unread mark 0/2 in:\n%s", string(body))
	}
	// The read state is embedded for app.js (agent ids + agent->seq map).
	if !strings.Contains(string(body), `data-agents="[&#34;codex:alpha:proj&#34;,&#34;opencode:beta:proj&#34;]"`) {
		t.Fatalf("session view missing data-agents in:\n%s", string(body))
	}
	if !strings.Contains(string(body), `data-reads="{}"`) {
		t.Fatalf("session view missing empty data-reads in:\n%s", string(body))
	}

	// One agent waits (cursor lands on the message) → 1/2, not "all".
	if err := st.MarkRead(ctx, "s1", "codex:alpha:proj", 1); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	resp, err = client.Get(srv.URL + "/panel/sessions/s1")
	if err != nil {
		t.Fatalf("session view: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `✓1/2`) || strings.Contains(string(body), "m-read all") {
		t.Fatalf("session view missing 1/2 mark (not all) in:\n%s", string(body))
	}

	// Both read → 2/2 with the "all" class.
	if err := st.MarkRead(ctx, "s1", "opencode:beta:proj", 1); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	resp, err = client.Get(srv.URL + "/panel/sessions/s1")
	if err != nil {
		t.Fatalf("session view: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `✓2/2`) || !strings.Contains(string(body), "m-read all") {
		t.Fatalf("session view missing 2/2 all mark in:\n%s", string(body))
	}
}

func TestSSEStreamReads(t *testing.T) {
	srv, st, h, adminTok := newPanel(t)
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatalf("create you: %v", err)
	}
	if _, err := st.CreateAgent(ctx, "codex:alpha:proj", "codex:alpha:proj", "codex", "h", "/w", ""); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := st.CreateSession(ctx, "s1", "sse reads", "admin", []string{"codex:alpha:proj"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	msg, err := st.InsertMessage(ctx, &store.Message{SessionID: "s1", SenderID: "admin", Kind: "chat", Content: "did you get this"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	client := newBrowser(t)
	login(t, client, srv.URL, adminTok)

	streamCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(streamCtx, "GET", srv.URL+"/panel/sessions/s1/stream?after=0", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer resp.Body.Close()

	go func() {
		time.Sleep(150 * time.Millisecond)
		// The agent's wait batch landing in the store: cursor advances and
		// the session stream is woken (as api.markReads does).
		if err := st.MarkRead(ctx, "s1", "codex:alpha:proj", msg.Seq); err != nil {
			t.Errorf("mark read: %v", err)
			return
		}
		h.NotifySession("s1")
	}()

	events := readSSE(t, resp.Body, 2*time.Second)
	var gotMessage, gotReads bool
	for _, ev := range events {
		if ev.name == "message" && strings.Contains(ev.data, "did you get this") {
			gotMessage = true
		}
		if ev.name == "reads" && strings.Contains(ev.data, "codex:alpha:proj") {
			gotReads = true
		}
	}
	if !gotMessage {
		t.Fatalf("expected message event, got %v", events)
	}
	if !gotReads {
		t.Fatalf("expected reads event for codex:alpha:proj, got %v", events)
	}
}
