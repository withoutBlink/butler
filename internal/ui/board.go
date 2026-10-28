package ui

import (
	"context"
	"errors"
	"net/http"
	"time"

	"butler/internal/board"
	"butler/internal/store"
)

type assignChoice struct {
	ID    string
	Label string
}

type boardNote struct {
	Author string
	Body   string
	At     int64
}

type boardCard struct {
	Task         store.Task
	Assignee     string
	Creator      string
	SessionTitle string
	Comments     []boardNote
}

type boardColumn struct {
	Status string
	Label  string
	Tasks  []boardCard
}

type boardPage struct {
	Notice   string
	Agents   []assignChoice
	Sessions []store.Session
	Columns  []boardColumn
}

func statusLabel(status string) string {
	switch status {
	case store.TaskTodo:
		return "To do"
	case store.TaskInProgress:
		return "In progress"
	case store.TaskBlocked:
		return "Blocked"
	case store.TaskReview:
		return "Review"
	case store.TaskDone:
		return "Done"
	default:
		return status
	}
}

func (p *Panel) loadBoard(ctx context.Context) (boardPage, error) {
	tasks, err := p.Store.ListTasks(ctx, store.TaskFilter{})
	if err != nil {
		return boardPage{}, err
	}
	agents, err := p.Store.ListAgents(ctx)
	if err != nil {
		return boardPage{}, err
	}
	sessions, err := p.Store.ListSessions(ctx, "")
	if err != nil {
		return boardPage{}, err
	}
	names := map[string]string{}
	var choices []assignChoice
	for _, a := range agents {
		names[a.ID] = store.NoticeLabel(&a)
		choices = append(choices, assignChoice{ID: a.ID, Label: agentChoiceLabel(&a)})
	}
	titles := map[string]string{}
	for _, sess := range sessions {
		titles[sess.ID] = sess.Title
	}
	label := func(id string) string {
		if id == "" {
			return ""
		}
		if n, ok := names[id]; ok {
			return n
		}
		ag, err := p.Store.GetAgent(ctx, id)
		if err != nil {
			return id
		}
		names[id] = store.NoticeLabel(ag)
		return names[id]
	}
	// An assignee who is hidden or was kicked still has to appear in the
	// edit menu, or saving the card would silently clear them.
	seen := map[string]bool{}
	for _, c := range choices {
		seen[c.ID] = true
	}
	for _, t := range tasks {
		if t.AssigneeID == "" || seen[t.AssigneeID] {
			continue
		}
		ag, err := p.Store.GetAgent(ctx, t.AssigneeID)
		if err != nil {
			continue
		}
		choices = append(choices, assignChoice{ID: ag.ID, Label: agentChoiceLabel(ag)})
		seen[ag.ID] = true
		names[ag.ID] = store.NoticeLabel(ag)
	}

	byStatus := map[string][]boardCard{}
	for _, t := range tasks {
		notes, err := p.Store.ListTaskComments(ctx, t.ID)
		if err != nil {
			return boardPage{}, err
		}
		cardNotes := make([]boardNote, 0, len(notes))
		for _, n := range notes {
			cardNotes = append(cardNotes, boardNote{Author: label(n.AuthorID), Body: n.Body, At: n.CreatedAt})
		}
		byStatus[t.Status] = append(byStatus[t.Status], boardCard{
			Task:         t,
			Assignee:     label(t.AssigneeID),
			Creator:      label(t.CreatorID),
			SessionTitle: titles[t.SessionID],
			Comments:     cardNotes,
		})
	}
	cols := make([]boardColumn, 0, len(store.TaskStatuses))
	for _, status := range store.TaskStatuses {
		cards := byStatus[status]
		if cards == nil {
			cards = []boardCard{}
		}
		cols = append(cols, boardColumn{Status: status, Label: statusLabel(status), Tasks: cards})
	}
	return boardPage{Agents: choices, Sessions: sessions, Columns: cols}, nil
}

func agentChoiceLabel(a *store.Agent) string {
	label := store.NoticeLabel(a)
	if a.Kind != "" && a.Kind != "human" {
		return label + " · " + a.Kind
	}
	return label
}

func (p *Panel) boardPage(w http.ResponseWriter, r *http.Request) {
	if !p.requirePanel(w, r) {
		return
	}
	page, err := p.loadBoard(r.Context())
	if err != nil {
		p.httpError(w, "store error", http.StatusInternalServerError)
		return
	}
	page.Notice = p.sessions.takeNotice(p.panelToken(r))
	p.render(w, http.StatusOK, "board.tmpl", page)
}

func (p *Panel) boardStream(w http.ResponseWriter, r *http.Request) {
	if !p.requirePanel(w, r) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		p.httpError(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	send := func() {
		page, err := p.loadBoard(ctx)
		if err != nil {
			return
		}
		writeSSE(w, flusher, "board", p.fragment("board-columns", page))
	}
	ticker := time.NewTicker(p.Heartbeat)
	defer ticker.Stop()
	// Same missed-wakeup pattern as the session stream: render, subscribe,
	// render again, then wait. A task written in the gap is not lost.
	send()
	for {
		ch, cancel := p.Hub.SubscribeBoard()
		send()
		select {
		case <-ch:
			cancel()
			if p.Hub.Closed() {
				return
			}
		case <-ticker.C:
			cancel()
			send()
		case <-ctx.Done():
			cancel()
			return
		}
	}
}

func (p *Panel) createTask(w http.ResponseWriter, r *http.Request) {
	if !p.requirePanel(w, r) {
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin rejected", http.StatusForbidden)
		return
	}
	if err := readPanelForm(w, r); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	admin, err := p.adminAgent(r.Context())
	if err != nil {
		p.httpError(w, "store error", http.StatusInternalServerError)
		return
	}
	res, err := p.Board.Create(r.Context(), admin.ID, board.CreateInput{
		Title:       r.PostFormValue("title"),
		Description: r.PostFormValue("description"),
		Status:      r.PostFormValue("status"),
		Priority:    r.PostFormValue("priority"),
		Assignee:    r.PostFormValue("assignee"),
		SessionID:   r.PostFormValue("session"),
	})
	if err != nil {
		p.writeBoardErr(w, err)
		return
	}
	p.sessions.setNotice(p.panelToken(r), "Task “"+res.Task.Title+"” created.")
	http.Redirect(w, r, "/panel/board", http.StatusSeeOther)
}

func (p *Panel) updateTask(w http.ResponseWriter, r *http.Request) {
	if !p.requirePanel(w, r) {
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin rejected", http.StatusForbidden)
		return
	}
	if err := readPanelForm(w, r); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	admin, err := p.adminAgent(r.Context())
	if err != nil {
		p.httpError(w, "store error", http.StatusInternalServerError)
		return
	}
	// A field that the form did not send stays as it is. The compact
	// "Move" control posts only status; the edit form posts everything.
	var in board.UpdateInput
	if r.PostForm.Has("title") {
		v := r.PostFormValue("title")
		in.Title = &v
	}
	if r.PostForm.Has("description") {
		v := r.PostFormValue("description")
		in.Description = &v
	}
	if r.PostForm.Has("status") {
		v := r.PostFormValue("status")
		in.Status = &v
	}
	if r.PostForm.Has("priority") {
		v := r.PostFormValue("priority")
		in.Priority = &v
	}
	if r.PostForm.Has("assignee") {
		v := r.PostFormValue("assignee")
		in.Assignee = &v
	}
	if r.PostForm.Has("session") {
		v := r.PostFormValue("session")
		in.SessionID = &v
	}
	res, err := p.Board.Update(r.Context(), admin.ID, r.PathValue("id"), in)
	if err != nil {
		p.writeBoardErr(w, err)
		return
	}
	p.sessions.setNotice(p.panelToken(r), "Task “"+res.Task.Title+"” updated.")
	http.Redirect(w, r, "/panel/board", http.StatusSeeOther)
}

func (p *Panel) commentTask(w http.ResponseWriter, r *http.Request) {
	if !p.requirePanel(w, r) {
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-origin rejected", http.StatusForbidden)
		return
	}
	if err := readPanelForm(w, r); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	admin, err := p.adminAgent(r.Context())
	if err != nil {
		p.httpError(w, "store error", http.StatusInternalServerError)
		return
	}
	res, err := p.Board.Comment(r.Context(), admin.ID, r.PathValue("id"), r.PostFormValue("body"))
	if err != nil {
		p.writeBoardErr(w, err)
		return
	}
	p.sessions.setNotice(p.panelToken(r), "Comment added to “"+res.Task.Title+"”.")
	http.Redirect(w, r, "/panel/board", http.StatusSeeOther)
}

func (p *Panel) writeBoardErr(w http.ResponseWriter, err error) {
	var be *board.Error
	if errors.As(err, &be) {
		p.httpError(w, be.Msg, be.Code)
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		p.httpError(w, "That task no longer exists.", http.StatusNotFound)
		return
	}
	p.httpError(w, "store error", http.StatusInternalServerError)
}

func readPanelForm(w http.ResponseWriter, r *http.Request) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxPostBytes+4096)
	return parsePanelForm(r)
}
