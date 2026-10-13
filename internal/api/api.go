package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"butler/internal/auth"
	"butler/internal/hub"
	"butler/internal/ids"
	"butler/internal/metrics"
	"butler/internal/store"
	"butler/internal/ui"
)

const (
	maxMessageBytes = 64 << 10
	maxContextBytes = 256 << 10
	defaultWaitSecs = 25
	maxWaitSecs     = 60
	presenceWindow  = 90 * time.Second
)

var (
	agentIDRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	contextKeyRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
)

func ValidAgentID(s string) bool {
	return agentIDRe.MatchString(s)
}

type ctxKeyT int

const (
	requestIDKey ctxKeyT = iota
	identityKey
)

type Identity struct {
	Agent *store.Agent
	Admin bool
}

type API struct {
	Store      *store.Store
	Hub        *hub.Hub
	AdminToken string
	Log        *slog.Logger
	Metrics    *metrics.Metrics
	Done       <-chan struct{}
	Panel      *ui.Panel
}

type agentJSON struct {
	store.Agent
	Online bool   `json:"online"`
	Token  string `json:"token,omitempty"`
}

func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, a.wrap(h))
	}
	route("GET /api/v1/agents", a.listAgents)
	route("POST /api/v1/agents", a.createAgent)
	route("POST /api/v1/agents/{id}/rotate", a.rotateAgent)
	route("GET /api/v1/sessions", a.listSessions)
	route("POST /api/v1/sessions", a.createSession)
	route("GET /api/v1/sessions/{id}", a.getSession)
	route("PATCH /api/v1/sessions/{id}", a.patchSession)
	route("POST /api/v1/sessions/{id}/join", a.joinSession)
	route("GET /api/v1/sessions/{id}/messages", a.sessionMessages)
	route("POST /api/v1/sessions/{id}/messages", a.postMessage)
	route("GET /api/v1/sessions/{id}/context", a.listContext)
	route("GET /api/v1/sessions/{id}/context/{key}", a.getContextItem)
	route("PUT /api/v1/sessions/{id}/context/{key}", a.putContextItem)
	route("GET /api/v1/messages", a.catchupMessages)
	route("GET /api/v1/seq", a.latestSeq)
	route("GET /api/v1/wait", a.waitMessages)
	route("GET /metrics", a.getMetrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	if a.Panel != nil {
		a.Panel.RegisterRoutes(mux)
	}
	return mux
}

func (a *API) wrap(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := ids.New()[:8]
		lw := &respWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if rec := recover(); rec != nil {
				a.Log.Error("panic recovered", "rid", rid, "panic", rec)
				writeErr(lw, http.StatusInternalServerError, "internal error")
			}
		}()
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey, rid))
		r = a.authenticate(r)
		a.Metrics.Requests.Add(1)
		h(lw, r)
		if r.URL.Path != "/healthz" {
			ident, _ := identity(r)
			agentID := ""
			if ident.Agent != nil {
				agentID = ident.Agent.ID
			}
			a.Log.Info("http", "rid", rid, "method", r.Method, "path", r.URL.Path,
				"status", lw.status, "dur", time.Since(start).Round(time.Millisecond),
				"agent", agentID, "admin", ident.Admin)
		}
	}
}

func (a *API) authenticate(r *http.Request) *http.Request {
	tok := bearer(r)
	if tok == "" {
		return r
	}
	if auth.Equal(tok, a.AdminToken) {
		return r.WithContext(context.WithValue(r.Context(), identityKey, Identity{Admin: true}))
	}
	agent, err := a.Store.GetAgentByTokenHash(r.Context(), auth.Hash(tok))
	if err != nil {
		return r
	}
	a.Store.TouchAgent(r.Context(), agent.ID)
	return r.WithContext(context.WithValue(r.Context(), identityKey, Identity{Agent: agent}))
}

func identity(r *http.Request) (Identity, bool) {
	v, ok := r.Context().Value(identityKey).(Identity)
	return v, ok
}

func (a *API) requireAuth(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	ident, ok := identity(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "missing or invalid token")
		return Identity{}, false
	}
	return ident, true
}

func (a *API) requireAgent(w http.ResponseWriter, r *http.Request) *store.Agent {
	ident, ok := identity(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "missing or invalid token")
		return nil
	}
	if ident.Agent == nil {
		writeErr(w, http.StatusForbidden, "agent token required (admin token not accepted here)")
		return nil
	}
	return ident.Agent
}

func (a *API) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	ident, ok := identity(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "missing or invalid token")
		return false
	}
	if !ident.Admin {
		writeErr(w, http.StatusForbidden, "admin token required")
		return false
	}
	return true
}

func (a *API) sessionReadAllowed(w http.ResponseWriter, r *http.Request, sid string) bool {
	ident, ok := identity(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "missing or invalid token")
		return false
	}
	if ident.Admin {
		return true
	}
	okp, err := a.Store.IsParticipant(r.Context(), sid, ident.Agent.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store error")
		return false
	}
	if !okp {
		writeErr(w, http.StatusForbidden, "not a participant")
		return false
	}
	return true
}

func (a *API) writeStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict")
	default:
		writeErr(w, http.StatusInternalServerError, "store error")
	}
}

func (a *API) notifySession(ctx context.Context, sid string) {
	a.Hub.NotifySession(sid)
	participants, err := a.Store.Participants(ctx, sid)
	if err != nil {
		a.Log.Error("list participants", "err", err, "session", sid)
		return
	}
	ids := make([]string, 0, len(participants))
	for _, p := range participants {
		ids = append(ids, p.ID)
	}
	a.Hub.Notify(ids)
}

func (a *API) writeMessages(w http.ResponseWriter, msgs []store.Message) {
	if msgs == nil {
		msgs = []store.Message{}
	}
	resp := struct {
		Messages  []store.Message `json:"messages"`
		LatestSeq int64           `json:"latest_seq,omitempty"`
	}{Messages: msgs}
	if len(msgs) > 0 {
		resp.LatestSeq = msgs[len(msgs)-1].Seq
	}
	writeJSON(w, http.StatusOK, resp)
}

func toAgentJSON(a *store.Agent) agentJSON {
	online := a.LastSeenAt != nil && time.Since(time.Unix(*a.LastSeenAt, 0)) < presenceWindow
	return agentJSON{Agent: *a, Online: online}
}

func (a *API) listAgents(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.requireAuth(w, r); !ok {
		return
	}
	agents, err := a.Store.ListAgents(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store error")
		return
	}
	out := make([]agentJSON, 0, len(agents))
	for i := range agents {
		out = append(out, toAgentJSON(&agents[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
}

func (a *API) createAgent(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	var req struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		Kind        string `json:"kind"`
	}
	if !decodeJSON(w, r, &req, 64<<10) {
		return
	}
	if !ValidAgentID(req.ID) {
		writeErr(w, http.StatusBadRequest, "invalid agent id: lowercase letters, digits, - and _, max 32 chars, must start with letter/digit")
		return
	}
	display := strings.TrimSpace(req.DisplayName)
	if display == "" {
		display = req.ID
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = "other"
	}
	tok := auth.NewToken()
	agent, err := a.Store.CreateAgent(r.Context(), req.ID, display, kind, auth.Hash(tok))
	if err != nil {
		a.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, agentJSON{Agent: *agent, Online: true, Token: tok})
}

func (a *API) rotateAgent(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	tok := auth.NewToken()
	if err := a.Store.RotateAgentToken(r.Context(), id, auth.Hash(tok)); err != nil {
		a.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "token": tok})
}

func (a *API) listSessions(w http.ResponseWriter, r *http.Request) {
	ident, ok := a.requireAuth(w, r)
	if !ok {
		return
	}
	joinedBy := ""
	if r.URL.Query().Get("joined") == "1" {
		if ident.Agent == nil {
			writeErr(w, http.StatusForbidden, "joined=1 requires an agent token")
			return
		}
		joinedBy = ident.Agent.ID
	}
	sessions, err := a.Store.ListSessions(r.Context(), joinedBy)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store error")
		return
	}
	if sessions == nil {
		sessions = []store.Session{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (a *API) createSession(w http.ResponseWriter, r *http.Request) {
	agent := a.requireAgent(w, r)
	if agent == nil {
		return
	}
	var req struct {
		Title        string   `json:"title"`
		Participants []string `json:"participants"`
	}
	if !decodeJSON(w, r, &req, 64<<10) {
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		writeErr(w, http.StatusBadRequest, "title is required")
		return
	}
	if len(title) > 200 {
		writeErr(w, http.StatusBadRequest, "title exceeds 200 characters")
		return
	}
	seen := map[string]bool{agent.ID: true}
	participants := []string{}
	for _, p := range req.Participants {
		p = strings.TrimSpace(p)
		if p == "" || p == agent.ID || seen[p] {
			continue
		}
		if !ValidAgentID(p) {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("invalid agent id %q", p))
			return
		}
		seen[p] = true
		participants = append(participants, p)
	}
	var unknown []string
	for _, p := range participants {
		if _, err := a.Store.GetAgent(r.Context(), p); err != nil {
			unknown = append(unknown, p)
		}
	}
	if len(unknown) > 0 {
		writeErr(w, http.StatusBadRequest, "unknown agents: "+strings.Join(unknown, ", "))
		return
	}
	sid := ids.New()
	sess, err := a.Store.CreateSession(r.Context(), sid, title, agent.ID, participants)
	if err != nil {
		a.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (a *API) getSession(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	if _, ok := a.requireAuth(w, r); !ok {
		return
	}
	sess, err := a.Store.GetSession(r.Context(), sid)
	if err != nil {
		a.writeStoreErr(w, err)
		return
	}
	if !a.sessionReadAllowed(w, r, sid) {
		return
	}
	participants, err := a.Store.Participants(r.Context(), sid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store error")
		return
	}
	out := make([]agentJSON, 0, len(participants))
	for i := range participants {
		out = append(out, toAgentJSON(&participants[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sess, "participants": out})
}

func (a *API) patchSession(w http.ResponseWriter, r *http.Request) {
	ident, ok := a.requireAuth(w, r)
	if !ok {
		return
	}
	sid := r.PathValue("id")
	var req struct {
		Status string `json:"status"`
	}
	if !decodeJSON(w, r, &req, 4<<10) {
		return
	}
	if req.Status != "active" && req.Status != "archived" {
		writeErr(w, http.StatusBadRequest, "status must be active or archived")
		return
	}
	sess, err := a.Store.GetSession(r.Context(), sid)
	if err != nil {
		a.writeStoreErr(w, err)
		return
	}
	if !ident.Admin && (ident.Agent == nil || ident.Agent.ID != sess.CreatedBy) {
		writeErr(w, http.StatusForbidden, "only the session creator or admin may change status")
		return
	}
	if err := a.Store.SetSessionStatus(r.Context(), sid, req.Status); err != nil {
		a.writeStoreErr(w, err)
		return
	}
	sess.Status = req.Status
	writeJSON(w, http.StatusOK, sess)
}

func (a *API) joinSession(w http.ResponseWriter, r *http.Request) {
	agent := a.requireAgent(w, r)
	if agent == nil {
		return
	}
	sid := r.PathValue("id")
	if _, err := a.Store.GetSession(r.Context(), sid); err != nil {
		a.writeStoreErr(w, err)
		return
	}
	joined, err := a.Store.JoinSession(r.Context(), sid, agent.ID)
	if err != nil {
		a.writeStoreErr(w, err)
		return
	}
	if joined {
		a.notifySession(r.Context(), sid)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"joined": joined})
}

func (a *API) sessionMessages(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	if _, ok := a.requireAuth(w, r); !ok {
		return
	}
	if _, err := a.Store.GetSession(r.Context(), sid); err != nil {
		a.writeStoreErr(w, err)
		return
	}
	if !a.sessionReadAllowed(w, r, sid) {
		return
	}
	var msgs []store.Message
	var err error
	if raw := r.URL.Query().Get("latest"); raw != "" {
		n, convErr := strconv.Atoi(raw)
		if convErr != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "latest must be a positive integer")
			return
		}
		if n > 500 {
			n = 500
		}
		msgs, err = a.Store.SessionLatestMessages(r.Context(), sid, n)
	} else {
		after := queryInt(r, "after", 0, 0, 1<<62)
		limit := int(queryInt(r, "limit", 200, 1, 500))
		msgs, err = a.Store.MessagesSince(r.Context(), "", sid, after, limit)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store error")
		return
	}
	a.writeMessages(w, msgs)
}

func (a *API) postMessage(w http.ResponseWriter, r *http.Request) {
	agent := a.requireAgent(w, r)
	if agent == nil {
		return
	}
	sid := r.PathValue("id")
	var req struct {
		ID      string `json:"id"`
		Content string `json:"content"`
		Kind    string `json:"kind"`
	}
	if !decodeJSON(w, r, &req, maxMessageBytes+4096) {
		return
	}
	content := strings.TrimSpace(req.Content)
	if content == "" {
		writeErr(w, http.StatusBadRequest, "content is required")
		return
	}
	if len(content) > maxMessageBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "content exceeds 64KB limit")
		return
	}
	kind := req.Kind
	if kind == "" {
		kind = "chat"
	}
	if kind != "chat" {
		writeErr(w, http.StatusBadRequest, `only kind "chat" is accepted; other kinds are server-generated`)
		return
	}
	sess, err := a.Store.GetSession(r.Context(), sid)
	if err != nil {
		a.writeStoreErr(w, err)
		return
	}
	if sess.Status != "active" {
		writeErr(w, http.StatusConflict, "session is "+sess.Status)
		return
	}
	okp, err := a.Store.IsParticipant(r.Context(), sid, agent.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store error")
		return
	}
	if !okp {
		writeErr(w, http.StatusForbidden, "not a participant; join the session first")
		return
	}
	msg, err := a.Store.InsertMessage(r.Context(), &store.Message{
		ID: req.ID, SessionID: sid, SenderID: agent.ID, Kind: kind, Content: content,
	})
	if err != nil {
		a.writeStoreErr(w, err)
		return
	}
	a.notifySession(r.Context(), sid)
	a.Metrics.MessagesPosted.Add(1)
	writeJSON(w, http.StatusCreated, msg)
}

func (a *API) listContext(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	if _, ok := a.requireAuth(w, r); !ok {
		return
	}
	if _, err := a.Store.GetSession(r.Context(), sid); err != nil {
		a.writeStoreErr(w, err)
		return
	}
	if !a.sessionReadAllowed(w, r, sid) {
		return
	}
	items, err := a.Store.ListContext(r.Context(), sid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store error")
		return
	}
	if items == nil {
		items = []store.ContextItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *API) getContextItem(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	key := r.PathValue("key")
	if _, ok := a.requireAuth(w, r); !ok {
		return
	}
	if _, err := a.Store.GetSession(r.Context(), sid); err != nil {
		a.writeStoreErr(w, err)
		return
	}
	if !a.sessionReadAllowed(w, r, sid) {
		return
	}
	item, err := a.Store.GetContextItem(r.Context(), sid, key)
	if err != nil {
		a.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (a *API) putContextItem(w http.ResponseWriter, r *http.Request) {
	agent := a.requireAgent(w, r)
	if agent == nil {
		return
	}
	sid := r.PathValue("id")
	key := r.PathValue("key")
	if !contextKeyRe.MatchString(key) {
		writeErr(w, http.StatusBadRequest, "invalid context key")
		return
	}
	var req struct {
		Value string `json:"value"`
	}
	if !decodeJSON(w, r, &req, maxContextBytes+4096) {
		return
	}
	if strings.TrimSpace(req.Value) == "" {
		writeErr(w, http.StatusBadRequest, "value is required")
		return
	}
	if len(req.Value) > maxContextBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "value exceeds 256KB limit")
		return
	}
	sess, err := a.Store.GetSession(r.Context(), sid)
	if err != nil {
		a.writeStoreErr(w, err)
		return
	}
	if sess.Status != "active" {
		writeErr(w, http.StatusConflict, "session is "+sess.Status)
		return
	}
	okp, err := a.Store.IsParticipant(r.Context(), sid, agent.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store error")
		return
	}
	if !okp {
		writeErr(w, http.StatusForbidden, "not a participant")
		return
	}
	item, notifySeq, err := a.Store.UpsertContext(r.Context(), sid, key, req.Value, agent.ID)
	if err != nil {
		a.writeStoreErr(w, err)
		return
	}
	a.notifySession(r.Context(), sid)
	a.Metrics.ContextUpdates.Add(1)
	writeJSON(w, http.StatusOK, struct {
		store.ContextItem
		NotifySeq int64 `json:"notify_seq"`
	}{ContextItem: *item, NotifySeq: notifySeq})
}

func (a *API) catchupMessages(w http.ResponseWriter, r *http.Request) {
	agent := a.requireAgent(w, r)
	if agent == nil {
		return
	}
	after := queryInt(r, "after", 0, 0, 1<<62)
	limit := int(queryInt(r, "limit", 200, 1, 500))
	sid := r.URL.Query().Get("session_id")
	if sid != "" {
		if _, err := a.Store.GetSession(r.Context(), sid); err != nil {
			a.writeStoreErr(w, err)
			return
		}
		okp, err := a.Store.IsParticipant(r.Context(), sid, agent.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "store error")
			return
		}
		if !okp {
			writeErr(w, http.StatusForbidden, "not a participant")
			return
		}
	}
	msgs, err := a.Store.MessagesSince(r.Context(), agent.ID, sid, after, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store error")
		return
	}
	a.writeMessages(w, msgs)
}

func (a *API) latestSeq(w http.ResponseWriter, r *http.Request) {
	if a.requireAgent(w, r) == nil {
		return
	}
	n, err := a.Store.LatestSeq(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"latest_seq": n})
}

func (a *API) waitMessages(w http.ResponseWriter, r *http.Request) {
	agent := a.requireAgent(w, r)
	if agent == nil {
		return
	}
	after := queryInt(r, "after", 0, 0, 1<<62)
	timeoutS := queryInt(r, "timeout", defaultWaitSecs, 1, maxWaitSecs)
	limit := int(queryInt(r, "limit", 200, 1, 500))
	timer := time.NewTimer(time.Duration(timeoutS) * time.Second)
	defer timer.Stop()
	ctx := r.Context()
	a.Metrics.WaitersActive.Add(1)
	defer a.Metrics.WaitersActive.Add(-1)
	for {
		msgs, err := a.Store.MessagesSince(ctx, agent.ID, "", after, limit)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "store error")
			return
		}
		if len(msgs) > 0 {
			a.writeMessages(w, msgs)
			return
		}
		ch, cancel := a.Hub.Subscribe(agent.ID)
		msgs, err = a.Store.MessagesSince(ctx, agent.ID, "", after, limit)
		if err != nil {
			cancel()
			writeErr(w, http.StatusInternalServerError, "store error")
			return
		}
		if len(msgs) > 0 {
			cancel()
			a.writeMessages(w, msgs)
			return
		}
		select {
		case <-ch:
			cancel()
			a.Metrics.WaitWakeups.Add(1)
		case <-timer.C:
			cancel()
			a.Metrics.WaitTimeouts.Add(1)
			a.writeMessages(w, nil)
			return
		case <-ctx.Done():
			cancel()
			return
		case <-a.Done:
			cancel()
			a.writeMessages(w, nil)
			return
		}
	}
}

func (a *API) getMetrics(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	a.Metrics.ServeHTTP(w, r)
}

type respWriter struct {
	http.ResponseWriter
	status int
}

func (w *respWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *respWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any, maxBytes int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func queryInt(r *http.Request, key string, def, min, max int64) int64 {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}
