package ui_test

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	if _, err := st.CreateAgent(ctx, "alpha", "alpha", "codex", "alpha-host", "/tmp/proj"); err != nil {
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
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(frag), "hello from the panel") || !strings.Contains(string(frag), "you") {
		t.Fatalf("post fragment: %d %s", resp.StatusCode, string(frag))
	}

	if _, err := st.GetAgent(ctx, "you"); err != nil {
		t.Fatalf("you agent should be auto-created: %v", err)
	}
	ok, err := st.IsParticipant(ctx, "s1", "you")
	if err != nil || !ok {
		t.Fatalf("you should be auto-joined: %v %v", ok, err)
	}
	msgs, err := st.SessionLatestMessages(ctx, "s1", 10)
	if err != nil {
		t.Fatalf("latest messages: %v", err)
	}
	var hasJoin, hasPost bool
	for _, m := range msgs {
		if m.Kind == "system" && strings.Contains(m.Content, "you joined") {
			hasJoin = true
		}
		if m.SenderID == "you" && m.Content == "hello from the panel" {
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
	if err != nil || item.Value != "panel-ok" || item.AuthorID != "you" {
		t.Fatalf("context item: %v %+v", err, item)
	}

	_ = h
}

func TestDashboardShowsRegistrationInfo(t *testing.T) {
	srv, st, _, adminTok := newPanel(t)
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "codex-laptop", "codex-laptop", "codex", "laptop.local", "/Users/andrew/proj"); err != nil {
		t.Fatalf("create agent: %v", err)
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
	for _, want := range []string{"codex-laptop", "laptop.local", "/Users/andrew/proj", "Working directory"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("dashboard missing %q in:\n%s", want, string(body))
		}
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
	if _, err := st.CreateAgent(ctx, "alpha", "alpha", "codex", "alpha-host", "/tmp/proj"); err != nil {
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

func TestArchivedSessionRejectsPosts(t *testing.T) {
	srv, st, _, adminTok := newPanel(t)
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "alpha", "alpha", "codex", "alpha-host", "/tmp/proj"); err != nil {
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
