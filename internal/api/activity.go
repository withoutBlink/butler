package api

import (
	"context"
	"net/http"
	"time"

	"butler/internal/store"
)

// activities loads every agent and every task and derives who is working,
// idle, blocked, stalled, or offline, plus which in_progress tasks have gone
// quiet. A store error degrades to an empty snapshot so a list still returns;
// the failure is logged.
func (a *API) activities(ctx context.Context) store.ActivitySnapshot {
	empty := store.ActivitySnapshot{ByAgent: map[string]store.ActivityView{}, Stalled: map[string]bool{}}
	agents, err := a.Store.AllAgents(ctx)
	if err != nil {
		a.Log.Error("activity agents", "err", err)
		return empty
	}
	tasks, err := a.Store.ListTasks(ctx, store.TaskFilter{})
	if err != nil {
		a.Log.Error("activity tasks", "err", err)
		return empty
	}
	waiting := map[string]bool{}
	if a.Hub != nil {
		waiting = a.Hub.WaitingAgents()
	}
	return store.BuildActivity(agents, tasks, store.ActivityInput{
		Now:           time.Now(),
		Waiting:       waiting,
		AlwaysPresent: map[string]bool{adminAgentID: true},
	})
}

// taskJSON is a task as the API returns it: the stored row, whether that
// claimed in_progress task has gone quiet, and the owner's derived activity.
type taskJSON struct {
	store.Task
	NotifySeq        int64  `json:"notify_seq,omitempty"`
	Stalled          bool   `json:"stalled"`
	AssigneeActivity string `json:"assignee_activity,omitempty"`
}

func (a *API) decorateTask(ctx context.Context, t store.Task, seq int64) taskJSON {
	return a.decorateTasks(ctx, []store.Task{t}, []int64{seq})[0]
}

func (a *API) decorateTasks(ctx context.Context, tasks []store.Task, seqs []int64) []taskJSON {
	snap := a.activities(ctx)
	out := make([]taskJSON, len(tasks))
	for i, t := range tasks {
		view := taskJSON{Task: t, Stalled: snap.Stalled[t.ID]}
		if i < len(seqs) {
			view.NotifySeq = seqs[i]
		}
		if t.AssigneeID != "" {
			view.AssigneeActivity = snap.ByAgent[t.AssigneeID].State
		}
		out[i] = view
	}
	return out
}

// setActivity records the caller's own working / idle / blocked report.
// It does not post a message, comment on a task, or wake any agent — a
// self-report is not an assignment. The panel's live channels are nudged so
// the indicator moves without waiting for the next heartbeat.
func (a *API) setActivity(w http.ResponseWriter, r *http.Request) {
	agent := a.requireAgent(w, r)
	if agent == nil {
		return
	}
	var req struct {
		State string `json:"state"`
		Note  string `json:"note"`
	}
	if !decodeJSON(w, r, &req, 8<<10) {
		return
	}
	state, note, err := store.NormalizeActivity(req.State, req.Note)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.Store.SetActivity(r.Context(), agent.ID, state, note); err != nil {
		a.writeStoreErr(w, err)
		return
	}
	a.publishActivity(r.Context(), agent.ID)
	snap := a.activities(r.Context())
	view := snap.ByAgent[agent.ID]
	if view.State == "" {
		view = store.ActivityView{State: state, Note: note}
	}
	writeJSON(w, http.StatusOK, map[string]any{"activity": view})
}

// publishActivity refreshes open panels. It does not Hub.Notify: waking
// long-poll waiters would deliver an empty "something happened" to agents
// who were not assigned anything.
func (a *API) publishActivity(ctx context.Context, agentID string) {
	if a.Hub == nil {
		return
	}
	a.Hub.NotifyBoard()
	sids, err := a.Store.SessionsForAgent(ctx, agentID)
	if err != nil {
		a.Log.Error("activity sessions", "err", err, "agent", agentID)
		return
	}
	for _, sid := range sids {
		a.Hub.NotifySession(sid)
	}
}
