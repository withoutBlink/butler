package ui

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"butler/internal/store"
)

// projectPrefix marks a picker value that means "this working directory",
// not one agent. createSession expands it to one role per teammate in that
// project. A real agent id never has this shape.
const projectPrefix = "project:"

// rosterIdentity is one dashboard row. A project with two or more teammates
// is a group (ID is projectPrefix + workdir). Anything else — a teammate
// alone in its project, or an agent with no workdir — is a single row.
type rosterIdentity struct {
	ID       string
	Title    string
	Tip      string
	Kind     string
	Hostname string
	Workdir  string
	LastTxt  string
	Activity store.ActivityView
	Grouped  bool
	Kick     bool
	Members  []rosterMember
	// MemberLabel is the disclosure summary, e.g. "3 teammates".
	MemberLabel string
	Checked     bool
	recency     time.Time
}

// rosterMember is one teammate inside a project. OpenCode's per-session
// roles are already collapsed: ID is the live or most recently seen role,
// not every historical session.
type rosterMember struct {
	ID       string
	Title    string
	Tip      string
	Kind     string
	Hostname string
	Activity store.ActivityView
	Checked  bool
	recency  time.Time
	lastTxt  string
}

// rosterSession is one stored role while a teammate's representative is chosen.
type rosterSession struct {
	ID       string
	Title    string
	Activity store.ActivityView
	Live     bool
	appLevel bool
	recency  time.Time
	seen     time.Time
}

func projectToken(workdir string) string { return projectPrefix + workdir }

func projectWorkdir(token string) (string, bool) {
	wd, ok := strings.CutPrefix(token, projectPrefix)
	if !ok || strings.TrimSpace(wd) == "" {
		return "", false
	}
	return wd, true
}

// groupRoster groups the visible agents by working directory — the project
// they are in. agents is the ListAgents result (evicted agents and hidden
// clients are already gone). Inside a project, OpenCode's per-session roles
// collapse to one teammate: the live role, or the most recently seen role
// when nobody is live. A project with a single teammate is just that row.
// An empty workdir is not a project; each such agent stays on its own, so
// Admin is never filed under someone else's directory.
func groupRoster(agents []store.Agent, activity map[string]store.ActivityView, kindNames map[string]string) []rosterIdentity {
	type tkey struct{ kind, host string }
	type bucket struct {
		order []tkey
		teams map[tkey][]store.Agent
		solos []store.Agent
	}
	projects := map[string]*bucket{}
	var projectOrder []string
	var unscoped []store.Agent
	bucketFor := func(dir string) *bucket {
		b := projects[dir]
		if b == nil {
			b = &bucket{teams: map[tkey][]store.Agent{}}
			projects[dir] = b
			projectOrder = append(projectOrder, dir)
		}
		return b
	}
	for _, a := range agents {
		dir, ok := projectDir(a.Workdir)
		if !ok {
			unscoped = append(unscoped, a)
			continue
		}
		b := bucketFor(dir)
		if a.ClientSession == "" {
			b.solos = append(b.solos, a)
			continue
		}
		k := tkey{a.Kind, a.Hostname}
		if _, seen := b.teams[k]; !seen {
			b.order = append(b.order, k)
		}
		b.teams[k] = append(b.teams[k], a)
	}
	// An app-level identity joins its roles when that install already has
	// some. Otherwise it is its own teammate (Pi, DeepSeek, or a Codex
	// process that has not bound a thread). Codex threads register with
	// client_session set and collapse like OpenCode roles: one teammate
	// per install, the live or most recently seen thread.
	var out []rosterIdentity
	for _, dir := range projectOrder {
		b := projects[dir]
		var members []rosterMember
		used := map[tkey]bool{}
		for _, k := range b.order {
			people := b.teams[k]
			for _, solo := range b.solos {
				if solo.Kind == k.kind && solo.Hostname == k.host {
					people = append(people, solo)
					used[k] = true
				}
			}
			members = append(members, buildTeammate(people, activity, kindNames))
		}
		for _, solo := range b.solos {
			k := tkey{solo.Kind, solo.Hostname}
			if used[k] {
				continue
			}
			// Two app-level agents can share kind and host in tests; each
			// stays its own teammate because neither is a session role.
			if _, already := b.teams[k]; already {
				continue
			}
			members = append(members, buildTeammate([]store.Agent{solo}, activity, kindNames))
		}
		out = append(out, projectRows(dir, members)...)
	}
	for _, a := range unscoped {
		out = append(out, soloRow(buildTeammate([]store.Agent{a}, activity, kindNames), ""))
	}
	disambiguateProjects(out)
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := liveActivity(out[i].Activity.State), liveActivity(out[j].Activity.State)
		if li != lj {
			return li
		}
		if !out[i].recency.Equal(out[j].recency) {
			return out[i].recency.After(out[j].recency)
		}
		ti, tj := strings.ToLower(out[i].Title), strings.ToLower(out[j].Title)
		if ti != tj {
			return ti < tj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func projectDir(workdir string) (string, bool) {
	w := strings.TrimSpace(workdir)
	if w == "" || w == "." || w == "/" {
		return "", false
	}
	return w, true
}

func projectRows(dir string, members []rosterMember) []rosterIdentity {
	sortMembers(members)
	if len(members) == 1 {
		return []rosterIdentity{soloRow(members[0], dir)}
	}
	host := members[0].Hostname
	for _, m := range members[1:] {
		if m.Hostname != host {
			host = ""
			break
		}
	}
	names := make([]string, len(members))
	for i, m := range members {
		names[i] = m.Title
	}
	row := rosterIdentity{
		ID:          projectToken(dir),
		Title:       projectLabel(dir),
		Workdir:     dir,
		Hostname:    host,
		Grouped:     true,
		Members:     members,
		MemberLabel: teammateLabel(len(members)),
		Activity:    members[0].Activity,
		LastTxt:     seenText(members[0].recency),
		recency:     members[0].recency,
	}
	row.Tip = dir + " — invites " + strings.Join(names, ", ")
	row.LastTxt = members[0].lastTxt
	return []rosterIdentity{row}
}

func soloRow(m rosterMember, dir string) rosterIdentity {
	return rosterIdentity{
		ID:       m.ID,
		Title:    m.Title,
		Tip:      m.Tip,
		Kind:     m.Kind,
		Hostname: m.Hostname,
		Workdir:  dir,
		LastTxt:  m.lastTxt,
		Activity: m.Activity,
		Kick:     m.ID != AdminAgentID,
		recency:  m.recency,
	}
}

func buildTeammate(members []store.Agent, activity map[string]store.ActivityView, kindNames map[string]string) rosterMember {
	sessions := make([]rosterSession, len(members))
	for i, a := range members {
		title := sessionTitle(a)
		sessions[i] = rosterSession{
			ID:       a.ID,
			Title:    title,
			Activity: activity[a.ID],
			Live:     liveActivity(activity[a.ID].State),
			appLevel: a.ClientSession == "",
			recency:  agentRecency(a),
			seen:     seenAt(a),
		}
	}
	chosen := sessions[chooseDefault(sessions)]
	kind := members[0].Kind
	title := chosen.Title
	tip := labelTip(title, chosen.ID)
	if len(members) > 1 {
		title = kindNames[kind]
		if title == "" {
			title = kind
		}
		tip = title + " — " + inviteTip(chosen.Title, chosen.ID)
	}
	return rosterMember{
		ID:       chosen.ID,
		Title:    title,
		Tip:      tip,
		Kind:     kind,
		Hostname: members[0].Hostname,
		Activity: chosen.Activity,
		recency:  chosen.recency,
		lastTxt:  seenText(chosen.seen),
	}
}

// chooseDefault picks the role a teammate checkbox invites. A live session
// role wins over the app-level client (that client is hidden once roles
// exist, and inviting it would not show up in the meeting). Otherwise the
// most recently seen role wins, so a quiet install is still invited.
func chooseDefault(members []rosterSession) int {
	if i := bestIndex(members, func(s rosterSession) bool { return s.Live && !s.appLevel }); i >= 0 {
		return i
	}
	if i := bestIndex(members, func(s rosterSession) bool { return s.Live }); i >= 0 {
		return i
	}
	if i := bestIndex(members, func(s rosterSession) bool { return !s.appLevel }); i >= 0 {
		return i
	}
	return bestIndex(members, func(rosterSession) bool { return true })
}

func bestIndex(members []rosterSession, pred func(rosterSession) bool) int {
	best := -1
	for i, m := range members {
		if !pred(m) {
			continue
		}
		if best < 0 || fresher(m, members[best]) {
			best = i
		}
	}
	return best
}

func fresher(a, b rosterSession) bool {
	if !a.recency.Equal(b.recency) {
		return a.recency.After(b.recency)
	}
	return a.ID < b.ID
}

func sortSessions(sessions []rosterSession) {
	sort.SliceStable(sessions, func(i, j int) bool {
		return fresher(sessions[i], sessions[j])
	})
}

func sortMembers(members []rosterMember) {
	sort.SliceStable(members, func(i, j int) bool {
		li, lj := liveActivity(members[i].Activity.State), liveActivity(members[j].Activity.State)
		if li != lj {
			return li
		}
		if !members[i].recency.Equal(members[j].recency) {
			return members[i].recency.After(members[j].recency)
		}
		ti, tj := strings.ToLower(members[i].Title), strings.ToLower(members[j].Title)
		if ti != tj {
			return ti < tj
		}
		return members[i].ID < members[j].ID
	})
}

func projectLabel(workdir string) string {
	base := path.Base(workdir)
	if base == "" || base == "." || base == "/" {
		return workdir
	}
	return base
}

func teammateLabel(n int) string {
	if n == 1 {
		return "1 teammate"
	}
	return fmt.Sprintf("%d teammates", n)
}

func seenText(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format("15:04:05")
}

// disambiguateProjects appends the parent directory when two projects share
// a basename. The tooltip already carries the full path.
func disambiguateProjects(rows []rosterIdentity) {
	counts := map[string]int{}
	for i := range rows {
		if rows[i].Grouped {
			counts[strings.ToLower(rows[i].Title)]++
		}
	}
	for i := range rows {
		if !rows[i].Grouped || counts[strings.ToLower(rows[i].Title)] < 2 {
			continue
		}
		parent := path.Base(path.Dir(rows[i].Workdir))
		if parent == "" || parent == "." || parent == "/" || strings.HasSuffix(rows[i].Title, parent) {
			continue
		}
		rows[i].Title += " · " + parent
		rows[i].Tip = rows[i].Workdir + " — " + strings.TrimPrefix(rows[i].Tip, rows[i].Workdir+" — ")
	}
}

func sessionTitle(a store.Agent) string {
	if a.DisplayName != "" {
		return a.DisplayName
	}
	return a.ID
}

func inviteTip(title, id string) string {
	if title == "" || title == id {
		return "Invites " + id
	}
	return "Invites " + title + " (" + id + ")"
}

// labelTip is the hover text for a truncated name: the full title, and the
// id when the title is a separate display name.
func labelTip(title, id string) string {
	if title == "" || title == id {
		return id
	}
	return title + " (" + id + ")"
}

// pickerModel is the new-session roster after an optional filter. Hidden
// holds selected agent ids that the filter scrolled out of the visible
// checkboxes; they stay in the form so a checked teammate is not dropped.
type pickerModel struct {
	Roster []rosterIdentity
	Hidden []string
	Query  string
	Total  int
	Shown  int
}

// filterRoster narrows the picker. An empty query returns every non-admin
// row. A query matches a project path or a teammate name. selected values
// that belong to the roster but are not on screen — an agent id or a
// project token — come back in Hidden so the filter cannot drop a check.
func filterRoster(roster []rosterIdentity, query string, selected map[string]bool) pickerModel {
	q := strings.TrimSpace(query)
	if utf8.RuneCountInString(q) > 200 {
		q = string([]rune(q)[:200])
	}
	total := 0
	for _, idn := range roster {
		if idn.ID != AdminAgentID {
			total++
		}
	}
	shown := make([]rosterIdentity, 0, len(roster))
	rendered := map[string]bool{}
	for _, idn := range roster {
		if idn.ID == AdminAgentID {
			continue
		}
		next, ok := narrowIdentity(idn, q)
		if !ok {
			continue
		}
		markSelected(&next, selected)
		shown = append(shown, next)
		rendered[next.ID] = true
		for _, m := range next.Members {
			rendered[m.ID] = true
		}
	}
	var hidden []string
	for id := range selected {
		id = strings.TrimSpace(id)
		if id == "" || id == AdminAgentID || rendered[id] || !rosterKnows(roster, id) {
			continue
		}
		hidden = append(hidden, id)
	}
	sort.Strings(hidden)
	return pickerModel{Roster: shown, Hidden: hidden, Query: q, Total: total, Shown: len(shown)}
}

func narrowIdentity(idn rosterIdentity, q string) (rosterIdentity, bool) {
	if q == "" {
		return idn, true
	}
	if identityFieldsMatch(idn, q) {
		return idn, true
	}
	var matched []rosterMember
	for _, m := range idn.Members {
		if containsFold(m.Title, q) || containsFold(m.ID, q) || containsFold(m.Kind, q) || containsFold(m.Tip, q) {
			matched = append(matched, m)
		}
	}
	if len(matched) == 0 {
		return rosterIdentity{}, false
	}
	idn.Members = matched
	idn.MemberLabel = teammateLabel(len(matched))
	return idn, true
}

func identityFieldsMatch(idn rosterIdentity, q string) bool {
	for _, f := range []string{idn.Title, idn.Kind, idn.Hostname, idn.Workdir, idn.Tip} {
		if containsFold(f, q) {
			return true
		}
	}
	return false
}

func containsFold(hay, needle string) bool {
	return needle != "" && strings.Contains(strings.ToLower(hay), strings.ToLower(needle))
}

func markSelected(idn *rosterIdentity, selected map[string]bool) {
	if len(selected) == 0 {
		return
	}
	idn.Checked = selected[idn.ID]
	for i := range idn.Members {
		idn.Members[i].Checked = selected[idn.Members[i].ID]
	}
}

func rosterKnows(roster []rosterIdentity, id string) bool {
	for _, idn := range roster {
		if idn.ID == id {
			return true
		}
		for _, m := range idn.Members {
			if m.ID == id {
				return true
			}
		}
	}
	return false
}

func seenAt(a store.Agent) time.Time {
	if a.LastSeenAt == nil || *a.LastSeenAt <= 0 {
		return time.Time{}
	}
	return time.Unix(*a.LastSeenAt, 0).UTC()
}

// liveActivity is a role a person can still reach: the presence window (or a
// held long-poll, or a fresh self-report) produced working, idle, or blocked.
// Stalled and offline are historical for the picker — they stay collapsed.
func liveActivity(state string) bool {
	switch state {
	case store.ActivityWorking, store.ActivityIdle, store.ActivityBlocked:
		return true
	default:
		return false
	}
}

// agentRecency is the later of last seen and last action. last_seen_at is
// unix seconds; last_action_at is unix milliseconds (values past 1e12).
func agentRecency(a store.Agent) time.Time {
	var best time.Time
	if t, ok := contactTime(a.LastSeenAt); ok && t.After(best) {
		best = t
	}
	if t, ok := contactTime(a.LastActionAt); ok && t.After(best) {
		best = t
	}
	return best
}

func contactTime(ts *int64) (time.Time, bool) {
	if ts == nil || *ts <= 0 {
		return time.Time{}, false
	}
	if *ts > 1_000_000_000_000 {
		return time.UnixMilli(*ts), true
	}
	return time.Unix(*ts, 0), true
}
