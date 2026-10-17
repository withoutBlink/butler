package ui

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
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
	maxPostBytes = 64 << 10
)

// AdminAgentID is the fixed identity of the human on the panel (kind
// "human"). It never authenticates via the agent API — the panel acts as it
// through its login sessions — so it is treated as always online: anyone
// seeing presence is, by definition, looking at the panel.
const AdminAgentID = "admin"

type Panel struct {
	Store      *store.Store
	Hub        *hub.Hub
	Log        *slog.Logger
	AdminToken string

	sessions     *sessionStore
	templates    *template.Template
	Heartbeat    time.Duration
	assets       map[string][]byte
	assetVersion string
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
	assets, assetVersion, err := loadAssets()
	if err != nil {
		return nil, fmt.Errorf("load static assets: %w", err)
	}
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
		// asset returns a cache-busting URL: the version is a hash of the
		// embedded files, so every new binary serves fresh assets.
		"asset": func(name string) string {
			return "/panel/static/" + name + "?v=" + assetVersion
		},
		// agentOnline drives the participants sidebar dot: present within
		// the presence window, or the admin (always online — see
		// AdminAgentID).
		"agentOnline": func(a store.Agent) bool { return agentOnline(&a) },
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(files, "templates/*.tmpl")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Panel{
		Store:        st,
		Hub:          h,
		Log:          log,
		AdminToken:   adminToken,
		sessions:     newSessionStore(),
		templates:    tmpl,
		Heartbeat:    15 * time.Second,
		assets:       assets,
		assetVersion: assetVersion,
	}, nil
}

// loadAssets reads the embedded static files into memory and derives a
// version hash from their contents. Embedded files carry no modtime, so
// browsers cannot revalidate them; the hash gives every binary a distinct,
// safely immutable asset URL.
func loadAssets() (map[string][]byte, string, error) {
	assets := map[string][]byte{}
	h := sha256.New()
	err := fs.WalkDir(files, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		assets[strings.TrimPrefix(p, "static/")] = b
		h.Write([]byte(p))
		h.Write(b)
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return assets, hex.EncodeToString(h.Sum(nil))[:12], nil
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
	mux.HandleFunc("POST /panel/sessions", p.createSession)
	mux.HandleFunc("GET /panel/sessions/{id}", p.sessionView)
	mux.HandleFunc("POST /panel/sessions/{id}/join", p.joinParticipant)
	mux.HandleFunc("POST /panel/sessions/{id}/messages", p.postMessage)
	mux.HandleFunc("POST /panel/sessions/{id}/context", p.putContext)
	mux.HandleFunc("GET /panel/sessions/{id}/stream", p.stream)
	mux.HandleFunc("POST /panel/agents/{id}/revoke", p.revokeAgent)
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
	name := strings.TrimPrefix(r.URL.Path, "/panel/static/")
	content, ok := p.assets[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	ct := mime.TypeByExtension(filepath.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	if r.URL.Query().Get("v") == p.assetVersion {
		// The content is embedded in this binary and the version query
		// matches its hash: the response is immutable, cache forever.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// Unversioned URL: never let browsers serve a stale copy.
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Write(content)
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
	// Pickable reports whether any agent other than "admin" exists (i.e.
	// the new-session form has someone to pick).
	Pickable bool
}

type agentRow struct {
	ID   string
	Kind string
	// Title is the display title (the OpenCode/codex session title for
	// per-session agents, its own UUID otherwise). Sub shows the id when
	// it differs — it is what @mentions resolve against.
	Title    string
	Sub      string
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
	pickable := false
	for _, a := range agents {
		row := agentRow{ID: a.ID, Kind: a.Kind, Title: a.DisplayName, Hostname: a.Hostname, Workdir: a.Workdir, Online: agentOnline(&a), LastTxt: "never"}
		if a.DisplayName != a.ID {
			row.Sub = a.ID
		}
		if a.LastSeenAt != nil {
			row.LastTxt = time.Unix(*a.LastSeenAt, 0).UTC().Format("15:04:05")
		}
		if a.ID != AdminAgentID {
			pickable = true
		}
		agentRows = append(agentRows, row)
	}
	p.render(w, http.StatusOK, "dashboard.tmpl", dashData{Sessions: rows, Agents: agentRows, Pickable: pickable})
}

// createSession handles the dashboard "New session" form: a title plus a
// picked set of agents. The session is created by "admin" (joined
// automatically), every picked agent is added with a join notice so waiting
// agents learn they were added, and the browser lands on the session page.
// An agent can be in any number of sessions at once.
func (p *Panel) createSession(w http.ResponseWriter, r *http.Request) {
	if !p.requirePanel(w, r) {
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin rejected", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, maxPostBytes+4096)
	if err := parsePanelForm(r); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(r.PostFormValue("title"))
	if title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	if len(title) > 200 {
		http.Error(w, "title exceeds 200 characters", http.StatusBadRequest)
		return
	}
	admin, err := p.adminAgent(ctx)
	if err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	seen := map[string]bool{}
	picked := []string{}
	for _, a := range r.PostForm["agents"] {
		a = strings.TrimSpace(a)
		if a == "" || a == AdminAgentID || seen[a] {
			continue
		}
		if _, err := p.Store.GetAgent(ctx, a); err != nil {
			http.Error(w, "unknown agent "+a, http.StatusBadRequest)
			return
		}
		seen[a] = true
		picked = append(picked, a)
	}
	sid := ids.New()
	if _, err := p.Store.CreateSession(ctx, sid, title, admin.ID, nil); err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	for _, a := range picked {
		if _, err := p.Store.JoinSession(ctx, sid, a); err != nil {
			p.httpError(w, "store error", 500)
			return
		}
	}
	p.fanout(ctx, sid)
	http.Redirect(w, r, "/panel/sessions/"+sid, http.StatusSeeOther)
}

// revokeAgent is the dashboard agents-table "kick" button. It evicts the
// agent: tokens revoked AND the identity hidden from the panel and session
// participants (with a system notice per session) — a kick is visible
// immediately, not just after the 90s presence window. The row itself
// survives (history references it); a live process re-registering the
// identity revives it, so kicking a stale agent is permanent cleanup while
// kicking a live one only disconnects it until its next registration.
func (p *Panel) revokeAgent(w http.ResponseWriter, r *http.Request) {
	if !p.requirePanel(w, r) {
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin rejected", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	if id == "" || id == AdminAgentID {
		http.Redirect(w, r, "/panel/", http.StatusSeeOther)
		return
	}
	agent, err := p.Store.GetAgent(r.Context(), id)
	if err != nil {
		p.httpError(w, "store error", 404)
		return
	}
	if err := p.Store.RemoveAgent(r.Context(), id); err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	p.Log.Info("panel evicted agent", "id", id)
	p.announceEviction(r.Context(), agent)
	http.Redirect(w, r, "/panel/", http.StatusSeeOther)
}

// announceEviction posts a system message in every session the evicted agent
// participated in and wakes both subscriber kinds, so open panels and
// waiting agents see immediately why it vanished.
func (p *Panel) announceEviction(ctx context.Context, agent *store.Agent) {
	sids, err := p.Store.SessionsForAgent(ctx, agent.ID)
	if err != nil {
		p.Log.Error("list sessions for agent", "err", err, "agent", agent.ID)
		return
	}
	content := agent.ID + " was kicked by admin (tokens revoked; hidden until it re-registers)"
	for _, sid := range sids {
		if _, err := p.Store.InsertMessage(ctx, &store.Message{
			ID: ids.New(), SessionID: sid, SenderID: agent.ID, Kind: "system", Content: content,
		}); err != nil {
			p.Log.Error("announce eviction", "err", err, "session", sid)
			continue
		}
		p.fanout(ctx, sid)
	}
}

type sessionViewData struct {
	Session      store.Session
	Participants []store.Agent
	Messages     []uiMsg
	ContextItems []store.ContextItem
	LatestSeq    int64
	// Mentionables is a JSON array of @autocomplete targets for the
	// composer: every other participant's display name plus "all" (the
	// server resolves @<title> as well as @<id>, @<kind> and @all).
	Mentionables string
	// Agents is a JSON array of the session's agent participant ids and
	// Reads a JSON object of agent id -> highest read seq. app.js uses
	// these (plus SSE "reads" events) to keep the per-message read marks
	// current without a page reload.
	Agents string
	Reads  string
	// Addable are the visible agents (roles) not yet in the session — the
	// participants-area "add to meeting" picker.
	Addable []agentOption
}

// agentOption is one entry of the session view's add-participant picker.
type agentOption struct {
	ID    string
	Title string
	Kind  string
}

// addableAgents lists visible agents that are not participants yet. Hidden
// agent clients and evicted agents never show (ListAgents filters them) —
// the picker offers roles only, matching the panel's vocabulary.
func (p *Panel) addableAgents(ctx context.Context, parts []store.Agent) []agentOption {
	out := []agentOption{}
	agents, err := p.Store.ListAgents(ctx)
	if err != nil {
		p.Log.Error("list agents", "err", err)
		return out
	}
	inSession := map[string]bool{}
	for _, a := range parts {
		inSession[a.ID] = true
	}
	for _, a := range agents {
		if a.ID == AdminAgentID || inSession[a.ID] {
			continue
		}
		title := a.DisplayName
		if title == "" || title == a.ID {
			title = a.Kind
		}
		out = append(out, agentOption{ID: a.ID, Title: title, Kind: a.Kind})
	}
	return out
}

// uiMsg is a message plus the panel's read state for it.
type uiMsg struct {
	store.Message
	ReadCount int    // agent participants (≠ sender) whose read cursor covers the message
	ReadTotal int    // agent participants (≠ sender) who could read it
	Readers   string // comma-joined ids of the readers, for the mark's tooltip
}

// readState is a session's read-cursor snapshot: the agent participants and
// their highest read seq. "admin" has no cursor — the human reads in the panel.
type readState struct {
	agents []string
	marks  map[string]int64
}

func (p *Panel) loadReadState(ctx context.Context, sid string) readState {
	rs := readState{agents: []string{}, marks: map[string]int64{}}
	parts, err := p.Store.Participants(ctx, sid)
	if err != nil {
		p.Log.Error("participants", "err", err, "session", sid)
		return rs
	}
	for _, a := range parts {
		if a.ID != AdminAgentID {
			rs.agents = append(rs.agents, a.ID)
		}
	}
	marks, err := p.Store.ReadMarks(ctx, sid)
	if err != nil {
		p.Log.Error("read marks", "err", err, "session", sid)
		return rs
	}
	rs.marks = marks
	return rs
}

// attachReadState computes per-message read marks for rendering: a chat
// message counts as read for each agent participant other than the sender
// whose cursor has reached its seq. Non-chat kinds never show marks.
func attachReadState(rs readState, msgs []store.Message) []uiMsg {
	out := make([]uiMsg, len(msgs))
	for i, m := range msgs {
		vm := uiMsg{Message: m}
		if m.Kind != "chat" {
			out[i] = vm
			continue
		}
		var readers []string
		for _, a := range rs.agents {
			if a == m.SenderID {
				continue
			}
			vm.ReadTotal++
			if rs.marks[a] >= m.Seq {
				readers = append(readers, a)
			}
		}
		vm.ReadCount = len(readers)
		vm.Readers = strings.Join(readers, ", ")
		out[i] = vm
	}
	return out
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
	targets := make([]string, 0, len(parts)+1)
	targets = append(targets, "all")
	seenTarget := map[string]bool{"all": true}
	for _, pa := range parts {
		if pa.ID == AdminAgentID {
			continue
		}
		// The menu shows display names (session titles), not ids: picking
		// one writes "@<title>" and the server resolves names too. Untitled
		// identities are labeled with their id, so the id stays mentionable.
		label := pa.DisplayName
		if label == "" {
			label = pa.ID
		}
		if seenTarget[label] {
			continue
		}
		seenTarget[label] = true
		targets = append(targets, label)
	}
	mentionables, err := json.Marshal(targets)
	if err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	rs := p.loadReadState(ctx, sid)
	readsJSON, err := json.Marshal(rs.marks)
	if err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	agentsJSON, err := json.Marshal(rs.agents)
	if err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	p.render(w, http.StatusOK, "session.tmpl", sessionViewData{
		Session: *sess, Participants: parts, Messages: attachReadState(rs, msgs), ContextItems: items, LatestSeq: latest,
		Mentionables: string(mentionables), Agents: string(agentsJSON), Reads: string(readsJSON),
		Addable: p.addableAgents(ctx, parts),
	})
}

// joinParticipant is the participants-area "add" form on the session view:
// it adds one more agent (a role) to an existing meeting. JoinSession writes
// the participant row plus a "joined the session" system notice in one
// transaction; the fanout wakes the new participant's wait loop and every
// panel viewing the session. admin is not addable (it joins implicitly when
// posting) and hidden agent clients / evicted agents are rejected here —
// the picker is fed by the same visible list, this re-validates the submit.
func (p *Panel) joinParticipant(w http.ResponseWriter, r *http.Request) {
	if !p.requirePanel(w, r) {
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin rejected", http.StatusForbidden)
		return
	}
	sid := r.PathValue("id")
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := parsePanelForm(r); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	agentID := strings.TrimSpace(r.PostFormValue("agent"))
	if agentID == "" {
		http.Error(w, "agent is required", http.StatusBadRequest)
		return
	}
	if agentID == AdminAgentID {
		http.Error(w, "admin participates by posting, not by joining", http.StatusBadRequest)
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
	if _, err := p.Store.GetVisibleAgent(ctx, agentID); err != nil {
		http.Error(w, "unknown agent "+agentID, http.StatusBadRequest)
		return
	}
	if ok, err := p.Store.IsParticipant(ctx, sid, agentID); err != nil {
		p.httpError(w, "store error", 500)
		return
	} else if ok {
		http.Error(w, agentID+" is already a participant", http.StatusBadRequest)
		return
	}
	if _, err := p.Store.JoinSession(ctx, sid, agentID); err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	p.fanout(ctx, sid)
	http.Redirect(w, r, "/panel/sessions/"+sid, http.StatusSeeOther)
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
	admin, err := p.adminAgent(ctx)
	if err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	if ok, err := p.Store.IsParticipant(ctx, sid, admin.ID); err == nil && !ok {
		if _, err := p.Store.JoinSession(ctx, sid, admin.ID); err != nil {
			p.httpError(w, "store error", 500)
			return
		}
		p.fanout(ctx, sid)
	}
	msg, err := p.Store.InsertMessage(ctx, &store.Message{
		ID: ids.New(), SessionID: sid, SenderID: admin.ID, Kind: "chat", Content: content,
	})
	if err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	p.fanout(ctx, sid)
	p.renderFragment(w, "message", attachReadState(p.loadReadState(ctx, sid), []store.Message{*msg})[0])
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
	admin, err := p.adminAgent(ctx)
	if err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	if _, _, err := p.Store.UpsertContext(ctx, sid, key, value, admin.ID); err != nil {
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
	// The page embeds the read marks it was rendered with; remember them so
	// only advances get emitted as "reads" events.
	lastMarks := map[string]int64{}
	if marks, err := p.Store.ReadMarks(ctx, sid); err == nil {
		lastMarks = marks
	}
	emitReads := func() {
		marks, err := p.Store.ReadMarks(ctx, sid)
		if err != nil {
			return
		}
		for a, seq := range marks {
			if seq <= lastMarks[a] {
				continue
			}
			lastMarks[a] = seq
			if data, err := json.Marshal(map[string]any{"agent": a, "seq": seq}); err == nil {
				writeSSE(w, flusher, "reads", string(data))
			}
		}
	}
	sendMessages := func(msgs []store.Message) {
		if len(msgs) == 0 {
			return
		}
		vms := attachReadState(p.loadReadState(ctx, sid), msgs)
		for _, vm := range vms {
			writeSSE(w, flusher, "message", p.fragment("message", vm))
			cursor = vm.Seq
		}
	}
	for {
		msgs, err := p.Store.MessagesSince(ctx, "", sid, cursor, 500)
		if err != nil {
			return
		}
		sendMessages(msgs)
		ch, cancel := p.Hub.SubscribeSession(sid)
		msgs, err = p.Store.MessagesSince(ctx, "", sid, cursor, 500)
		if err != nil {
			cancel()
			return
		}
		sendMessages(msgs)
		select {
		case <-ch:
			cancel()
			if p.Hub.Closed() {
				return
			}
			emitReads()
		case <-ticker.C:
			cancel()
			if parts, err := p.Store.Participants(ctx, sid); err == nil {
				writeSSE(w, flusher, "presence", p.fragment("participants", parts))
			} else {
				writeSSEPing(w, flusher)
			}
			emitReads()
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

func (p *Panel) adminAgent(ctx context.Context) (*store.Agent, error) {
	a, err := p.Store.GetAgent(ctx, AdminAgentID)
	if errors.Is(err, store.ErrNotFound) {
		return p.Store.CreateAgent(ctx, AdminAgentID, "Admin", "human", "", "", "")
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

// agentOnline reports panel presence: seen within the presence window, or
// the admin — it never calls the agent API (the panel acts as it directly),
// yet whoever reads presence is by definition at the panel, so it counts as
// online.
func agentOnline(a *store.Agent) bool {
	return a.ID == AdminAgentID || online(a.LastSeenAt)
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
