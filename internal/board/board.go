package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"butler/internal/hub"
	"butler/internal/store"
)

const (
	maxTitleLen = 200
	maxDescLen  = 16 << 10
	maxNoteLen  = 8 << 10
	maxURLLen   = 2048
)

// taskIDRe is the shape of a client-supplied task id: the server's own
// UUIDs match, and so does a short slug a caller retries with.
var taskIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// Error is a caller-facing failure with an HTTP status. Store errors pass
// through unchanged so the API can map not-found and conflict itself.
type Error struct {
	Code int
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

func bad(code int, msg string) error { return &Error{Code: code, Msg: msg} }

// Service writes tasks and then tells the room. Assignment, status changes
// and comments post a chat message that @mentions the people who should
// act, into the task's session (or the global session), so the existing
// wait / relay paths deliver it. Webhooks for remote assistants fire in
// the background.
type Service struct {
	Store *store.Store
	Hub   *hub.Hub
	Log   *slog.Logger
	Hooks *Hooks
}

func New(st *store.Store, h *hub.Hub, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Service{Store: st, Hub: h, Log: log, Hooks: newHooks(log)}
}

// Result is one successful task write.
type Result struct {
	Task      *store.Task
	Comment   *store.TaskComment
	NotifySeq int64
	// Created is false when Create found the client-supplied id already
	// stored and left it alone (same idempotency as message posts).
	Created bool
}

// CreateInput is a new task. Assignee is a mention-style token (id,
// display name, or kind), not a resolved id. Empty leaves it unassigned.
type CreateInput struct {
	ID          string
	Title       string
	Description string
	Status      string
	Priority    string
	Assignee    string
	SessionID   string
}

// UpdateInput is a partial edit. Nil leaves the field unchanged. Assignee
// is a mention-style token; a pointer to "" clears the assignee.
type UpdateInput struct {
	Title       *string
	Description *string
	Status      *string
	Priority    *string
	Assignee    *string
	SessionID   *string
}

func (s *Service) Create(ctx context.Context, actorID string, in CreateInput) (Result, error) {
	if in.ID != "" {
		if !taskIDRe.MatchString(in.ID) {
			return Result{}, bad(400, "invalid task id")
		}
		existing, err := s.Store.GetTask(ctx, in.ID)
		if err == nil {
			return Result{Task: existing, Created: false}, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return Result{}, err
		}
	}
	title, err := cleanTitle(in.Title)
	if err != nil {
		return Result{}, err
	}
	desc, err := cleanDesc(in.Description)
	if err != nil {
		return Result{}, err
	}
	status, err := cleanStatus(in.Status, store.TaskTodo)
	if err != nil {
		return Result{}, err
	}
	priority, err := cleanPriority(in.Priority, store.PriorityNormal)
	if err != nil {
		return Result{}, err
	}
	if err := s.checkSession(ctx, in.SessionID); err != nil {
		return Result{}, err
	}
	assignee, err := s.resolve(ctx, in.Assignee)
	if err != nil {
		return Result{}, err
	}
	task, err := s.Store.CreateTask(ctx, &store.Task{
		ID:          in.ID,
		Title:       title,
		Description: desc,
		Status:      status,
		Priority:    priority,
		AssigneeID:  assignee,
		CreatorID:   actorID,
		SessionID:   strings.TrimSpace(in.SessionID),
	})
	if err != nil {
		return Result{}, err
	}
	seq := s.announce(ctx, actorID, nil, task, nil)
	return Result{Task: task, NotifySeq: seq, Created: true}, nil
}

func (s *Service) Update(ctx context.Context, actorID, taskID string, in UpdateInput) (Result, error) {
	var assignee *string
	if in.Assignee != nil {
		id, err := s.resolve(ctx, *in.Assignee)
		if err != nil {
			return Result{}, err
		}
		assignee = &id
	}
	return s.apply(ctx, actorID, taskID, in, assignee)
}

// Claim assigns the task to actorID and moves it to in_progress. Repeating
// a claim that is already there is a no-op (no second notice).
func (s *Service) Claim(ctx context.Context, actorID, taskID string) (Result, error) {
	cur, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return Result{}, err
	}
	if cur.AssigneeID == actorID && cur.Status == store.TaskInProgress {
		return Result{Task: cur}, nil
	}
	status := store.TaskInProgress
	return s.apply(ctx, actorID, taskID, UpdateInput{Status: &status}, &actorID)
}

func (s *Service) Comment(ctx context.Context, actorID, taskID, body string) (Result, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return Result{}, bad(400, "comment body is required")
	}
	if len(body) > maxNoteLen {
		return Result{}, bad(400, "comment exceeds 8KB")
	}
	before, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return Result{}, err
	}
	c, err := s.Store.AddTaskComment(ctx, &store.TaskComment{TaskID: taskID, AuthorID: actorID, Body: body})
	if err != nil {
		return Result{}, err
	}
	after, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return Result{}, err
	}
	seq := s.announce(ctx, actorID, before, after, c)
	return Result{Task: after, Comment: c, NotifySeq: seq}, nil
}

func (s *Service) apply(ctx context.Context, actorID, taskID string, in UpdateInput, assignee *string) (Result, error) {
	before, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return Result{}, err
	}
	if in.Title == nil && in.Description == nil && in.Status == nil && in.Priority == nil && in.SessionID == nil && assignee == nil {
		return Result{Task: before}, nil
	}
	patch := store.TaskPatch{AssigneeID: assignee}
	if in.Title != nil {
		title, err := cleanTitle(*in.Title)
		if err != nil {
			return Result{}, err
		}
		patch.Title = &title
	}
	if in.Description != nil {
		desc, err := cleanDesc(*in.Description)
		if err != nil {
			return Result{}, err
		}
		patch.Description = &desc
	}
	if in.Status != nil {
		status, err := cleanStatus(*in.Status, "")
		if err != nil {
			return Result{}, err
		}
		if status == "" {
			return Result{}, bad(400, "invalid status")
		}
		patch.Status = &status
	}
	if in.Priority != nil {
		priority, err := cleanPriority(*in.Priority, "")
		if err != nil {
			return Result{}, err
		}
		if priority == "" {
			return Result{}, bad(400, "invalid priority")
		}
		patch.Priority = &priority
	}
	if in.SessionID != nil {
		sid := strings.TrimSpace(*in.SessionID)
		if err := s.checkSession(ctx, sid); err != nil {
			return Result{}, err
		}
		patch.SessionID = &sid
	}
	after, err := s.Store.UpdateTask(ctx, taskID, patch)
	if err != nil {
		return Result{}, err
	}
	if sameTask(before, after) {
		s.wakeBoard()
		return Result{Task: after}, nil
	}
	seq := s.announce(ctx, actorID, before, after, nil)
	return Result{Task: after, NotifySeq: seq}, nil
}

// announce posts the mention, wakes waiters and the board, and queues
// webhooks. Failures here are logged: the task write already committed,
// and a missed notice must not turn the request into a 500.
func (s *Service) announce(ctx context.Context, actorID string, before, after *store.Task, comment *store.TaskComment) int64 {
	seq := s.postNotice(ctx, actorID, before, after, comment)
	s.wakeBoard()
	s.deliverHooks(ctx, actorID, before, after, comment)
	return seq
}

func (s *Service) wakeBoard() {
	if s.Hub != nil {
		s.Hub.NotifyBoard()
	}
}

func (s *Service) postNotice(ctx context.Context, actorID string, before, after *store.Task, comment *store.TaskComment) int64 {
	targets := mentionTargets(actorID, before, after, comment)
	if len(targets) == 0 {
		return 0
	}
	sid := s.noticeSession(ctx, after)
	if sid == "" {
		s.Log.Warn("task notice skipped, no active session", "task", after.ID)
		return 0
	}
	for _, id := range targets {
		if _, err := s.Store.JoinSession(ctx, sid, id); err != nil {
			s.Log.Error("join for task notice", "err", err, "session", sid, "agent", id)
		}
	}
	actor, err := s.Store.GetAgent(ctx, actorID)
	if err != nil {
		s.Log.Error("load notice actor", "err", err, "agent", actorID)
		actor = &store.Agent{ID: actorID, DisplayName: actorID}
	}
	msg, err := s.Store.InsertMessage(ctx, &store.Message{
		SessionID: sid,
		SenderID:  actorID,
		Kind:      "chat",
		Content:   noticeText(actor, before, after, comment, targets),
	})
	if err != nil {
		s.Log.Error("task notice", "err", err, "task", after.ID)
		return 0
	}
	if s.Hub != nil {
		s.Hub.NotifySession(sid)
		if parts, err := s.Store.Participants(ctx, sid); err == nil {
			ids := make([]string, 0, len(parts))
			for _, p := range parts {
				ids = append(ids, p.ID)
			}
			s.Hub.Notify(ids)
		} else {
			s.Log.Error("list participants", "err", err, "session", sid)
		}
	}
	return msg.Seq
}

// noticeSession is where the @mention is posted: the task's linked session
// when that room is active, otherwise the global session. An archived
// link falls back to global so the assignee still wakes up.
func (s *Service) noticeSession(ctx context.Context, task *store.Task) string {
	if task.SessionID != "" {
		if sess, err := s.Store.GetSession(ctx, task.SessionID); err == nil && sess.Status == "active" {
			return sess.ID
		}
	}
	if sess, err := s.Store.GetSession(ctx, store.GlobalSessionID); err == nil && sess.Status == "active" {
		return store.GlobalSessionID
	}
	return ""
}

// mentionTargets are the agents the notice @mentions: the creator, the
// current assignee, and the previous assignee when the assignment moved.
// The actor is left out — a mention means "please reply", and the author
// of the change already knows.
func mentionTargets(actorID string, before, after *store.Task, comment *store.TaskComment) []string {
	var ids []string
	add := func(id string) {
		if id == "" || id == actorID {
			return
		}
		for _, e := range ids {
			if e == id {
				return
			}
		}
		ids = append(ids, id)
	}
	// A brand-new unassigned task has nobody to wake. Everything else
	// (assignment, status, comment, edit) tells the other party.
	if comment == nil && before == nil && after.AssigneeID == "" {
		return nil
	}
	add(after.CreatorID)
	add(after.AssigneeID)
	if before != nil {
		add(before.AssigneeID)
	}
	return ids
}

func noticeText(actor *store.Agent, before, after *store.Task, comment *store.TaskComment, mentions []string) string {
	var b strings.Builder
	for i, id := range mentions {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('@')
		b.WriteString(id)
	}
	if len(mentions) > 0 {
		b.WriteByte(' ')
	}
	switch {
	case comment != nil:
		fmt.Fprintf(&b, "comment on task %q (%s)", after.Title, after.ID)
	case (before == nil || before.AssigneeID != after.AssigneeID) && after.AssigneeID != "":
		fmt.Fprintf(&b, "assigned task %q (%s)", after.Title, after.ID)
	case before != nil && before.AssigneeID != after.AssigneeID:
		fmt.Fprintf(&b, "unassigned task %q (%s)", after.Title, after.ID)
	case before != nil && before.Status != after.Status:
		fmt.Fprintf(&b, "task %q (%s) status %s → %s", after.Title, after.ID, before.Status, after.Status)
	default:
		fmt.Fprintf(&b, "updated task %q (%s)", after.Title, after.ID)
	}
	fmt.Fprintf(&b, "\nstatus: %s · priority: %s", after.Status, after.Priority)
	if after.SessionID != "" {
		fmt.Fprintf(&b, " · session: %s", after.SessionID)
	}
	if comment != nil {
		fmt.Fprintf(&b, "\n%s", clip(comment.Body, 500))
	} else if after.Description != "" && (before == nil || before.AssigneeID != after.AssigneeID) {
		fmt.Fprintf(&b, "\n%s", clip(after.Description, 280))
	}
	fmt.Fprintf(&b, "\n— %s", store.NoticeLabel(actor))
	return b.String()
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

type hookEvent struct {
	agentID string
	event   string
}

type hookChange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type hookActor struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Kind        string `json:"kind"`
}

type hookPayload struct {
	Event   string                `json:"event"`
	At      int64                 `json:"at"`
	Actor   hookActor             `json:"actor"`
	Task    store.Task            `json:"task"`
	Comment *store.TaskComment    `json:"comment,omitempty"`
	Changes map[string]hookChange `json:"changes,omitempty"`
}

// deliverHooks queues one POST per agent who should wake: the new assignee
// on assignment, the previous assignee when they lose it, and the current
// assignee plus the creator when status or a comment changes. The actor is
// skipped so a remote assistant does not get a webhook for its own write.
func (s *Service) deliverHooks(ctx context.Context, actorID string, before, after *store.Task, comment *store.TaskComment) {
	if s.Hooks == nil {
		return
	}
	targets := hookTargets(actorID, before, after, comment)
	if len(targets) == 0 {
		return
	}
	actor, err := s.Store.GetAgent(ctx, actorID)
	if err != nil {
		actor = &store.Agent{ID: actorID, DisplayName: actorID}
	}
	changes := hookChanges(before, after)
	for _, t := range targets {
		rawURL, err := s.Store.WebhookURL(ctx, t.agentID)
		if err != nil {
			s.Log.Error("lookup webhook", "err", err, "agent", t.agentID)
			continue
		}
		if rawURL == "" {
			continue
		}
		payload := hookPayload{
			Event:   t.event,
			At:      time.Now().UnixMilli(),
			Actor:   hookActor{ID: actor.ID, DisplayName: store.NoticeLabel(actor), Kind: actor.Kind},
			Task:    *after,
			Changes: changes,
		}
		if comment != nil {
			payload.Comment = comment
		}
		body, err := json.Marshal(payload)
		if err != nil {
			s.Log.Error("encode webhook", "err", err)
			continue
		}
		s.Hooks.Post(rawURL, body)
	}
}

func hookTargets(actorID string, before, after *store.Task, comment *store.TaskComment) []hookEvent {
	var out []hookEvent
	seen := map[string]bool{}
	add := func(id, event string) {
		if id == "" || id == actorID || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, hookEvent{id, event})
	}
	if comment != nil {
		add(after.AssigneeID, "task.comment")
		add(after.CreatorID, "task.comment")
		return out
	}
	assigneeChanged := before == nil || before.AssigneeID != after.AssigneeID
	if assigneeChanged && after.AssigneeID != "" {
		add(after.AssigneeID, "task.assigned")
	}
	if before != nil && before.AssigneeID != "" && before.AssigneeID != after.AssigneeID {
		add(before.AssigneeID, "task.unassigned")
	}
	if before != nil && before.Status != after.Status {
		add(after.AssigneeID, "task.status")
		add(after.CreatorID, "task.status")
		return out
	}
	if before != nil && !assigneeChanged && !sameTask(before, after) {
		add(after.AssigneeID, "task.updated")
		add(after.CreatorID, "task.updated")
	}
	return out
}

func hookChanges(before, after *store.Task) map[string]hookChange {
	if before == nil {
		return nil
	}
	out := map[string]hookChange{}
	put := func(name, from, to string) {
		if from != to {
			out[name] = hookChange{From: from, To: to}
		}
	}
	put("title", before.Title, after.Title)
	put("status", before.Status, after.Status)
	put("priority", before.Priority, after.Priority)
	put("assignee_id", before.AssigneeID, after.AssigneeID)
	put("session_id", before.SessionID, after.SessionID)
	if len(out) == 0 {
		return nil
	}
	return out
}

func sameTask(a, b *store.Task) bool {
	return a.Title == b.Title && a.Description == b.Description && a.Status == b.Status &&
		a.Priority == b.Priority && a.AssigneeID == b.AssigneeID && a.SessionID == b.SessionID
}

// resolve maps a mention token to an agent id. An empty token is "no
// assignee", not an error.
func (s *Service) resolve(ctx context.Context, token string) (string, error) {
	if strings.TrimSpace(token) == "" {
		return "", nil
	}
	ag, err := s.Store.ResolveAssignee(ctx, token)
	if err == nil {
		return ag.ID, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		shown := strings.TrimPrefix(strings.TrimSpace(token), "@")
		return "", bad(400, fmt.Sprintf("no agent matches assignee %q", shown))
	}
	if errors.Is(err, store.ErrConflict) {
		msg := strings.TrimPrefix(err.Error(), store.ErrConflict.Error()+": ")
		return "", bad(400, msg)
	}
	return "", err
}

func (s *Service) checkSession(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	if _, err := s.Store.GetSession(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return bad(400, "session not found")
		}
		return err
	}
	return nil
}

func cleanTitle(title string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", bad(400, "title is required")
	}
	if len(title) > maxTitleLen {
		return "", bad(400, "title exceeds 200 characters")
	}
	return title, nil
}

func cleanDesc(desc string) (string, error) {
	if len(desc) > maxDescLen {
		return "", bad(400, "description exceeds 16KB")
	}
	return desc, nil
}

func cleanStatus(status, def string) (string, error) {
	status = strings.TrimSpace(status)
	if status == "" {
		return def, nil
	}
	if !store.ValidTaskStatus(status) {
		return "", bad(400, "status must be one of todo, in_progress, blocked, review, done")
	}
	return status, nil
}

func cleanPriority(priority, def string) (string, error) {
	priority = strings.TrimSpace(priority)
	if priority == "" {
		return def, nil
	}
	if !store.ValidTaskPriority(priority) {
		return "", bad(400, "priority must be one of low, normal, high, urgent")
	}
	return priority, nil
}

// ValidWebhookURL reports whether raw is an http(s) URL with a host.
func ValidWebhookURL(raw string) error {
	if len(raw) > maxURLLen {
		return bad(400, "webhook url is too long")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return bad(400, "webhook url must be http or https")
	}
	return nil
}
