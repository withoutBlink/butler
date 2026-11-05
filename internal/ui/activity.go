package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"butler/internal/store"
)

// participantRow is one sidebar entry: the agent plus its derived activity,
// so the participants fragment can show the indicator without a second lookup.
type participantRow struct {
	store.Agent
	Activity store.ActivityView
}

// dashPerson is a dashboard session participant with the same indicator.
type dashPerson struct {
	store.Agent
	Activity store.ActivityView
}

func (p *Panel) loadActivity(ctx context.Context) store.ActivitySnapshot {
	empty := store.ActivitySnapshot{ByAgent: map[string]store.ActivityView{}, Stalled: map[string]bool{}}
	agents, err := p.Store.AllAgents(ctx)
	if err != nil {
		p.Log.Error("activity agents", "err", err)
		return empty
	}
	tasks, err := p.Store.ListTasks(ctx, store.TaskFilter{})
	if err != nil {
		p.Log.Error("activity tasks", "err", err)
		return empty
	}
	waiting := map[string]bool{}
	if p.Hub != nil {
		waiting = p.Hub.WaitingAgents()
	}
	return store.BuildActivity(agents, tasks, store.ActivityInput{
		Now:           time.Now(),
		Waiting:       waiting,
		AlwaysPresent: map[string]bool{AdminAgentID: true},
	})
}

func withActivity(agents []store.Agent, snap store.ActivitySnapshot) []participantRow {
	rows := make([]participantRow, len(agents))
	for i, a := range agents {
		rows[i] = participantRow{Agent: a, Activity: snap.ByAgent[a.ID]}
	}
	return rows
}

func (p *Panel) writeActivity(ctx context.Context, w http.ResponseWriter, flusher http.Flusher) {
	snap := p.loadActivity(ctx)
	data, err := json.Marshal(snap.ByAgent)
	if err != nil {
		return
	}
	writeSSE(w, flusher, "activity", string(data))
}
