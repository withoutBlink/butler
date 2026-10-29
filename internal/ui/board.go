package ui

import (
	"context"
	"errors"
	"hash/fnv"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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

type chainStep struct {
	Name    string
	Initial string
	Current bool
	Review  bool
}

type boardCard struct {
	Task         store.Task
	Assignee     string
	Creator      string
	Owner        string
	OwnerInitial string
	OwnerClass   string
	Chain        []chainStep
	ChainText    string
	SessionTitle string
	Comments     []boardNote
	ShowHandoff  bool
	ShowApprove  bool
	ShowReject   bool
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
		owner := label(t.AssigneeID)
		var namesInChain []string
		var steps []chainStep
		for _, id := range t.Chain {
			name := label(id)
			namesInChain = append(namesInChain, name)
			steps = append(steps, chainStep{
				Name:    name,
				Initial: ownerInitial(name),
				Current: t.Status != store.TaskReview && id == t.AssigneeID,
			})
		}
		revName := label(t.Reviewer())
		if revName == "" {
			revName = "creator"
		}
		steps = append(steps, chainStep{
			Name:    revName,
			Initial: ownerInitial(revName),
			Review:  true,
			Current: t.Status == store.TaskReview,
		})
		byStatus[t.Status] = append(byStatus[t.Status], boardCard{
			Task:         t,
			Assignee:     owner,
			Creator:      label(t.CreatorID),
			Owner:        owner,
			OwnerInitial: ownerInitial(owner),
			OwnerClass:   ownerClass(t.AssigneeID),
			Chain:        steps,
			ChainText:    strings.Join(namesInChain, ", "),
			SessionTitle: titles[t.SessionID],
			Comments:     cardNotes,
			ShowHandoff:  t.Status != store.TaskDone && t.Status != store.TaskReview,
			// Approve and Send back are the review step. Offering Approve
			// earlier put it next to Hand off, and in a narrow column the
			// Hand off button was the one that got clipped.
			ShowApprove: t.Status == store.TaskReview,
			ShowReject:  t.Status == store.TaskReview,
		})
	}
	cols := make([]boardColumn, 0, len(store.TaskStatuses))
	for _, status := range store.TaskStatuses {
		cards := byStatus[status]
		if cards == nil {
			cards = []boardCard{}
		}
		// Done is a log of finished work: newest first, not priority order.
		if status == store.TaskDone {
			sort.Slice(cards, func(i, j int) bool {
				return cards[i].Task.UpdatedAt > cards[j].Task.UpdatedAt
			})
		}
		cols = append(cols, boardColumn{Status: status, Label: statusLabel(status), Tasks: cards})
	}
	return boardPage{Agents: choices, Sessions: sessions, Columns: cols}, nil
}

func ownerInitial(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "?"
	}
	r, _ := utf8.DecodeRuneInString(name)
	return string(unicode.ToUpper(r))
}

func ownerClass(id string) string {
	classes := []string{"c0", "c1", "c2", "c3", "c4", "c5", "c6", "c7"}
	if id == "" {
		return classes[0]
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return classes[h.Sum32()%uint32(len(classes))]
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
		Chain:       board.SplitChain(r.PostFormValue("chain")),
		Reviewer:    r.PostFormValue("reviewer"),
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
	if r.PostForm.Has("chain") {
		v := board.SplitChain(r.PostFormValue("chain"))
		in.Chain = &v
	}
	if r.PostForm.Has("reviewer") {
		v := r.PostFormValue("reviewer")
		in.Reviewer = &v
	}
	res, err := p.Board.Update(r.Context(), admin.ID, r.PathValue("id"), in, true)
	if err != nil {
		p.writeBoardErr(w, err)
		return
	}
	p.sessions.setNotice(p.panelToken(r), "Task “"+res.Task.Title+"” updated.")
	http.Redirect(w, r, "/panel/board", http.StatusSeeOther)
}

func (p *Panel) handoffTask(w http.ResponseWriter, r *http.Request) {
	p.ownerAction(w, r, "handed off", func(ctx context.Context, actor, id string) (board.Result, error) {
		return p.Board.Handoff(ctx, actor, id, r.PostFormValue("to"), r.PostFormValue("note"), true)
	})
}

func (p *Panel) approveTask(w http.ResponseWriter, r *http.Request) {
	p.ownerAction(w, r, "approved", func(ctx context.Context, actor, id string) (board.Result, error) {
		return p.Board.Approve(ctx, actor, id, true)
	})
}

func (p *Panel) rejectTask(w http.ResponseWriter, r *http.Request) {
	p.ownerAction(w, r, "sent back", func(ctx context.Context, actor, id string) (board.Result, error) {
		return p.Board.Reject(ctx, actor, id, r.PostFormValue("to"), r.PostFormValue("note"), true)
	})
}

func (p *Panel) ownerAction(w http.ResponseWriter, r *http.Request, verb string, fn func(context.Context, string, string) (board.Result, error)) {
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
	res, err := fn(r.Context(), admin.ID, r.PathValue("id"))
	if err != nil {
		p.writeBoardErr(w, err)
		return
	}
	p.sessions.setNotice(p.panelToken(r), "Task “"+res.Task.Title+"” "+verb+".")
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
