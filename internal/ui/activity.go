package ui

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
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
// Tip is the hover text: the full display name, plus the id when it differs.
type dashPerson struct {
	store.Agent
	Activity store.ActivityView
	Tip      string
}

// dashParticipantLimit is how many names the sessions table shows before
// the rest sit under “+N more”. A meeting of a few people stays fully
// visible; Global, with every teammate, does not become a wall of titles.
const dashParticipantLimit = 4

// condenseParticipants orders people live and recently seen first, then
// keeps the first dashParticipantLimit visible. The rest are still in the
// page, inside a collapsed disclosure.
func condenseParticipants(people []dashPerson) (shown, more []dashPerson, moreLabel string) {
	if len(people) == 0 {
		return nil, nil, ""
	}
	ordered := append([]dashPerson(nil), people...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return personBefore(ordered[i], ordered[j])
	})
	for i := range ordered {
		ordered[i].Tip = personTip(ordered[i])
	}
	if len(ordered) <= dashParticipantLimit {
		return ordered, nil, ""
	}
	n := len(ordered) - dashParticipantLimit
	return ordered[:dashParticipantLimit], ordered[dashParticipantLimit:], moreParticipantsLabel(n)
}

func personBefore(a, b dashPerson) bool {
	la, lb := liveActivity(a.Activity.State), liveActivity(b.Activity.State)
	if la != lb {
		return la
	}
	ra, rb := agentRecency(a.Agent), agentRecency(b.Agent)
	if !ra.Equal(rb) {
		return ra.After(rb)
	}
	na, nb := strings.ToLower(a.DisplayName), strings.ToLower(b.DisplayName)
	if na != nb {
		return na < nb
	}
	return a.ID < b.ID
}

func personTip(p dashPerson) string {
	name := p.DisplayName
	if name == "" {
		return p.ID
	}
	if name == p.ID {
		return p.ID
	}
	return name + " (" + p.ID + ")"
}

func moreParticipantsLabel(n int) string {
	if n == 1 {
		return "+1 more"
	}
	return fmt.Sprintf("+%d more", n)
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

// writeLiveMeta sends out-of-band HTML for the read chips of the messages
// currently on the session page. HTMX applies hx-swap-oob. Activity stays on
// the member list, which the presence event re-renders, not on each line.
func (p *Panel) writeLiveMeta(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, sid string) {
	msgs, err := p.Store.SessionLatestMessages(ctx, sid, 200)
	if err != nil || len(msgs) == 0 {
		return
	}
	vms := p.attachReadState(ctx, p.loadReadState(ctx, sid), msgs)
	writeSSE(w, flusher, "reads", p.fragment("live-reads", vms))
}
