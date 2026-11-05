package store

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Derived activity for one agent identity (an app-level client or one of its
// per-session roles). Butler does not spawn agents; this is computed from
// signals it already has plus an optional self-report.
const (
	ActivityWorking = "working"
	ActivityIdle    = "idle"
	ActivityBlocked = "blocked" // waiting on a review, a human, or a blocked task
	ActivityStalled = "stalled" // claimed in_progress work and the owner went quiet
	ActivityOffline = "offline"
)

const (
	// ActivityReportTTL is how long an explicit set_activity report stays
	// current. After that, inferred signals take over. The agent is expected
	// to refresh a deliberate "working" or "blocked".
	ActivityReportTTL = 2 * time.Minute
	// ActivityWorkingFor is how long a substantive action (post, task write,
	// context write) counts as working on its own.
	ActivityWorkingFor = 45 * time.Second
	// ActivityStallAfter is how long an in_progress task's owner can go
	// without a call or a held long-poll before the task is stalled.
	ActivityStallAfter = 5 * time.Minute
	// ActivityPresenceFor matches the panel's online window: a call or a
	// held long-poll inside it means the agent is still around.
	ActivityPresenceFor = 90 * time.Second
	// ActivityNoteMax is the cap on a self-reported note.
	ActivityNoteMax = 200
)

// ActivityView is the derived state exposed by the API and the panel.
// Since is the unix-second timestamp of the signal the state is anchored to.
type ActivityView struct {
	State string `json:"state"`
	Note  string `json:"note,omitempty"`
	Since int64  `json:"since,omitempty"`
}

// Title is the hover text: the state, plus the note when there is one.
func (v ActivityView) Title() string {
	if v.Note == "" {
		return v.State
	}
	return v.State + " — " + v.Note
}

// ActivityInput is the clock and the live signals BuildActivity cannot see
// in the database. Waiting is the set of agent ids currently blocked in
// GET /api/v1/wait. AlwaysPresent is the set that counts as here even with
// no API calls (the panel human).
type ActivityInput struct {
	Now           time.Time
	Waiting       map[string]bool
	AlwaysPresent map[string]bool
}

// ActivitySnapshot is every agent's derived activity plus the ids of
// in_progress tasks whose owner has gone quiet.
type ActivitySnapshot struct {
	ByAgent map[string]ActivityView
	Stalled map[string]bool
}

// BuildActivity derives activity for each agent and which claimed
// in_progress tasks are stalled. An agent missing from agents (a deleted
// row) still stalls an in_progress task assigned to them.
func BuildActivity(agents []Agent, tasks []Task, in ActivityInput) ActivitySnapshot {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	byID := make(map[string]Agent, len(agents))
	for _, a := range agents {
		byID[a.ID] = a
	}
	owned := map[string][]Task{}
	for _, t := range tasks {
		if t.AssigneeID == "" {
			continue
		}
		owned[t.AssigneeID] = append(owned[t.AssigneeID], t)
	}
	snap := ActivitySnapshot{
		ByAgent: make(map[string]ActivityView, len(agents)),
		Stalled: map[string]bool{},
	}
	for _, t := range tasks {
		if t.Status != TaskInProgress || t.AssigneeID == "" {
			continue
		}
		ag, ok := byID[t.AssigneeID]
		if !ok || quiet(ag, in.Waiting[t.AssigneeID], in.AlwaysPresent[t.AssigneeID], now) {
			snap.Stalled[t.ID] = true
		}
	}
	for _, a := range agents {
		snap.ByAgent[a.ID] = deriveActivity(a, owned[a.ID], in.Waiting[a.ID], in.AlwaysPresent[a.ID], now)
	}
	return snap
}

func deriveActivity(a Agent, owned []Task, waiting, always bool, now time.Time) ActivityView {
	inProg, blocked, review := taskHints(owned)
	last := latestUnix(a.LastSeenAt, a.LastActionAt)
	online := always || waiting || within(last, now, ActivityPresenceFor)
	if inProg && quiet(a, waiting, always, now) {
		return ActivityView{State: ActivityStalled, Since: unixOr(last, now)}
	}
	// A self-report only applies while the agent is still around. A "working"
	// from several minutes ago must not mask a disconnect.
	reportOK := online && knownExplicit(a.ActivityState) && within(a.ActivityAt, now, ActivityReportTTL)
	actionOK := within(a.LastActionAt, now, ActivityWorkingFor)
	if reportOK && !newer(a.LastActionAt, a.ActivityAt) {
		return ActivityView{State: a.ActivityState, Note: a.ActivityNote, Since: unixSeconds(a.ActivityAt)}
	}
	if actionOK {
		return ActivityView{State: ActivityWorking, Since: unixSeconds(a.LastActionAt)}
	}
	if reportOK {
		return ActivityView{State: a.ActivityState, Note: a.ActivityNote, Since: unixSeconds(a.ActivityAt)}
	}
	// The panel human is always present, so "owns in_progress" would pin them
	// to working forever. Their state follows actions only.
	if !always && online && inProg {
		return ActivityView{State: ActivityWorking, Since: unixOr(last, now)}
	}
	if !always && online && (blocked || review) {
		note := "waiting on review"
		if blocked {
			note = "task blocked"
		}
		return ActivityView{State: ActivityBlocked, Note: note, Since: unixOr(last, now)}
	}
	if online {
		return ActivityView{State: ActivityIdle, Since: unixOr(last, now)}
	}
	return ActivityView{State: ActivityOffline, Since: unixOr(last, now)}
}

func taskHints(owned []Task) (inProg, blocked, review bool) {
	for _, t := range owned {
		switch t.Status {
		case TaskInProgress:
			inProg = true
		case TaskBlocked:
			blocked = true
		case TaskReview:
			review = true
		}
	}
	return inProg, blocked, review
}

// quiet reports that the agent is not in a long-poll and has not been seen
// or acted within the stall window. The panel human is never quiet.
func quiet(a Agent, waiting, always bool, now time.Time) bool {
	if always || waiting {
		return false
	}
	return !within(latestUnix(a.LastSeenAt, a.LastActionAt), now, ActivityStallAfter)
}

func knownExplicit(state string) bool {
	switch state {
	case ActivityWorking, ActivityIdle, ActivityBlocked:
		return true
	default:
		return false
	}
}

// unixTime reads a stored timestamp. last_seen_at is unix seconds (the
// presence column every existing caller writes). activity_at and
// last_action_at are unix milliseconds so two events in the same second
// still have an order. A value past 1e12 is milliseconds; anything smaller
// is seconds.
func unixTime(ts *int64) (time.Time, bool) {
	if ts == nil || *ts <= 0 {
		return time.Time{}, false
	}
	if *ts > 1_000_000_000_000 {
		return time.UnixMilli(*ts), true
	}
	return time.Unix(*ts, 0), true
}

func within(ts *int64, now time.Time, window time.Duration) bool {
	at, ok := unixTime(ts)
	if !ok {
		return false
	}
	return now.Sub(at) < window
}

func newer(a, b *int64) bool {
	ta, oka := unixTime(a)
	tb, okb := unixTime(b)
	if !oka {
		return false
	}
	if !okb {
		return true
	}
	return ta.After(tb)
}

func latestUnix(a, b *int64) *int64 {
	ta, oka := unixTime(a)
	tb, okb := unixTime(b)
	if !oka {
		return b
	}
	if !okb {
		return a
	}
	if !ta.Before(tb) {
		return a
	}
	return b
}

func unixSeconds(p *int64) int64 {
	at, ok := unixTime(p)
	if !ok {
		return 0
	}
	return at.Unix()
}

func unixOr(p *int64, now time.Time) int64 {
	if s := unixSeconds(p); s > 0 {
		return s
	}
	return now.Unix()
}

// NormalizeActivity checks a self-report. state is working, idle, or blocked.
// note is optional, trimmed, at most ActivityNoteMax runes, and free of
// control characters. stalled and offline are derived, not reported.
func NormalizeActivity(state, note string) (string, string, error) {
	state = strings.ToLower(strings.TrimSpace(state))
	switch state {
	case ActivityWorking, ActivityIdle, ActivityBlocked:
	default:
		return "", "", fmt.Errorf("state must be working, idle, or blocked")
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > ActivityNoteMax {
		return "", "", fmt.Errorf("note exceeds %d characters", ActivityNoteMax)
	}
	for _, r := range note {
		if r < 0x20 || r == 0x7f {
			return "", "", fmt.Errorf("note contains a control character")
		}
	}
	return state, note, nil
}

// SetActivity stores an explicit self-report and refreshes presence. It does
// not count as a substantive action, so a report of "idle" is not immediately
// overridden by the call that sent it. A missing agent is ErrNotFound.
func (s *Store) SetActivity(ctx context.Context, id, state, note string) error {
	now := time.Now().UnixMilli()
	seen := time.Now().Unix()
	res, err := s.db.ExecContext(ctx, `UPDATE agents SET activity_state = ?, activity_note = ?, activity_at = ?, last_seen_at = ? WHERE id = ?`,
		state, note, now, seen, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetContactTimes overwrites last_seen_at and last_action_at (unix seconds).
// A nil pointer clears that column. Production paths use TouchAgent and
// TouchAction; this exists so activity can be evaluated against a known clock.
func (s *Store) SetContactTimes(ctx context.Context, id string, lastSeen, lastAction *int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE agents SET last_seen_at = ?, last_action_at = ? WHERE id = ?`,
		nullInt(lastSeen), nullInt(lastAction), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func nullInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// migrateAgents adds the activity columns to databases created before them.
// Each ALTER is skipped when the column is already there (a fresh database
// gets them from schema.sql).
func (s *Store) migrateAgents() error {
	cols, err := s.tableColumns("agents")
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return nil
	}
	alters := []struct{ name, ddl string }{
		{"activity_state", `ALTER TABLE agents ADD COLUMN activity_state TEXT NOT NULL DEFAULT ''`},
		{"activity_note", `ALTER TABLE agents ADD COLUMN activity_note TEXT NOT NULL DEFAULT ''`},
		{"activity_at", `ALTER TABLE agents ADD COLUMN activity_at INTEGER`},
		{"last_action_at", `ALTER TABLE agents ADD COLUMN last_action_at INTEGER`},
	}
	for _, a := range alters {
		if cols[a.name] {
			continue
		}
		if _, err := s.db.Exec(a.ddl); err != nil {
			return fmt.Errorf("migrate agents.%s: %w", a.name, err)
		}
	}
	return nil
}
