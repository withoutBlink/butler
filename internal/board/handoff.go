package board

import (
	"context"
	"fmt"
	"strings"

	"butler/internal/store"
)

// SplitChain splits a handoff list typed by a person: commas, "→", or "->".
func SplitChain(raw string) []string {
	raw = strings.ReplaceAll(raw, "→", ",")
	raw = strings.ReplaceAll(raw, "->", ",")
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (s *Service) resolveChain(ctx context.Context, tokens []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, tok := range tokens {
		id, err := s.resolve(ctx, tok)
		if err != nil {
			return nil, err
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// Handoff moves ownership. toToken set: that agent. Empty: the next agent
// in the chain, or the reviewer once the chain is exhausted (status becomes
// review). Any other new owner gets status todo. Only the current owner or
// an admin may call it.
func (s *Service) Handoff(ctx context.Context, actorID, taskID, toToken, note string, admin bool) (Result, error) {
	before, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return Result{}, err
	}
	if !admin && (before.AssigneeID == "" || actorID != before.AssigneeID) {
		return Result{}, bad(403, "only the current owner can hand off this task")
	}
	note, err = cleanNote(note)
	if err != nil {
		return Result{}, err
	}
	dest, review, err := s.handoffDest(ctx, before, toToken)
	if err != nil {
		return Result{}, err
	}
	if dest == before.AssigneeID && !review {
		return Result{}, bad(400, "that agent already owns this task")
	}
	return s.transfer(ctx, actorID, before, dest, review, note, false)
}

// Reject sends a task in review back to an owner. toToken empty means the
// owner it had just before review. Only the reviewer or an admin may call it.
func (s *Service) Reject(ctx context.Context, actorID, taskID, toToken, note string, admin bool) (Result, error) {
	before, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return Result{}, err
	}
	if !admin && actorID != before.Reviewer() {
		return Result{}, bad(403, "only the reviewer can send this task back")
	}
	if before.Status != store.TaskReview {
		return Result{}, bad(409, "this task is not in review")
	}
	note, err = cleanNote(note)
	if err != nil {
		return Result{}, err
	}
	dest := before.ReviewFromID
	if strings.TrimSpace(toToken) != "" {
		dest, err = s.resolve(ctx, toToken)
		if err != nil {
			return Result{}, err
		}
	} else if dest == "" && len(before.Chain) > 0 {
		dest = before.Chain[len(before.Chain)-1]
	}
	if dest == "" || dest == before.Reviewer() {
		return Result{}, bad(400, "say who to send this task back to")
	}
	return s.transfer(ctx, actorID, before, dest, false, note, true)
}

// Approve marks the task done. Only the reviewer or an admin may call it.
func (s *Service) Approve(ctx context.Context, actorID, taskID string, admin bool) (Result, error) {
	before, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return Result{}, err
	}
	if !admin && actorID != before.Reviewer() {
		return Result{}, bad(403, "only the reviewer can mark this task done; hand it off for review")
	}
	if before.Status == store.TaskDone {
		return Result{Task: before}, nil
	}
	status := store.TaskDone
	return s.apply(ctx, actorID, taskID, UpdateInput{Status: &status}, nil, true)
}

func (s *Service) handoffDest(ctx context.Context, task *store.Task, toToken string) (string, bool, error) {
	// In review the next step is the reviewer's: approve, or send it back.
	// A bare handoff would otherwise jump to the start of the chain.
	if task.Status == store.TaskReview && strings.TrimSpace(toToken) == "" {
		return "", false, bad(409, "this task is in review; send it back or name who should own it")
	}
	if strings.TrimSpace(toToken) != "" {
		id, err := s.resolve(ctx, toToken)
		if err != nil {
			return "", false, err
		}
		if id == "" {
			return "", false, bad(400, "say who to hand this task off to")
		}
		return id, id == task.Reviewer(), nil
	}
	cur := task.AssigneeID
	idx := -1
	for i, id := range task.Chain {
		if id == cur {
			idx = i
		}
	}
	if idx >= 0 && idx+1 < len(task.Chain) {
		return task.Chain[idx+1], false, nil
	}
	if idx == -1 && len(task.Chain) > 0 && task.Chain[0] != cur {
		return task.Chain[0], false, nil
	}
	rev := task.Reviewer()
	if rev == "" {
		return "", false, bad(400, "nowhere to hand this task off; set a chain or a reviewer")
	}
	return rev, true, nil
}

func (s *Service) transfer(ctx context.Context, actorID string, before *store.Task, dest string, review bool, note string, rejected bool) (Result, error) {
	status := store.TaskTodo
	patch := store.TaskPatch{AssigneeID: &dest, Status: &status}
	if review {
		status = store.TaskReview
		patch.Status = &status
		from := before.AssigneeID
		patch.ReviewFromID = &from
	}
	after, err := s.Store.UpdateTask(ctx, before.ID, patch)
	if err != nil {
		return Result{}, err
	}
	actor, err := s.Store.GetAgent(ctx, actorID)
	if err != nil {
		actor = &store.Agent{ID: actorID, DisplayName: actorID}
	}
	next, err := s.Store.GetAgent(ctx, dest)
	if err != nil {
		next = &store.Agent{ID: dest, DisplayName: dest}
	}
	var body string
	if rejected {
		body = fmt.Sprintf("%s sent this back to %s", store.NoticeLabel(actor), store.NoticeLabel(next))
	} else {
		body = fmt.Sprintf("%s handed off to %s", store.NoticeLabel(actor), store.NoticeLabel(next))
	}
	if note != "" {
		body += ": " + note
	}
	c, err := s.Store.AddTaskComment(ctx, &store.TaskComment{TaskID: before.ID, AuthorID: actorID, Body: body})
	if err != nil {
		return Result{}, err
	}
	s.touch(ctx, actorID)
	seq := s.announce(ctx, actorID, before, after, c, &xfer{rejected: rejected})
	return Result{Task: after, Comment: c, NotifySeq: seq}, nil
}

func cleanNote(note string) (string, error) {
	note = strings.TrimSpace(note)
	if len(note) > maxNoteLen {
		return "", bad(400, "note exceeds 8KB")
	}
	return note, nil
}
