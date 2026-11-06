package ui

import (
	"context"
	"net/http"
	"time"

	"butler/internal/store"
)

// participantRow is one sidebar entry: the agent plus its derived activity
// and the seq its membership starts at (read-mark audience).
type participantRow struct {
	store.Agent
	Activity  store.ActivityView
	JoinedSeq int64
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

func (p *Panel) decorateParticipants(ctx context.Context, sid string, parts []store.Agent, snap store.ActivitySnapshot) []participantRow {
	since := map[string]int64{}
	if sid != "" {
		if aud, err := p.Store.SessionAudience(ctx, sid); err == nil {
			for _, m := range aud {
				since[m.ID] = m.JoinedSeq
			}
		}
	}
	rows := make([]participantRow, len(parts))
	for i, a := range parts {
		rows[i] = participantRow{Agent: a, Activity: snap.ByAgent[a.ID], JoinedSeq: since[a.ID]}
	}
	return rows
}

// writeLiveMeta sends out-of-band HTML for the read chips and activity chips
// of the messages currently on the session page. HTMX applies hx-swap-oob;
// nothing in the browser computes the counts.
func (p *Panel) writeLiveMeta(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, sid, event string) {
	msgs, err := p.Store.SessionLatestMessages(ctx, sid, 200)
	if err != nil || len(msgs) == 0 {
		return
	}
	vms := p.attachReadState(ctx, p.loadReadState(ctx, sid), msgs)
	name := "live-reads"
	if event == "activity" {
		name = "live-activity"
	}
	writeSSE(w, flusher, event, p.fragment(name, vms))
}
