package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"butler/internal/board"
	"butler/internal/store"
)

// taskJSON is a task plus the optional bits a write response carries:
// the seq of the mention that announced it, and (on GET) its comment log.
type taskJSON struct {
	store.Task
	NotifySeq int64 `json:"notify_seq,omitempty"`
}

type commentJSON struct {
	store.TaskComment
	NotifySeq int64 `json:"notify_seq,omitempty"`
}

func (a *API) writeBoardErr(w http.ResponseWriter, err error) {
	var be *board.Error
	if errors.As(err, &be) {
		writeErr(w, be.Code, be.Msg)
		return
	}
	a.writeStoreErr(w, err)
}

// actor is who a task write is attributed to. The admin token acts as the
// panel human; an instance token acts as that agent. Kind tokens are rejected
// by requireAuth. The admin row is created on demand so a fresh database
// can still open a task before the panel has been used.
func (a *API) actor(w http.ResponseWriter, r *http.Request) (string, bool, bool) {
	ident, ok := a.requireAuth(w, r)
	if !ok {
		return "", false, false
	}
	if ident.Admin {
		if _, err := a.Store.GetAgent(r.Context(), adminAgentID); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				writeErr(w, http.StatusInternalServerError, "store error")
				return "", false, false
			}
			if _, err := a.Store.CreateAgent(r.Context(), adminAgentID, "Admin", "human", "", "", ""); err != nil {
				writeErr(w, http.StatusInternalServerError, "store error")
				return "", false, false
			}
		}
		return adminAgentID, true, true
	}
	return ident.Agent.ID, false, true
}

func (a *API) createTask(w http.ResponseWriter, r *http.Request) {
	actor, _, ok := a.actor(w, r)
	if !ok {
		return
	}
	var req struct {
		ID          string   `json:"id"`
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Status      string   `json:"status"`
		Priority    string   `json:"priority"`
		Assignee    string   `json:"assignee"`
		SessionID   string   `json:"session_id"`
		Chain       []string `json:"chain"`
		Reviewer    string   `json:"reviewer"`
	}
	if !decodeJSON(w, r, &req, maxMessageBytes) {
		return
	}
	res, err := a.Board.Create(r.Context(), actor, board.CreateInput{
		ID: req.ID, Title: req.Title, Description: req.Description,
		Status: req.Status, Priority: req.Priority,
		Assignee: req.Assignee, SessionID: req.SessionID,
		Chain: req.Chain, Reviewer: req.Reviewer,
	})
	if err != nil {
		a.writeBoardErr(w, err)
		return
	}
	status := http.StatusCreated
	if !res.Created {
		status = http.StatusOK
	}
	writeJSON(w, status, taskJSON{Task: *res.Task, NotifySeq: res.NotifySeq})
}

func (a *API) listTasks(w http.ResponseWriter, r *http.Request) {
	actor, _, ok := a.actor(w, r)
	if !ok {
		return
	}
	f := store.TaskFilter{
		Status:    strings.TrimSpace(r.URL.Query().Get("status")),
		SessionID: strings.TrimSpace(r.URL.Query().Get("session")),
		CreatorID: strings.TrimSpace(r.URL.Query().Get("creator")),
	}
	if f.Status != "" && !store.ValidTaskStatus(f.Status) {
		writeErr(w, http.StatusBadRequest, "status must be one of todo, in_progress, blocked, review, done")
		return
	}
	assignee := strings.TrimSpace(r.URL.Query().Get("assignee"))
	switch assignee {
	case "":
	case "unassigned":
		f.Unassigned = true
	case "me":
		f.AssigneeID = actor
	default:
		resolved, rerr := a.resolveAssignee(r, assignee)
		if rerr != nil {
			a.writeBoardErr(w, rerr)
			return
		}
		f.AssigneeID = resolved
	}
	tasks, err := a.Store.ListTasks(r.Context(), f)
	if err != nil {
		a.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

// resolveAssignee maps a list/create token the same way the board service
// does, so a bad filter is a 400 rather than an empty list.
func (a *API) resolveAssignee(r *http.Request, token string) (string, error) {
	ag, err := a.Store.ResolveAssignee(r.Context(), token)
	if err == nil {
		return ag.ID, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		shown := strings.TrimPrefix(strings.TrimSpace(token), "@")
		return "", &board.Error{Code: http.StatusBadRequest, Msg: "no agent matches assignee " + strconv.Quote(shown)}
	}
	if errors.Is(err, store.ErrConflict) {
		msg := strings.TrimPrefix(err.Error(), store.ErrConflict.Error()+": ")
		return "", &board.Error{Code: http.StatusBadRequest, Msg: msg}
	}
	return "", err
}

func (a *API) getTask(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := a.actor(w, r); !ok {
		return
	}
	task, err := a.Store.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeStoreErr(w, err)
		return
	}
	comments, err := a.Store.ListTaskComments(r.Context(), task.ID)
	if err != nil {
		a.writeStoreErr(w, err)
		return
	}
	// Always a JSON array, including when the log is empty — clients
	// parse it into a list.
	writeJSON(w, http.StatusOK, struct {
		store.Task
		Comments []store.TaskComment `json:"comments"`
	}{Task: *task, Comments: comments})
}

func (a *API) patchTask(w http.ResponseWriter, r *http.Request) {
	actor, admin, ok := a.actor(w, r)
	if !ok {
		return
	}
	var req struct {
		Title       *string   `json:"title"`
		Description *string   `json:"description"`
		Status      *string   `json:"status"`
		Priority    *string   `json:"priority"`
		Assignee    *string   `json:"assignee"`
		SessionID   *string   `json:"session_id"`
		Chain       *[]string `json:"chain"`
		Reviewer    *string   `json:"reviewer"`
	}
	if !decodeJSON(w, r, &req, maxMessageBytes) {
		return
	}
	res, err := a.Board.Update(r.Context(), actor, r.PathValue("id"), board.UpdateInput{
		Title: req.Title, Description: req.Description, Status: req.Status,
		Priority: req.Priority, Assignee: req.Assignee, SessionID: req.SessionID,
		Chain: req.Chain, Reviewer: req.Reviewer,
	}, admin)
	if err != nil {
		a.writeBoardErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, taskJSON{Task: *res.Task, NotifySeq: res.NotifySeq})
}

func (a *API) commentTask(w http.ResponseWriter, r *http.Request) {
	actor, _, ok := a.actor(w, r)
	if !ok {
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if !decodeJSON(w, r, &req, maxMessageBytes) {
		return
	}
	res, err := a.Board.Comment(r.Context(), actor, r.PathValue("id"), req.Body)
	if err != nil {
		a.writeBoardErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, commentJSON{TaskComment: *res.Comment, NotifySeq: res.NotifySeq})
}

func (a *API) claimTask(w http.ResponseWriter, r *http.Request) {
	actor, _, ok := a.actor(w, r)
	if !ok {
		return
	}
	res, err := a.Board.Claim(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		a.writeBoardErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, taskJSON{Task: *res.Task, NotifySeq: res.NotifySeq})
}

func (a *API) handoffTask(w http.ResponseWriter, r *http.Request) {
	actor, admin, ok := a.actor(w, r)
	if !ok {
		return
	}
	var req struct {
		To   string `json:"to"`
		Note string `json:"note"`
	}
	if !decodeOptionalJSON(w, r, &req) {
		return
	}
	res, err := a.Board.Handoff(r.Context(), actor, r.PathValue("id"), req.To, req.Note, admin)
	if err != nil {
		a.writeBoardErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, taskJSON{Task: *res.Task, NotifySeq: res.NotifySeq})
}

func (a *API) approveTask(w http.ResponseWriter, r *http.Request) {
	actor, admin, ok := a.actor(w, r)
	if !ok {
		return
	}
	res, err := a.Board.Approve(r.Context(), actor, r.PathValue("id"), admin)
	if err != nil {
		a.writeBoardErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, taskJSON{Task: *res.Task, NotifySeq: res.NotifySeq})
}

func (a *API) rejectTask(w http.ResponseWriter, r *http.Request) {
	actor, admin, ok := a.actor(w, r)
	if !ok {
		return
	}
	var req struct {
		To   string `json:"to"`
		Note string `json:"note"`
	}
	if !decodeOptionalJSON(w, r, &req) {
		return
	}
	res, err := a.Board.Reject(r.Context(), actor, r.PathValue("id"), req.To, req.Note, admin)
	if err != nil {
		a.writeBoardErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, taskJSON{Task: *res.Task, NotifySeq: res.NotifySeq})
}

// decodeOptionalJSON accepts an empty body as an empty value. Handoff and
// reject are valid with no fields.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	err := json.NewDecoder(r.Body).Decode(v)
	if err == nil || errors.Is(err, io.EOF) {
		return true
	}
	writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
	return false
}

func (a *API) listWebhooks(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	hooks, err := a.Store.ListWebhooks(r.Context())
	if err != nil {
		a.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhooks": hooks})
}

func (a *API) putAgentWebhook(w http.ResponseWriter, r *http.Request) {
	ident, ok := a.requireAuth(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !ident.Admin && (ident.Agent == nil || ident.Agent.ID != id) {
		writeErr(w, http.StatusForbidden, "you can only set your own webhook")
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if !decodeJSON(w, r, &req, 8<<10) {
		return
	}
	if err := board.ValidWebhookURL(strings.TrimSpace(req.URL)); err != nil {
		a.writeBoardErr(w, err)
		return
	}
	hook, err := a.Store.SetAgentWebhook(r.Context(), id, strings.TrimSpace(req.URL))
	if err != nil {
		a.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hook)
}

func (a *API) deleteAgentWebhook(w http.ResponseWriter, r *http.Request) {
	ident, ok := a.requireAuth(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !ident.Admin && (ident.Agent == nil || ident.Agent.ID != id) {
		writeErr(w, http.StatusForbidden, "you can only delete your own webhook")
		return
	}
	if err := a.Store.DeleteAgentWebhook(r.Context(), id); err != nil {
		a.writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) putKindWebhook(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if !decodeJSON(w, r, &req, 8<<10) {
		return
	}
	if err := board.ValidWebhookURL(strings.TrimSpace(req.URL)); err != nil {
		a.writeBoardErr(w, err)
		return
	}
	hook, err := a.Store.SetKindWebhook(r.Context(), r.PathValue("id"), strings.TrimSpace(req.URL))
	if err != nil {
		a.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hook)
}

func (a *API) deleteKindWebhook(w http.ResponseWriter, r *http.Request) {
	if !a.requireAdmin(w, r) {
		return
	}
	if err := a.Store.DeleteKindWebhook(r.Context(), r.PathValue("id")); err != nil {
		a.writeStoreErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
