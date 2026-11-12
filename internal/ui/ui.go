package ui

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
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
	"unicode/utf8"

	"butler/internal/auth"
	"butler/internal/board"
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
	// Board announces task changes (mention + webhook + SSE wake).
	Board *board.Service

	sessions     *sessionStore
	templates    *template.Template
	Heartbeat    time.Duration
	assets       map[string][]byte
	assetVersion string
}

type sessionStore struct {
	mu sync.Mutex
	m  map[string]sessionEntry
}

// sessionEntry is one panel login: its expiry plus the one-shot notice the
// dashboard shows on the render that follows a lifecycle action's 303 (see
// panelNotice). Per-login, not per-process, so two tabs never eat or mix up
// each other's feedback.
type sessionEntry struct {
	expires time.Time
	notice  string
}

func newSessionStore() *sessionStore {
	return &sessionStore{m: make(map[string]sessionEntry)}
}

func (s *sessionStore) issue() string {
	id := ids.New()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = sessionEntry{expires: time.Now().Add(sessionTTL)}
	return id
}

func (s *sessionStore) valid(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[id]
	if ok && time.Now().Before(e.expires) {
		e.expires = time.Now().Add(sessionTTL)
		s.m[id] = e
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

// setNotice leaves a message for the login's next dashboard render; takeNotice
// returns and clears it, so a refresh does not repeat stale feedback.
func (s *sessionStore) setNotice(id, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[id]; ok {
		e.notice = msg
		s.m[id] = e
	}
}

func (s *sessionStore) takeNotice(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[id]
	if !ok {
		return ""
	}
	msg := e.notice
	e.notice = ""
	s.m[id] = e
	return msg
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
		Board:        board.New(st, h, log),
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
	mux.HandleFunc("GET /panel/picks", p.picks)
	mux.HandleFunc("GET /panel/board", p.boardPage)
	mux.HandleFunc("GET /panel/board/stream", p.boardStream)
	mux.HandleFunc("POST /panel/board/done", p.toggleDone)
	mux.HandleFunc("POST /panel/tasks", p.createTask)
	mux.HandleFunc("POST /panel/tasks/{id}", p.updateTask)
	mux.HandleFunc("POST /panel/tasks/{id}/comment", p.commentTask)
	mux.HandleFunc("POST /panel/tasks/{id}/handoff", p.handoffTask)
	mux.HandleFunc("POST /panel/tasks/{id}/approve", p.approveTask)
	mux.HandleFunc("POST /panel/tasks/{id}/reject", p.rejectTask)
	mux.HandleFunc("POST /panel/sessions", p.createSession)
	mux.HandleFunc("GET /panel/sessions/{id}", p.sessionView)
	mux.HandleFunc("POST /panel/sessions/{id}/join", p.joinParticipant)
	mux.HandleFunc("POST /panel/sessions/{id}/participants/{agent}/remove", p.removeParticipant)
	mux.HandleFunc("POST /panel/sessions/{id}/status", p.setSessionStatus)
	mux.HandleFunc("POST /panel/sessions/{id}/delete", p.deleteSession)
	mux.HandleFunc("POST /panel/sessions/{id}/messages", p.postMessage)
	mux.HandleFunc("POST /panel/sessions/{id}/mentions", p.mentionSuggest)
	mux.HandleFunc("POST /panel/sessions/{id}/mentions/apply", p.mentionApply)
	mux.HandleFunc("POST /panel/sessions/{id}/context", p.putContext)
	mux.HandleFunc("GET /panel/sessions/{id}/stream", p.stream)
	mux.HandleFunc("POST /panel/agents/{id}/revoke", p.revokeAgent)
}

// panelToken returns the login token of a valid panel session, or "" when the
// cookie is missing/expired. requirePanel gates on it; handlers that leave a
// notice (panelNotice) need the token itself.
func (p *Panel) panelToken(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	if !p.sessions.valid(c.Value) {
		return ""
	}
	return c.Value
}

func (p *Panel) requirePanel(w http.ResponseWriter, r *http.Request) bool {
	if p.panelToken(r) != "" {
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
	if strings.HasSuffix(name, ".js") {
		ct = "text/javascript; charset=utf-8"
	}
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
	Session     store.Session
	Shown       []dashPerson
	More        []dashPerson
	MoreLabel   string
	LastPreview string
	LastAt      *int64
	// Global marks the fixed global session: no archive/reopen/delete control
	// (the store refuses both), everyone is in it.
	Global bool
}

type dashData struct {
	Sessions []dashSessionRow
	// Roster is one row per teammate identity, live and recent first.
	// Sibling roles that share kind + hostname + workdir nest under that row.
	Roster []rosterIdentity
	// Picks is the new-session list (admin omitted), ready for the filter box.
	Picks pickerModel
	// Pickable reports whether any agent other than "admin" exists (i.e.
	// the new-session form has someone to pick).
	Pickable bool
	// Notice is the one-shot feedback of the lifecycle action that redirected
	// here (archived / reopened / deleted / already gone), so the user can
	// read the outcome without guessing.
	Notice string
}

// errorData backs error.tmpl: the HTTP status's text plus a plain sentence.
type errorData struct {
	Title   string
	Message string
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
	rawParts := make([][]store.Agent, 0, len(sessions))
	for _, sess := range sessions {
		row := dashSessionRow{Session: sess, Global: sess.ID == store.GlobalSessionID}
		var parts []store.Agent
		if listed, err := p.Store.Participants(ctx, sess.ID); err == nil {
			parts = listed
		}
		if msgs, err := p.Store.SessionLatestMessages(ctx, sess.ID, 1); err == nil && len(msgs) == 1 {
			row.LastPreview = msgs[0].Content
			at := msgs[0].CreatedAt
			row.LastAt = &at
		}
		rows = append(rows, row)
		rawParts = append(rawParts, parts)
	}
	agents, err := p.Store.ListAgents(ctx)
	if err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	snap := p.loadActivity(ctx)
	for i := range rows {
		people := make([]dashPerson, len(rawParts[i]))
		for j, a := range rawParts[i] {
			people[j] = dashPerson{Agent: a, Activity: snap.ByAgent[a.ID]}
		}
		rows[i].Shown, rows[i].More, rows[i].MoreLabel = condenseParticipants(people)
	}
	roster := p.groupVisible(ctx, agents, snap.ByAgent)
	picks := filterRoster(roster, "", nil)
	p.render(w, http.StatusOK, "dashboard.tmpl", dashData{Sessions: rows, Roster: roster, Picks: picks, Pickable: picks.Total > 0, Notice: p.sessions.takeNotice(p.panelToken(r))})
}

// picks re-renders the new-session roster for the filter box. Checked agent
// ids travel with the request and come back as checkboxes, or as hidden
// inputs when the filter hides them, so creating the session still invites
// whoever was selected.
func (p *Panel) picks(w http.ResponseWriter, r *http.Request) {
	if !p.requirePanel(w, r) {
		return
	}
	ctx := r.Context()
	agents, err := p.Store.ListAgents(ctx)
	if err != nil {
		p.httpError(w, "store error", 500)
		return
	}
	selected := map[string]bool{}
	for _, id := range r.URL.Query()["agents"] {
		id = strings.TrimSpace(id)
		if id != "" {
			selected[id] = true
		}
	}
	roster := p.groupVisible(ctx, agents, p.loadActivity(ctx).ByAgent)
	p.renderFragment(w, "picks", filterRoster(roster, r.URL.Query().Get("pick"), selected))
}

func (p *Panel) groupVisible(ctx context.Context, agents []store.Agent, byAgent map[string]store.ActivityView) []rosterIdentity {
	kindNames := map[string]string{}
	if kinds, err := p.Store.ListKinds(ctx); err != nil {
		p.Log.Error("list kinds", "err", err)
	} else {
		for _, k := range kinds {
			kindNames[k.ID] = k.DisplayName
		}
	}
	return groupRoster(agents, byAgent, kindNames)
}

// createSession handles the dashboard "New session" form: a title plus a
// picked set of agents. The session is created by "admin" (joined
// automatically), every picked agent is added with a join notice so waiting
// agents learn they were added, and the browser lands on the session page.
// An agent can be in any number of sessions at once.
//
// A project checkbox posts "project:" plus the working directory. That
// expands to the live (or most recently seen) role of each teammate in the
// project — not every historical OpenCode session, and not the hidden
// app-level client. A nested teammate posts that role's id alone.
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
	picked, err := p.resolvePicks(ctx, r.PostForm["agents"])
	if err != nil {
		if errors.Is(err, errUnknownPick) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.httpError(w, "store error", 500)
		return
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

var errUnknownPick = errors.New("unknown agent")

// resolvePicks turns form values into agent ids. A project token invites the
// representative role of every teammate in that working directory.
func (p *Panel) resolvePicks(ctx context.Context, tokens []string) ([]string, error) {
	var projects map[string][]string
	for _, token := range tokens {
		if _, ok := projectWorkdir(strings.TrimSpace(token)); ok {
			agents, err := p.Store.ListAgents(ctx)
			if err != nil {
				return nil, err
			}
			projects = map[string][]string{}
			for _, row := range p.groupVisible(ctx, agents, p.loadActivity(ctx).ByAgent) {
				if !row.Grouped {
					continue
				}
				ids := make([]string, len(row.Members))
				for i, m := range row.Members {
					ids[i] = m.ID
				}
				projects[row.Workdir] = ids
			}
			break
		}
	}
	var picked []string
	seen := map[string]bool{}
	add := func(id string) {
		if id == "" || id == AdminAgentID || seen[id] {
			return
		}
		seen[id] = true
		picked = append(picked, id)
	}
	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" || token == AdminAgentID {
			continue
		}
		if wd, ok := projectWorkdir(token); ok {
			ids := projects[wd]
			if len(ids) == 0 {
				return nil, fmt.Errorf("%w: project %s", errUnknownPick, wd)
			}
			for _, id := range ids {
				add(id)
			}
			continue
		}
		if _, err := p.Store.GetAgent(ctx, token); err != nil {
			return nil, fmt.Errorf("%w: %s", errUnknownPick, token)
		}
		add(token)
	}
	return picked, nil
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
	content := store.NoticeLabel(agent) + " was kicked by admin (tokens revoked; hidden until it re-registers)"
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
	Session store.Session
	// Participants feeds the shared "participants" fragment (the sidebar):
	// the agents plus the session id and whether rows get a remove control.
	Participants participantsView
	Messages     []uiMsg
	ContextItems []store.ContextItem
	LatestSeq    int64
	// Composer is the message box. HTMX asks the server for @mention
	// suggestions as the human types; the server completes the token.
	Composer composerView
	// Addable are the visible agents (roles) not yet in the session — the
	// participants-area "add to meeting" picker.
	Addable []agentOption
	// Archived reports a frozen session (status != active): readable, but the
	// composer, add-participant picker and context form are hidden and the
	// banner points at the dashboard, where archive/reopen/delete live.
	Archived bool
}

// participantsView feeds the shared "participants" fragment — the session
// sidebar and its periodic SSE "presence" re-sends. Besides the agents it
// carries the session id and whether membership can change: each row gets a
// remove control on an active session (archived means frozen both ways, so
// the control is hidden and the store/panel refuse the request).
type participantsView struct {
	SessionID string
	CanRemove bool
	Agents    []participantRow
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

// composerView is the message box. OOB replaces the textarea after a send
// or a mention completion; Focus asks the browser to put the caret back.
type composerView struct {
	SessionID string
	Value     string
	OOB       bool
	Focus     bool
}

// messageLong is the server's stand-in for measuring overflow: a message
// with several lines, or a long single line, gets a checkbox that lifts the
// CSS clamp. Short messages stay fully visible.
func messageLong(s string) bool {
	if strings.Count(s, "\n") >= 3 {
		return true
	}
	return utf8.RuneCountInString(s) > 180
}

// uiMsg is a message plus the panel's read state for it.
type uiMsg struct {
	store.Message
	ReadCount  int                // audience members (≠ sender) whose read state covers the message
	ReadTotal  int                // audience members (≠ sender) who could read it
	Readers    string             // comma-joined display names of the readers
	Unread     string             // comma-joined display names of the audience who have not read it
	ReadTip    string             // the mark's hover text: readers and non-readers
	SenderName string             // sender's display name (session title); the id stays in data-sender/title
	Activity   store.ActivityView // derived activity shown next to the author
	Long       bool               // offer a CSS expand control; the server decides, not the browser
}

// readState is a session's read-cursor snapshot plus its display names: the
// participant audience and their highest read seq, and every participant's id →
// display name (the message list shows titles, not UUIDs).
//
// The audience is *every other participant*, the panel's human included: the
// human has no read cursor (nothing delivers messages to a human), but a mark
// is only ever visible to that human, so seeing it means having seen the
// message. That makes the human a reader without inventing a cursor for it.
type readState struct {
	members []store.AudienceMember
	marks   map[string]int64
	names   map[string]string
}

func (p *Panel) loadReadState(ctx context.Context, sid string) readState {
	rs := readState{marks: map[string]int64{}, names: map[string]string{}}
	members, err := p.Store.SessionAudience(ctx, sid)
	if err != nil {
		p.Log.Error("session audience", "err", err, "session", sid)
		return rs
	}
	rs.members = members
	for _, m := range members {
		rs.names[m.ID] = m.DisplayName
	}
	marks, err := p.Store.ReadMarks(ctx, sid)
	if err != nil {
		p.Log.Error("read marks", "err", err, "session", sid)
		return rs
	}
	rs.marks = marks
	return rs
}

// readTip is the mark's hover text: who has read the message and who has not.
// The groups are independent — the human counts as a reader whenever it can
// see the mark at all, so "not yet" only ever names agents.
func readTip(readers, unread string) string {
	switch {
	case readers != "" && unread != "":
		return "read by " + readers + " · not yet: " + unread
	case readers != "":
		return "read by " + readers
	case unread != "":
		return "not read yet: " + unread
	default:
		return ""
	}
}

// attachReadState computes per-message read marks for rendering: a chat
// message counts as read for each of the sender's audience — every other
// participant, human included — whose read state covers its seq. The human is
// always a reader (the panel is the human); a member who joined after the
// message was sent is not part of its audience at all (nobody can read history
// they were not present for, and such a message would otherwise stay below
// N/N forever). Non-chat kinds never show marks. Every message also gets the
// sender's display name — the id stays available as data-sender and the
// tooltip, and the name is looked up in the agents row when the sender is no
// longer a participant (a kicked agent's history still renders as a name, not
// a UUID). Only an identity whose display name *is* its id (an untitled
// app-level identity) shows a UUID in the list.
func (p *Panel) attachReadState(ctx context.Context, rs readState, msgs []store.Message) []uiMsg {
	snap := p.loadActivity(ctx)
	out := make([]uiMsg, len(msgs))
	nameOf := func(id string) string {
		if n, ok := rs.names[id]; ok && n != "" {
			return n
		}
		// Not a participant: resolve from the row itself (removed agents
		// keep their row) and remember it for the rest of the batch.
		if a, err := p.Store.GetAgent(ctx, id); err == nil {
			if n := store.NoticeLabel(a); n != "" {
				rs.names[id] = n
				return n
			}
		}
		rs.names[id] = id
		return id
	}
	for i, m := range msgs {
		vm := uiMsg{Message: m, SenderName: nameOf(m.SenderID), Activity: snap.ByAgent[m.SenderID], Long: messageLong(m.Content)}
		if m.Kind != "chat" {
			out[i] = vm
			continue
		}
		var readers, unread []string
		for _, a := range rs.members {
			if a.ID == m.SenderID {
				continue
			}
			// Joined after the message was sent: not in that audience.
			if a.JoinedSeq > m.Seq {
				continue
			}
			vm.ReadTotal++
			if a.ID == AdminAgentID || rs.marks[a.ID] >= m.Seq {
				readers = append(readers, nameOf(a.ID))
			} else {
				unread = append(unread, nameOf(a.ID))
			}
		}
		vm.ReadCount = len(readers)
		vm.Readers = strings.Join(readers, ", ")
		vm.Unread = strings.Join(unread, ", ")
		vm.ReadTip = readTip(vm.Readers, vm.Unread)
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
		p.httpError(w, "This session no longer exists — it may have been deleted. The sessions list shows what is still around.", 404)
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
	rs := p.loadReadState(ctx, sid)
	snap := p.loadActivity(ctx)
	shown := p.attachReadState(ctx, rs, msgs)
	// Newest first in the DOM. The log is a column-reverse flex, so the
	// newest message sits at the visual bottom and the scroll origin stays
	// there without a script. New SSE messages are inserted at the start.
	for i, j := 0, len(shown)-1; i < j; i, j = i+1, j-1 {
		shown[i], shown[j] = shown[j], shown[i]
	}
	p.render(w, http.StatusOK, "session.tmpl", sessionViewData{
		Session:      *sess,
		Participants: participantsView{SessionID: sid, CanRemove: sess.Status == "active", Agents: p.decorateParticipants(ctx, sid, parts, snap)},
		Messages:     shown, ContextItems: items, LatestSeq: latest,
		Composer: composerView{SessionID: sid},
		Addable:  p.addableAgents(ctx, parts),
		Archived: sess.Status != "active",
	})
}

// removeParticipant is the participants sidebar's per-row control: the agent
// leaves the session (or is taken out of it) without being evicted — it stays
// registered, keeps its other sessions and the add-participant picker can put
// it back. The store writes the "left"/"was removed by admin" notice and drops
// the agent's read cursor for the session; archived sessions are frozen, so the
// control is hidden then and the store would refuse anyway (409).
func (p *Panel) removeParticipant(w http.ResponseWriter, r *http.Request) {
	if !p.requirePanel(w, r) {
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin rejected", http.StatusForbidden)
		return
	}
	sid := r.PathValue("id")
	agentID := r.PathValue("agent")
	ctx := r.Context()
	if agentID == "" {
		p.httpError(w, "participant required", http.StatusBadRequest)
		return
	}
	if err := p.Store.RemoveParticipant(ctx, sid, agentID, AdminAgentID); err != nil {
		p.storeErr(w, err)
		return
	}
	p.Log.Info("panel removed participant", "session", sid, "agent", agentID)
	p.fanout(ctx, sid)
	http.Redirect(w, r, "/panel/sessions/"+sid, http.StatusSeeOther)
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

// setSessionStatus is the dashboard's Archive / Reopen icon button. Archiving
// freezes a session: the API and the panel already refuse posting, context
// writes and joins on a non-active session, so an archived room stays readable
// but silent until it is reopened. The fixed global session is refused by the
// store. Both directions land back on the dashboard, the only place the
// control lives.
func (p *Panel) setSessionStatus(w http.ResponseWriter, r *http.Request) {
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
	status := strings.TrimSpace(r.PostFormValue("status"))
	if status != "active" && status != "archived" {
		http.Error(w, "status must be active or archived", http.StatusBadRequest)
		return
	}
	// Read the session first: its title names the session in the outcome
	// notice, and a missing row is a stale dashboard (deleted in another tab,
	// or a second submit) rather than a store failure — the intent "archive
	// it" is already satisfied, so treat it as such instead of showing an
	// unexplained error page.
	sess, err := p.Store.GetSession(ctx, sid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			p.panelNotice(w, r, sessionGoneNotice)
			return
		}
		p.storeErr(w, err)
		return
	}
	if err := p.Store.SetSessionStatus(ctx, sid, status); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			p.panelNotice(w, r, sessionGoneNotice)
			return
		}
		p.storeErr(w, err)
		return
	}
	p.Log.Info("panel set session status", "id", sid, "status", status)
	p.fanout(ctx, sid)
	verb := "archived"
	if status == "active" {
		verb = "reopened"
	}
	p.panelNotice(w, r, fmt.Sprintf("Session “%s” %s.", sess.Title, verb))
}

// deleteSession is the panel's Delete control: the session and everything
// scoped to it (messages, participants, context items, read cursors) are gone
// for good — the button carries a confirm() because there is no undo, and no
// soft-removed row either. Participants' wait loops and any open stream for
// the session are woken so it stops showing up on their next poll.
func (p *Panel) deleteSession(w http.ResponseWriter, r *http.Request) {
	if !p.requirePanel(w, r) {
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin rejected", http.StatusForbidden)
		return
	}
	sid := r.PathValue("id")
	ctx := r.Context()
	// Title first (the notice names the session) and a vanished session is the
	// already-done case, not an error page — see setSessionStatus.
	sess, err := p.Store.GetSession(ctx, sid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			p.panelNotice(w, r, sessionGoneNotice)
			return
		}
		p.storeErr(w, err)
		return
	}
	participants, err := p.Store.DeleteSession(ctx, sid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			p.panelNotice(w, r, sessionGoneNotice)
			return
		}
		p.storeErr(w, err)
		return
	}
	p.Log.Info("panel deleted session", "id", sid, "participants", len(participants))
	p.Hub.NotifySession(sid)
	p.Hub.Notify(participants)
	p.panelNotice(w, r, fmt.Sprintf("Session “%s” deleted.", sess.Title))
}

// sessionGoneNotice is what a lifecycle action on a session that no longer
// exists reports: a stale dashboard row (deleted in another tab) or a repeat
// submit must leave the user informed, not on a bare "not found" page.
const sessionGoneNotice = "That session no longer exists — it may have been deleted from another tab."

// panelNotice stores msg for this login's next dashboard render and sends the
// browser back to the dashboard, so every lifecycle action ends on a page that
// states its outcome.
func (p *Panel) panelNotice(w http.ResponseWriter, r *http.Request, msg string) {
	p.sessions.setNotice(p.panelToken(r), msg)
	http.Redirect(w, r, "/panel/", http.StatusSeeOther)
}

// storeErr maps store errors onto panel responses: missing rows are 404s,
// refusals (the fixed global session, a bad status) are 409s, anything else
// is a store error.
func (p *Panel) storeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		p.httpError(w, "not found", http.StatusNotFound)
	case errors.Is(err, store.ErrConflict):
		p.httpError(w, strings.TrimPrefix(err.Error(), store.ErrConflict.Error()+": "), http.StatusConflict)
	default:
		p.httpError(w, "store error", http.StatusInternalServerError)
	}
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
	p.Store.TouchAction(ctx, admin.ID)
	p.fanout(ctx, sid)
	vm := p.attachReadState(ctx, p.loadReadState(ctx, sid), []store.Message{*msg})[0]
	// HTMX already has the stream open: the message arrives as an SSE event.
	// The response only clears the composer (and any open mention menu).
	// A plain form post still gets the message fragment.
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = p.templates.ExecuteTemplate(w, "composer", composerView{SessionID: sid, OOB: true})
		_ = p.templates.ExecuteTemplate(w, "mention-clear", nil)
		return
	}
	p.renderFragment(w, "message", vm)
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
	p.Store.TouchAction(ctx, admin.ID)
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
	// EventSource sends Last-Event-ID when it reconnects. Message events use
	// the seq as that id, so a dropped stream resumes instead of replaying.
	if raw := r.Header.Get("Last-Event-ID"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > after {
			after = n
		}
	}
	if raw := r.URL.Query().Get("after"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > after {
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
	sendMessages := func(msgs []store.Message) {
		if len(msgs) == 0 {
			return
		}
		vms := p.attachReadState(ctx, p.loadReadState(ctx, sid), msgs)
		for _, vm := range vms {
			writeSSEID(w, flusher, "message", p.fragment("message", vm), strconv.FormatInt(vm.Seq, 10))
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
			// A read advance re-renders the chips. The server applies the
			// audience rule (including a member who joined later); the
			// browser only swaps the HTML.
			p.writeLiveMeta(ctx, w, flusher, sid)
		case <-ticker.C:
			cancel()
			// The presence re-send also refreshes the participants list, so a
			// join/removal lands in every open panel within one heartbeat. A
			// session deleted under an open tab just goes on heartbeating (the
			// table is gone, so no remove controls): closing the stream would
			// make the browser's EventSource retry 404s in a loop.
			if parts, err := p.Store.Participants(ctx, sid); err == nil {
				canRemove := false
				if sess, err := p.Store.GetSession(ctx, sid); err == nil {
					canRemove = sess.Status == "active"
				}
				snap := p.loadActivity(ctx)
				writeSSE(w, flusher, "presence", p.fragment("participants", participantsView{SessionID: sid, CanRemove: canRemove, Agents: p.decorateParticipants(ctx, sid, parts, snap)}))
				p.writeLiveMeta(ctx, w, flusher, sid)
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

// httpError renders the panel's error page: the status code, one plain
// sentence and a way back to the sessions list. A browser reaches these paths
// through a stale tab, a bookmark, a back button or a link to a session that
// has since been deleted, so a bare text body ("not found") reads as a dead
// end with no hint of what happened.
func (p *Panel) httpError(w http.ResponseWriter, msg string, status int) {
	p.render(w, status, "error.tmpl", errorData{
		Title:   http.StatusText(status),
		Message: msg,
	})
}

func writeSSE(w io.Writer, flusher http.Flusher, event, data string) {
	writeSSEID(w, flusher, event, data, "")
}

func writeSSEID(w io.Writer, flusher http.Flusher, event, data, id string) {
	if id != "" {
		fmt.Fprintf(w, "id: %s\n", id)
	}
	fmt.Fprintf(w, "event: %s\n", event)
	if data == "" {
		fmt.Fprint(w, "data: \n")
	}
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
