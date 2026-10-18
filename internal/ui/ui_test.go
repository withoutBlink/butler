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
		if m.Kind == "system" && strings.Contains(m.Content, "Admin joined the session") {
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

// The panel's message list shows the sender's display name (the session
// title), not the UUID: the id stays only as the identity tooltip and in
// data-sender for the read-mark math. Read-mark tooltips name the readers
// too, and a sender who has left the session (a kicked agent's history) is
// resolved from its surviving agents row, so the list shows a UUID only for
// an identity whose display name *is* its id.
func TestMessageSenderShowsDisplayName(t *testing.T) {
	srv, st, _, adminTok := newPanel(t)
	ctx := context.Background()
	const (
		dev    = "2ad4443c-a473-4af8-a781-4b483d84d8d3"
		tester = "fb3b3d41-fc49-4492-bb2a-5586e537c071"
		kicked = "cccccccc-0000-0000-0000-000000000000"
		plain  = "dddddddd-0000-0000-0000-000000000000"
	)
	for _, a := range []struct{ id, display string }{
		{"admin", "Admin"},
		{dev, "Develop butler"},
		{tester, "Butler tester"},
		{kicked, "Kicked Role"},
		{plain, ""}, // untitled: its id is its label
	} {
		if _, err := st.CreateAgent(ctx, a.id, a.display, "opencode", "h", "/w", "ses_"+a.id); err != nil {
			t.Fatalf("create %s: %v", a.id, err)
		}
	}
	if _, err := st.CreateSession(ctx, "s1", "meeting", "admin", nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	// JoinSession writes the join notice — a system message whose sender must
	// show its title too (join notices used to read as UUIDs).
	for _, id := range []string{dev, tester, kicked} {
		if _, err := st.JoinSession(ctx, "s1", id); err != nil {
			t.Fatalf("join %s: %v", id, err)
		}
	}
	msg, err := st.InsertMessage(ctx, &store.Message{SessionID: "s1", SenderID: dev, Kind: "chat", Content: "did you get this"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.MarkRead(ctx, "s1", tester, msg.Seq); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	// History of a kicked role keeps its title: the agents row survives.
	if _, err := st.InsertMessage(ctx, &store.Message{SessionID: "s1", SenderID: kicked, Kind: "chat", Content: "before I was kicked"}); err != nil {
		t.Fatalf("insert kicked: %v", err)
	}
	if err := st.RemoveAgent(ctx, kicked); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := st.InsertMessage(ctx, &store.Message{SessionID: "s1", SenderID: plain, Kind: "chat", Content: "untitled here"}); err != nil {
		t.Fatalf("insert untitled: %v", err)
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
	for _, want := range []string{
		`<b title="` + dev + `">Develop butler</b>`,
		`<b title="` + tester + `">Butler tester</b>`, // join notice
		`<b title="` + kicked + `">Kicked Role</b>`,   // left the session: row survives
		`<b title="` + plain + `">` + plain + `</b>`,  // untitled: the id is the label
		// The read-mark tooltip names readers by display title. The human is
		// part of the audience and is always a reader, so a room of the human
		// plus two agents that the tester alone has read shows 2/2.
		`title="read by Admin, Butler tester"`,
		`✓2/2`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("session view missing %q in:\n%s", want, string(body))
		}
	}
	// The kicked role is gone as a participant (its title survives only in
	// the message list, where it names its own history).
	if strings.Contains(string(body), `<span class="id">Kicked Role</span>`) {
		t.Fatalf("kicked role should not be a participant:\n%s", string(body))
	}
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
	// The eviction notice in the sessions preview and the message list names
	// the agent by its display title (that's the point of the notice); what
	// must be gone is the participant row, the agents-table row (kick form)
	// and its pick.
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
	if strings.Contains(string(body), `<span class="id">Fix read marks</span>`) {
		t.Fatalf("evicted agent should not be a participant:\n%s", string(body))
	}
	if !strings.Contains(string(body), "Fix read marks was kicked by admin") {
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
	// Audience includes the human. Each member's "since" is the seq its
	// membership starts at: admin created the session (0 = present from the
	// start), the two agents were added with it.
	if !strings.Contains(string(body), `data-audience="[`) ||
		!strings.Contains(string(body), `&#34;id&#34;:&#34;admin&#34;,&#34;name&#34;:&#34;Admin&#34;,&#34;kind&#34;:&#34;human&#34;,&#34;since&#34;:0,&#34;human&#34;:true`) ||
		!strings.Contains(string(body), `&#34;id&#34;:&#34;opencode:beta:proj&#34;,&#34;name&#34;:&#34;opencode:beta:proj&#34;,&#34;kind&#34;:&#34;opencode&#34;,&#34;since&#34;:0`) {
		t.Fatalf("session view missing audience in:\n%s", string(body))
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

	// An agent's message counts the human as an audience member, and the human
	// is always a reader (a mark is only visible to the human, so seeing it
	// means having seen the message). The *other* agent is the only one who
	// can still be outstanding — and the tooltip must say who has not read it.
	if _, err := st.InsertMessage(ctx, &store.Message{SessionID: "s1", SenderID: "codex:alpha:proj", Kind: "chat", Content: "did you get this"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// A member added *after* that message is not part of its audience: an old
	// message must not sit below N/N forever because somebody joined later.
	late, err := st.CreateAgent(ctx, "codex:late:proj", "codex:late:proj", "codex", "h", "/w", "")
	if err != nil {
		t.Fatalf("create late agent: %v", err)
	}
	if _, err := st.JoinSession(ctx, "s1", late.ID); err != nil {
		t.Fatalf("join late agent: %v", err)
	}
	resp, err = client.Get(srv.URL + "/panel/sessions/s1")
	if err != nil {
		t.Fatalf("session view: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `✓1/2`) {
		t.Fatalf("agent message should be 1/2 (human read, other agent not) in:\n%s", string(body))
	}
	if !strings.Contains(string(body), `title="read by Admin · not yet: opencode:beta:proj"`) {
		t.Fatalf("mark tooltip should name readers and non-readers in:\n%s", string(body))
	}
	if strings.Contains(string(body), `✓1/3`) || strings.Contains(string(body), `✓2/3`) {
		t.Fatalf("late joiner must not widen an older message's audience in:\n%s", string(body))
	}
	// The late member's membership starts at its join notice's seq (3: the two
	// chats above), which is what excludes it from the older messages.
	if !strings.Contains(string(body), `&#34;id&#34;:&#34;codex:late:proj&#34;,&#34;name&#34;:&#34;codex:late:proj&#34;,&#34;kind&#34;:&#34;codex&#34;,&#34;since&#34;:3`) {
		t.Fatalf("audience should pin the late joiner to its join seq in:\n%s", string(body))
	}
	if !strings.Contains(string(body), `data-reads="{&#34;codex:alpha:proj&#34;:1,&#34;opencode:beta:proj&#34;:1}"`) {
		t.Fatalf("data-reads should carry both agent cursors in:\n%s", string(body))
	}

	// A removal shrinks N retroactively: the removed member has no read state
	// to report, so it neither pads N nor holds the marks back. That asymmetry
	// is deliberate — a join can never enlarge an older message's audience (the
	// joiner's seq excludes it), a leave can shrink it.
	if err := st.RemoveParticipant(ctx, "s1", "opencode:beta:proj", "admin"); err != nil {
		t.Fatalf("remove participant: %v", err)
	}
	resp, err = client.Get(srv.URL + "/panel/sessions/s1")
	if err != nil {
		t.Fatalf("session view: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `✓1/1`) || !strings.Contains(string(body), `title="read by Admin"`) {
		t.Fatalf("removal should shrink N to the human alone in:\n%s", string(body))
	}
	if strings.Contains(string(body), `&#34;id&#34;:&#34;opencode:beta:proj&#34;`) {
		t.Fatalf("removed member must drop out of the audience in:\n%s", string(body))
	}
}

// TestReadMarksHumanOutsideAudience: the human is part of a message's audience
// only while admin is a participant of that session — the audience is the
// sidebar's participant list. A session an agent created through the API has
// no admin participant until admin posts (the panel's post auto-joins), so its
// marks count the other agents alone: 0/1, not 1/2.
func TestReadMarksHumanOutsideAudience(t *testing.T) {
	srv, st, _, adminTok := newPanel(t)
	ctx := context.Background()
	for _, id := range []string{"codex:alpha:proj", "opencode:beta:proj"} {
		if _, err := st.CreateAgent(ctx, id, id, "codex", "h", "/w", ""); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	if _, err := st.CreateSession(ctx, "s1", "agent room", "codex:alpha:proj", []string{"opencode:beta:proj"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := st.InsertMessage(ctx, &store.Message{SessionID: "s1", SenderID: "codex:alpha:proj", Kind: "chat", Content: "no human here"}); err != nil {
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
	if !strings.Contains(string(body), `✓0/1`) || !strings.Contains(string(body), `title="not read yet: opencode:beta:proj"`) {
		t.Fatalf("audience should be the other agent only in:\n%s", string(body))
	}
	if strings.Contains(string(body), `&#34;id&#34;:&#34;admin&#34;`) {
		t.Fatalf("a non-participant human must not be in the audience in:\n%s", string(body))
	}
}

// TestReadMarksLateJoinerHuman: the later-joiner rule applies to the human too.
// Admin added to a session after messages have flowed is not part of those
// messages' audience — no special case — while messages sent after the join
// count it (and count it read, since a mark is only visible to the human).
func TestReadMarksLateJoinerHuman(t *testing.T) {
	srv, st, _, adminTok := newPanel(t)
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatalf("create you: %v", err)
	}
	for _, id := range []string{"codex:alpha:proj", "opencode:beta:proj"} {
		if _, err := st.CreateAgent(ctx, id, id, "codex", "h", "/w", ""); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	if _, err := st.CreateSession(ctx, "s1", "agent room", "codex:alpha:proj", []string{"opencode:beta:proj"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := st.InsertMessage(ctx, &store.Message{SessionID: "s1", SenderID: "codex:alpha:proj", Kind: "chat", Content: "before you were here"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Admin joins (its join notice lands at seq 2) and then posts.
	if _, err := st.JoinSession(ctx, "s1", "admin"); err != nil {
		t.Fatalf("join admin: %v", err)
	}
	if _, err := st.InsertMessage(ctx, &store.Message{SessionID: "s1", SenderID: "codex:alpha:proj", Kind: "chat", Content: "after you joined"}); err != nil {
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
	// The pre-join message is the other agent's alone (0/1); the post-join one
	// is the human (read) plus that agent (1/2).
	if !strings.Contains(string(body), `✓0/1`) {
		t.Fatalf("pre-join message should not count the late human in:\n%s", string(body))
	}
	if !strings.Contains(string(body), `✓1/2`) || !strings.Contains(string(body), `title="read by Admin · not yet: opencode:beta:proj"`) {
		t.Fatalf("post-join message should count the human as read in:\n%s", string(body))
	}
	if strings.Contains(string(body), `✓1/1`) || strings.Contains(string(body), `✓0/2`) {
		t.Fatalf("late human must not widen an older message's audience in:\n%s", string(body))
	}
	// The human's watermark is its join notice's seq (2: the one message sent
	// before it), which is exactly what excludes it from that message.
	if !strings.Contains(string(body), `&#34;id&#34;:&#34;admin&#34;,&#34;name&#34;:&#34;Admin&#34;,&#34;kind&#34;:&#34;human&#34;,&#34;since&#34;:2,&#34;human&#34;:true`) {
		t.Fatalf("audience should pin the late human to its join seq in:\n%s", string(body))
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

	// A join reaches an open panel through the same SSE stream: the audience
	// event on the next heartbeat carries the new member pinned to its join
	// notice's seq (2 — the chat above is 1), so older messages are not part
	// of its audience.
	if _, err := st.CreateAgent(ctx, "codex:late:proj", "codex:late:proj", "codex", "h", "/w", ""); err != nil {
		t.Fatalf("create late agent: %v", err)
	}
	if _, err := st.JoinSession(ctx, "s1", "codex:late:proj"); err != nil {
		t.Fatalf("join late agent: %v", err)
	}

	events := readSSE(t, resp.Body, 2*time.Second)
	var gotMessage, gotReads, gotAudience bool
	for _, ev := range events {
		if ev.name == "message" && strings.Contains(ev.data, "did you get this") {
			gotMessage = true
		}
		if ev.name == "reads" && strings.Contains(ev.data, "codex:alpha:proj") {
			gotReads = true
		}
		// The audience is raw JSON (no HTML escaping): the human is flagged
		// and the late joiner carries the seq its membership starts at.
		if ev.name == "audience" &&
			strings.Contains(ev.data, `"id":"admin","name":"Admin","kind":"human","since":0,"human":true`) &&
			strings.Contains(ev.data, `"id":"codex:late:proj","name":"codex:late:proj","kind":"codex","since":2`) {
			gotAudience = true
		}
	}
	if !gotMessage {
		t.Fatalf("expected message event, got %v", events)
	}
	if !gotReads {
		t.Fatalf("expected reads event for codex:alpha:proj, got %v", events)
	}
	if !gotAudience {
		t.Fatalf("expected audience event with the human and the late joiner, got %v", events)
	}
}

// The panel's session lifecycle controls: archive freezes a session (the
// composer, add-participant picker and context form go away; the API and the
// panel already refuse writes on a non-active session) and reopen restores
// it; delete removes the session and everything scoped to it for good. The
// fixed global session offers neither control and refuses both requests.
func TestPanelArchiveAndDeleteSession(t *testing.T) {
	srv, st, _, adminTok := newPanel(t)
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if _, err := st.CreateAgent(ctx, "role-x", "Fix read marks", "opencode", "h", "/w", "ses_x"); err != nil {
		t.Fatalf("create role: %v", err)
	}
	if _, err := st.CreateAgent(ctx, "role-y", "Spare role", "opencode", "h", "/w2", "ses_y"); err != nil {
		t.Fatalf("create spare role: %v", err)
	}
	if _, err := st.CreateSession(ctx, "s1", "panel lifecycle", "admin", []string{"role-x"}); err != nil {
		t.Fatalf("create s1: %v", err)
	}
	if _, err := st.CreateSession(ctx, "s2", "doomed", "admin", nil); err != nil {
		t.Fatalf("create s2: %v", err)
	}
	if _, err := st.InsertMessage(ctx, &store.Message{SessionID: "s2", SenderID: "admin", Kind: "chat", Content: "soon gone"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.EnsureGlobalSession(ctx, "admin"); err != nil {
		t.Fatalf("global session: %v", err)
	}

	client := newBrowser(t)
	login(t, client, srv.URL, adminTok)

	// Both controls on the dashboard rows; the global session is marked fixed.
	resp, err := client.Get(srv.URL + "/panel/")
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		`action="/panel/sessions/s1/status"`,
		`name="status" value="archived"`,
		`action="/panel/sessions/s1/delete"`,
		`action="/panel/sessions/s2/delete"`,
		`class="iconbtn"`,
		`aria-label="Archive session"`,
		`aria-label="Delete session"`,
		`title="The global session is fixed`,
		`class="fixed-ico"`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("dashboard missing %q in:\n%s", want, string(body))
		}
	}
	if strings.Contains(string(body), `action="/panel/sessions/global/status"`) || strings.Contains(string(body), `action="/panel/sessions/global/delete"`) {
		t.Fatalf("global session must offer no lifecycle controls:\n%s", string(body))
	}
	// The actions are icons only; the words live in title/aria-label.
	if strings.Contains(string(body), ">archive<") || strings.Contains(string(body), ">reopen<") || strings.Contains(string(body), ">delete<") {
		t.Fatalf("session actions should be icon buttons:\n%s", string(body))
	}

	// Archive from the dashboard: back to the dashboard, session frozen.
	resp, err = client.PostForm(srv.URL+"/panel/sessions/s1/status", url.Values{"status": {"archived"}})
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/panel/" {
		t.Fatalf("archive redirect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// The dashboard the browser lands on states the outcome, naming the
	// session — "I can't tell whether it archived or deleted" is the bug this
	// guards. It is one-shot: the next render drops it.
	resp, err = client.Get(srv.URL + "/panel/")
	if err != nil {
		t.Fatalf("dashboard after archive: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Session “panel lifecycle” archived.") || !strings.Contains(string(body), `class="notice"`) {
		t.Fatalf("dashboard should carry the archive notice:\n%s", string(body))
	}
	resp, err = client.Get(srv.URL + "/panel/")
	if err != nil {
		t.Fatalf("dashboard refresh: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), `class="notice"`) {
		t.Fatalf("the notice must be one-shot:\n%s", string(body))
	}
	if sess, err := st.GetSession(ctx, "s1"); err != nil || sess.Status != "archived" {
		t.Fatalf("status should be archived: %v %+v", err, sess)
	}

	// The session page is read-only now, carries no lifecycle controls (those
	// live on the dashboard) and keeps the back link next to the title.
	resp, err = client.Get(srv.URL + "/panel/sessions/s1")
	if err != nil {
		t.Fatalf("session view: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Archived — this session is frozen") {
		t.Fatalf("archived banner missing:\n%s", string(body))
	}
	if !strings.Contains(string(body), `class="back" href="/panel/"`) || !strings.Contains(string(body), `class="badge archived"`) {
		t.Fatalf("session page should carry the back link and an archived badge:\n%s", string(body))
	}
	if strings.Contains(string(body), "session-actions") || strings.Contains(string(body), `action="/panel/sessions/s1/delete"`) || strings.Contains(string(body), `name="status"`) {
		t.Fatalf("session page must not carry lifecycle controls:\n%s", string(body))
	}
	if strings.Contains(string(body), `id="compose"`) || strings.Contains(string(body), "Set context</button>") {
		t.Fatalf("archived session must not offer the composer or context form:\n%s", string(body))
	}
	// The add-participant picker is gone too, and the join request the browser
	// could still make is refused server-side (like the API's join).
	if strings.Contains(string(body), `action="/panel/sessions/s1/join"`) {
		t.Fatalf("archived session must not offer the join picker:\n%s", string(body))
	}
	resp, err = client.PostForm(srv.URL+"/panel/sessions/s1/join", url.Values{"agent": {"role-y"}})
	if err != nil {
		t.Fatalf("join archived: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("joining an archived session should 409: %d", resp.StatusCode)
	}
	if ok, err := st.IsParticipant(ctx, "s1", "role-y"); err != nil || ok {
		t.Fatalf("refused panel join must not add a participant: %v %v", ok, err)
	}

	// Reopen (dashboard-side control): back to the dashboard, writable again.
	resp, err = client.PostForm(srv.URL+"/panel/sessions/s1/status", url.Values{"status": {"active"}})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/panel/" {
		t.Fatalf("reopen redirect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if sess, err := st.GetSession(ctx, "s1"); err != nil || sess.Status != "active" {
		t.Fatalf("status should be active again: %v %+v", err, sess)
	}
	resp, err = client.Get(srv.URL + "/panel/sessions/s1")
	if err != nil {
		t.Fatalf("session view: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `id="compose"`) {
		t.Fatalf("reopened session should offer the composer:\n%s", string(body))
	}

	// A bogus status is rejected, not stored.
	resp, err = client.PostForm(srv.URL+"/panel/sessions/s1/status", url.Values{"status": {"deleted"}})
	if err != nil {
		t.Fatalf("bad status: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad status should 400: %d", resp.StatusCode)
	}

	// The global session refuses both requests even when asked directly.
	resp, err = client.PostForm(srv.URL+"/panel/sessions/global/status", url.Values{"status": {"archived"}})
	if err != nil {
		t.Fatalf("archive global: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("archiving global should 409: %d", resp.StatusCode)
	}
	resp, err = client.PostForm(srv.URL+"/panel/sessions/global/delete", nil)
	if err != nil {
		t.Fatalf("delete global: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("deleting global should 409: %d", resp.StatusCode)
	}
	if sess, err := st.GetSession(ctx, store.GlobalSessionID); err != nil || sess.Status != "active" {
		t.Fatalf("global session must survive untouched: %v %+v", err, sess)
	}

	// Delete s2: gone with its messages, dashboard redirect, and the outcome
	// notice names it.
	resp, err = client.PostForm(srv.URL+"/panel/sessions/s2/delete", nil)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/panel/" {
		t.Fatalf("delete redirect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, err = client.Get(srv.URL + "/panel/")
	if err != nil {
		t.Fatalf("dashboard after delete: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "Session “doomed” deleted.") {
		t.Fatalf("dashboard should carry the delete notice:\n%s", string(body))
	}
	if _, err := st.GetSession(ctx, "s2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session should be gone: %v", err)
	}
	if msgs, err := st.SessionLatestMessages(ctx, "s2", 10); err != nil || len(msgs) != 0 {
		t.Fatalf("messages should be gone with it: %v %v", msgs, err)
	}

	// A session that is already gone — a stale dashboard row (deleted in
	// another tab) or a repeat submit — must not dump the user on a bare
	// "not found" page with no idea what happened: the intent is already
	// satisfied, so it redirects to the dashboard and says so.
	resp, err = client.PostForm(srv.URL+"/panel/sessions/nope/delete", nil)
	if err != nil {
		t.Fatalf("delete unknown: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/panel/" {
		t.Fatalf("deleting a vanished session should land on the dashboard: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, err = client.Get(srv.URL + "/panel/")
	if err != nil {
		t.Fatalf("dashboard after vanished delete: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "no longer exists") {
		t.Fatalf("dashboard should explain the vanished session:\n%s", string(body))
	}
	// Same for a repeat archive of the now-deleted s2 — and it stayed deleted.
	resp, err = client.PostForm(srv.URL+"/panel/sessions/s2/status", url.Values{"status": {"archived"}})
	if err != nil {
		t.Fatalf("archive deleted: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("archiving a vanished session should land on the dashboard: %d", resp.StatusCode)
	}
	if _, err := st.GetSession(ctx, "s2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session should still be gone: %v", err)
	}

	// The session page of a session that is gone is a real page too: 404, an
	// explanation and a way back — never a bare "not found" text blob.
	resp, err = client.Get(srv.URL + "/panel/sessions/s2")
	if err != nil {
		t.Fatalf("gone session page: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a gone session page should be 404: %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "no longer exists") || !strings.Contains(string(body), `href="/panel/"`) {
		t.Fatalf("a gone session page should explain itself and link back:\n%s", string(body))
	}
}

// The participants sidebar's remove control: one ✕ per row on an active
// session (the notice names the actor), hidden and refused once the session is
// archived, and the removed role keeps everything else — it stays registered
// and keeps its other sessions.
func TestPanelRemoveParticipant(t *testing.T) {
	srv, st, _, adminTok := newPanel(t)
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if _, err := st.CreateAgent(ctx, "role-x", "Fix read marks", "opencode", "h", "/w", "ses_x"); err != nil {
		t.Fatalf("create role-x: %v", err)
	}
	if _, err := st.CreateAgent(ctx, "role-y", "Develop butler", "opencode", "h", "/w2", "ses_y"); err != nil {
		t.Fatalf("create role-y: %v", err)
	}
	if _, err := st.CreateSession(ctx, "s1", "the meeting", "admin", []string{"role-x", "role-y"}); err != nil {
		t.Fatalf("create s1: %v", err)
	}
	if _, err := st.CreateSession(ctx, "s2", "other work", "admin", []string{"role-x"}); err != nil {
		t.Fatalf("create s2: %v", err)
	}

	client := newBrowser(t)
	login(t, client, srv.URL, adminTok)

	resp, err := client.Get(srv.URL + "/panel/sessions/s1")
	if err != nil {
		t.Fatalf("session view: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		`action="/panel/sessions/s1/participants/role-x/remove"`,
		`action="/panel/sessions/s1/participants/role-y/remove"`,
		`action="/panel/sessions/s1/participants/admin/remove"`,
		`aria-label="Remove Fix read marks from this session"`,
		`class="rowbtn"`,
		`onclick="return confirm('Remove Fix read marks from this session?`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("participants sidebar missing %q in:\n%s", want, string(body))
		}
	}

	// Remove role-x: back to the session, gone from the participants, notice
	// written, other sessions untouched.
	resp, err = client.PostForm(srv.URL+"/panel/sessions/s1/participants/role-x/remove", nil)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/panel/sessions/s1" {
		t.Fatalf("remove redirect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if ok, err := st.IsParticipant(ctx, "s1", "role-x"); err != nil || ok {
		t.Fatalf("role-x should be out of s1: %v %v", ok, err)
	}
	if ok, err := st.IsParticipant(ctx, "s1", "role-y"); err != nil || !ok {
		t.Fatalf("role-y must stay in s1: %v %v", ok, err)
	}
	if ok, err := st.IsParticipant(ctx, "s2", "role-x"); err != nil || !ok {
		t.Fatalf("role-x must keep its other session: %v %v", ok, err)
	}
	msgs, err := st.MessagesSince(ctx, "", "s1", 0, 20)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if last := msgs[len(msgs)-1]; last.Kind != "system" || last.Content != "Fix read marks was removed from the session by Admin" {
		t.Fatalf("removal notice: %+v", last)
	}
	// The page now lists role-y (still removable) without role-x, notice
	// rendered as a system message.
	resp, err = client.Get(srv.URL + "/panel/sessions/s1")
	if err != nil {
		t.Fatalf("session view: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), `participants/role-x/remove`) {
		t.Fatalf("role-x should no longer have a remove control:\n%s", string(body))
	}
	if !strings.Contains(string(body), `participants/role-y/remove`) || !strings.Contains(string(body), "Fix read marks was removed from the session by Admin") {
		t.Fatalf("role-y's control or the notice is missing:\n%s", string(body))
	}

	// A non-participant (or unknown session) is a 404, never a silent success.
	resp, err = client.PostForm(srv.URL+"/panel/sessions/s1/participants/role-x/remove", nil)
	if err != nil {
		t.Fatalf("remove twice: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("removing a non-participant should 404: %d", resp.StatusCode)
	}
	resp, err = client.PostForm(srv.URL+"/panel/sessions/nope/participants/role-y/remove", nil)
	if err != nil {
		t.Fatalf("remove unknown session: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("removing from an unknown session should 404: %d", resp.StatusCode)
	}

	// Archived: the control disappears and the request is refused.
	resp, err = client.PostForm(srv.URL+"/panel/sessions/s1/status", url.Values{"status": {"archived"}})
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	resp.Body.Close()
	resp, err = client.Get(srv.URL + "/panel/sessions/s1")
	if err != nil {
		t.Fatalf("session view: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), `participants/role-y/remove`) {
		t.Fatalf("an archived session must not offer remove controls:\n%s", string(body))
	}
	resp, err = client.PostForm(srv.URL+"/panel/sessions/s1/participants/role-y/remove", nil)
	if err != nil {
		t.Fatalf("remove archived: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("removing from an archived session should 409: %d", resp.StatusCode)
	}
	if ok, err := st.IsParticipant(ctx, "s1", "role-y"); err != nil || !ok {
		t.Fatalf("the refused removal must not drop role-y: %v %v", ok, err)
	}
}
