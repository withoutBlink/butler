package ui

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"butler/internal/auth"
	"butler/internal/hub"
	"butler/internal/ids"
	"butler/internal/store"
)

//go:embed templates static
var files embed.FS

const (
	cookieName   = "butler_panel"
	sessionTTL   = 30 * 24 * time.Hour
	youAgentID   = "you"
	maxPostBytes = 64 << 10
)

type Panel struct {
	Store      *store.Store
	Hub        *hub.Hub
	Log        *slog.Logger
	AdminToken string

	sessions  *sessionStore
	templates *template.Template
	Heartbeat time.Duration
}

type sessionStore struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{m: make(map[string]time.Time)}
}

func (s *sessionStore) issue() string {
	id := ids.New()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = time.Now().Add(sessionTTL)
	return id
}

func (s *sessionStore) valid(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[id]
	if ok && time.Now().Before(exp) {
		s.m[id] = time.Now().Add(sessionTTL)
		return true
	}
	delete(s.m, id)
	return false
}

func (s *sessionStore) revoke(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

func New(st *store.Store, h *hub.Hub, adminToken string, log *slog.Logger) (*Panel, error) {
	funcs := template.FuncMap{
		"ts": func(ms int64) string { return time.UnixMilli(ms).UTC().Format("15:04:05") },
		"short": func(s string) string {
			r := []rune(s)
			if len(r) > 80 {
				return string(r[:80]) + "…"
			}
			return s
		},
		"path": func(s string) string {
			r := []rune(s)
			if len(r) > 56 {
				return string(r[:56]) + "…"
			}
			return s
		},
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(files, "templates/*.tmpl")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Panel{
		Store:      st,
		Hub:        h,
		Log:        log,
		AdminToken: adminToken,
		sessions:   newSessionStore(),
		templates:  tmpl,
		Heartbeat:  15 * time.Second,
	}, nil
}

func (p *Panel) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /panel", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/panel/", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /panel/static/", p.static)
	mux.HandleFunc("GET /panel/login", p.loginForm)
	mux.HandleFunc("POST /panel/login", p.loginSubmit)
	mux.HandleFunc("POST /panel/logout", p.logout)
	mux.HandleFunc("GET /panel/", p.dashboard)
	mux.HandleFunc("GET /panel/sessions/{id}", p.sessionView)
	mux.HandleFunc("POST /panel/sessions/{id}/messages", p.postMessage)
	mux.HandleFunc("POST /panel/sessions/{id}/context", p.putContext)
	mux.HandleFunc("GET /panel/sessions/{id}/stream", p.stream)
}

func (p *Panel) requirePanel(w http.ResponseWriter, r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err == nil && p.sessions.valid(c.Value) {
		return true
	}
	http.Redirect(w, r, "/panel/login", http.StatusSeeOther)
	return false
}

func (p *Panel) static(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.StripPrefix("/panel/static/", http.FileServer(http.FS(sub))).ServeHTTP(w, r)
}

func (p *Panel) loginForm(w http.ResponseWriter, r *http.Request) {
	p.render(w, http.StatusOK, "login.tmpl", map[string]string{"Error": ""})
}

func (p *Panel) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin login rejected", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(r.PostFormValue("token"))
	if token == "" || !auth.Equal(token, p.AdminToken) {
		p.render(w, http.StatusUnauthorized, "login.tmpl", map[string]string{"Error": "invalid admin token"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    p.sessions.issue(),
		Path:     "/panel",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
	})
	http.Redirect(w, r, "/panel/", http.StatusSeeOther)
}

func (p *Panel) logout(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin rejected", http.StatusForbidden)
		return
	}
	if c, err := r.Cookie(cookieName); err == nil {
		p.sessions.revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/panel", MaxAge: -1})
	http.Redirect(w, r, "/panel/login", http.StatusSeeOther)
}

type dashSessionRow struct {
	Session      store.Session
	Participants []store.Agent
	LastPreview  string
	LastAt       *int64
}

type dashData struct {
	Sessions []dashSessionRow
	Agents   []agentRow
}

type agentRow struct {
	ID       string
	Kind     string
	Hostname string
	Workdir  string
	Online   bool
	LastTxt  string
}

func (p *Panel) dashboard(w http.ResponseWriter, r *http.Request) {
	if !p.requirePanel(w, r) {
		return
	}
	ctx := r.Context()
	sessions, err := p.Store.ListSessions(ctx, "")
	if err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	rows := make([]dashSessionRow, 0, len(sessions))
	for _, sess := range sessions {
		row := dashSessionRow{Session: sess}
		if parts, err := p.Store.Participants(ctx, sess.ID); err == nil {
			row.Participants = parts
		}
		if msgs, err := p.Store.SessionLatestMessages(ctx, sess.ID, 1); err == nil && len(msgs) == 1 {
			row.LastPreview = msgs[0].Content
			at := msgs[0].CreatedAt
			row.LastAt = &at
		}
		rows = append(rows, row)
	}
	agents, err := p.Store.ListAgents(ctx)
	if err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	agentRows := make([]agentRow, 0, len(agents))
	for _, a := range agents {
		row := agentRow{ID: a.ID, Kind: a.Kind, Hostname: a.Hostname, Workdir: a.Workdir, Online: online(a.LastSeenAt), LastTxt: "never"}
		if a.LastSeenAt != nil {
			row.LastTxt = time.Unix(*a.LastSeenAt, 0).UTC().Format("15:04:05")
		}
		agentRows = append(agentRows, row)
	}
	p.render(w, http.StatusOK, "dashboard.tmpl", dashData{Sessions: rows, Agents: agentRows})
}

type sessionViewData struct {
	Session      store.Session
	Participants []store.Agent
	Messages     []store.Message
	ContextItems []store.ContextItem
	LatestSeq    int64
}

func (p *Panel) sessionView(w http.ResponseWriter, r *http.Request) {
	if !p.requirePanel(w, r) {
		return
	}
	sid := r.PathValue("id")
	ctx := r.Context()
	sess, err := p.Store.GetSession(ctx, sid)
	if err != nil {
		p.httpError(w, "session not found", 404)
		return
	}
	msgs, err := p.Store.SessionLatestMessages(ctx, sid, 200)
	if err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	parts, err := p.Store.Participants(ctx, sid)
	if err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	items, err := p.Store.ListContext(ctx, sid)
	if err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	var latest int64
	if len(msgs) > 0 {
		latest = msgs[len(msgs)-1].Seq
	}
	p.render(w, http.StatusOK, "session.tmpl", sessionViewData{
		Session: *sess, Participants: parts, Messages: msgs, ContextItems: items, LatestSeq: latest,
	})
}

func (p *Panel) postMessage(w http.ResponseWriter, r *http.Request) {
	if !p.requirePanel(w, r) {
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin rejected", http.StatusForbidden)
		return
	}
	sid := r.PathValue("id")
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, maxPostBytes+1024)
	if err := parsePanelForm(r); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	content := strings.TrimSpace(r.PostFormValue("content"))
	if content == "" {
		http.Error(w, "content is required", http.StatusBadRequest)
		return
	}
	if len(content) > maxPostBytes {
		http.Error(w, "content exceeds 64KB limit", http.StatusRequestEntityTooLarge)
		return
	}
	sess, err := p.Store.GetSession(ctx, sid)
	if err != nil {
		p.httpError(w, "session not found", 404)
		return
	}
	if sess.Status != "active" {
		http.Error(w, "session is "+sess.Status, http.StatusConflict)
		return
	}
	you, err := p.youAgent(ctx)
	if err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	if ok, err := p.Store.IsParticipant(ctx, sid, you.ID); err == nil && !ok {
		if _, err := p.Store.JoinSession(ctx, sid, you.ID); err != nil {
			p.httpError(w, "store error", 500)
			return
		}
		p.fanout(ctx, sid)
	}
	msg, err := p.Store.InsertMessage(ctx, &store.Message{
		ID: ids.New(), SessionID: sid, SenderID: you.ID, Kind: "chat", Content: content,
	})
	if err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	p.fanout(ctx, sid)
	p.renderFragment(w, "message", msg)
}

func (p *Panel) putContext(w http.ResponseWriter, r *http.Request) {
	if !p.requirePanel(w, r) {
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin rejected", http.StatusForbidden)
		return
	}
	sid := r.PathValue("id")
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, maxPostBytes+1024)
	if err := parsePanelForm(r); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(r.PostFormValue("key"))
	value := strings.TrimSpace(r.PostFormValue("value"))
	if key == "" || value == "" {
		http.Error(w, "key and value are required", http.StatusBadRequest)
		return
	}
	if len(value) > maxPostBytes {
		http.Error(w, "value exceeds 64KB limit", http.StatusRequestEntityTooLarge)
		return
	}
	sess, err := p.Store.GetSession(ctx, sid)
	if err != nil {
		p.httpError(w, "session not found", 404)
		return
	}
	if sess.Status != "active" {
		http.Error(w, "session is "+sess.Status, http.StatusConflict)
		return
	}
	you, err := p.youAgent(ctx)
	if err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	if _, _, err := p.Store.UpsertContext(ctx, sid, key, value, you.ID); err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	p.fanout(ctx, sid)
	http.Redirect(w, r, "/panel/sessions/"+sid, http.StatusSeeOther)
}

func (p *Panel) stream(w http.ResponseWriter, r *http.Request) {
	if !p.requirePanel(w, r) {
		return
	}
	sid := r.PathValue("id")
	if _, err := p.Store.GetSession(r.Context(), sid); err != nil {
		p.httpError(w, "session not found", 404)
		return
	}
	after := int64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			after = n
		}
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		p.httpError(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	cursor := after
	ticker := time.NewTicker(p.Heartbeat)
	defer ticker.Stop()
	for {
		msgs, err := p.Store.MessagesSince(ctx, "", sid, cursor, 500)
		if err != nil {
			return
		}
		for _, m := range msgs {
			writeSSE(w, flusher, "message", p.fragment("message", m))
			cursor = m.Seq
		}
		ch, cancel := p.Hub.SubscribeSession(sid)
		msgs, err = p.Store.MessagesSince(ctx, "", sid, cursor, 500)
		if err != nil {
			cancel()
			return
		}
		for _, m := range msgs {
			writeSSE(w, flusher, "message", p.fragment("message", m))
			cursor = m.Seq
		}
		select {
		case <-ch:
			cancel()
			if p.Hub.Closed() {
				return
			}
		case <-ticker.C:
			cancel()
			if parts, err := p.Store.Participants(ctx, sid); err == nil {
				writeSSE(w, flusher, "presence", p.fragment("participants", parts))
			} else {
				writeSSEPing(w, flusher)
			}
		case <-ctx.Done():
			cancel()
			return
		}
	}
}

func (p *Panel) fanout(ctx context.Context, sid string) {
	p.Hub.NotifySession(sid)
	participants, err := p.Store.Participants(ctx, sid)
	if err != nil {
		p.Log.Error("list participants", "err", err, "session", sid)
		return
	}
	agentIDs := make([]string, 0, len(participants))
	for _, a := range participants {
		agentIDs = append(agentIDs, a.ID)
	}
	p.Hub.Notify(agentIDs)
}

func (p *Panel) youAgent(ctx context.Context) (*store.Agent, error) {
	a, err := p.Store.GetAgent(ctx, youAgentID)
	if errors.Is(err, store.ErrNotFound) {
		return p.Store.CreateAgent(ctx, youAgentID, "You", "human", "", "")
	}
	return a, err
}

func (p *Panel) render(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := p.templates.ExecuteTemplate(w, name, data); err != nil {
		p.Log.Error("render template", "template", name, "err", err)
	}
}

func (p *Panel) renderFragment(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = p.templates.ExecuteTemplate(w, name, data)
}

func (p *Panel) fragment(name string, data any) string {
	var sb strings.Builder
	if err := p.templates.ExecuteTemplate(&sb, name, data); err != nil {
		p.Log.Error("render fragment", "fragment", name, "err", err)
		return ""
	}
	return strings.TrimSpace(sb.String())
}

func (p *Panel) httpError(w http.ResponseWriter, msg string, status int) {
	http.Error(w, msg, status)
}

func writeSSE(w io.Writer, flusher http.Flusher, event, data string) {
	fmt.Fprintf(w, "event: %s\n", event)
	for _, line := range strings.Split(data, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
	flusher.Flush()
}

func writeSSEPing(w io.Writer, flusher http.Flusher) {
	fmt.Fprint(w, ": ping\n\n")
	flusher.Flush()
}

func online(lastSeen *int64) bool {
	return lastSeen != nil && time.Since(time.Unix(*lastSeen, 0)) < 90*time.Second
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

// parsePanelForm fills r.PostForm for both application/x-www-form-urlencoded
// (plain form posts) and multipart/form-data (fetch with FormData) bodies.
// r.ParseForm alone silently ignores multipart bodies, yielding an empty form.
func parsePanelForm(r *http.Request) error {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return r.ParseMultipartForm(maxPostBytes + 1024)
	}
	return r.ParseForm()
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
