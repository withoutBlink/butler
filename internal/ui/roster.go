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

// rosterIdentity is one teammate on the dashboard: a single listable agent,
// or every visible sibling that shares kind + hostname + workdir.
//
// ListAgents hides the app-level client once it has a live role, so a group
// is roles only. The identity checkbox still invites one concrete agent
// (ID): the client when it is actually in the group, otherwise the most
// recently seen live role, otherwise the most recently seen role. Checking
// the box must not invite every historical session, and it must not invite
// nobody. Nested session checkboxes submit those role ids as well; the
// create-session handler joins the union.
type rosterIdentity struct {
	ID       string
	Title    string
	Tip      string // title attribute: the agent id, or which session an identity check invites
	Kind     string
	Hostname string
	Workdir  string
	LastTxt  string
	Activity store.ActivityView
	Grouped  bool
	// KindLabel is set when Title came from the kind, so two installs of the
	// same kind can be disambiguated. Templates do not read it.
	KindLabel    bool
	Kick         bool // parent-row kick; a group kicks the nested roles instead
	Live         []rosterSession
	Earlier      []rosterSession
	LiveLabel    string
	EarlierLabel string
	// Invite is a short visible hint on a grouped teammate: which session the
	// identity checkbox joins. Empty for a single-identity teammate, so the
	// choice stays visible even while historical sessions are collapsed.
	Invite string
	// Checked is set by the picker filter so a selection survives a re-render.
	Checked bool
	// ForceOpen expands the historical disclosure when a filter matched a
	// session that would otherwise be collapsed.
	ForceOpen bool
	recency   time.Time
}

// rosterSession is one role nested under a grouped identity.
type rosterSession struct {
	ID       string
	Title    string
	Activity store.ActivityView
	Tip      string
	Default  bool
	Checked  bool
	Live     bool
	appLevel bool
	recency  time.Time
	seen     time.Time // last_seen_at, for the Seen column (not the sort key)
}

// groupRoster collapses visible agents into teammate identities. agents is
// the ListAgents result (evicted agents and hidden clients already removed).
// activity is the derived snapshot keyed by agent id. kindNames maps a kind
// id to its display name; a missing name falls back to the kind id.
func groupRoster(agents []store.Agent, activity map[string]store.ActivityView, kindNames map[string]string) []rosterIdentity {
	type key struct{ kind, hostname, workdir string }
	order := []key{}
	groups := map[key][]store.Agent{}
	for _, a := range agents {
		k := key{a.Kind, a.Hostname, a.Workdir}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], a)
	}
	out := make([]rosterIdentity, 0, len(order))
	for _, k := range order {
		out = append(out, buildIdentity(k.kind, k.hostname, k.workdir, groups[k], activity, kindNames))
	}
	disambiguateKinds(out)
	for i := range out {
		if !out[i].Grouped {
			continue
		}
		// Title may have gained a host suffix after the tip was built.
		if _, rest, ok := strings.Cut(out[i].Tip, " — "); ok {
			out[i].Tip = out[i].Title + " — " + rest
		}
	}
	// Online and recently seen teammates first, so a long roster does not
	// bury the people who can actually be invited right now. Title breaks ties.
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

func buildIdentity(kind, hostname, workdir string, members []store.Agent, activity map[string]store.ActivityView, kindNames map[string]string) rosterIdentity {
	sessions := make([]rosterSession, len(members))
	for i, a := range members {
		act := activity[a.ID]
		title := sessionTitle(a)
		sessions[i] = rosterSession{
			ID:       a.ID,
			Title:    title,
			Tip:      labelTip(title, a.ID),
			Activity: act,
			Live:     liveActivity(act.State),
			appLevel: a.ClientSession == "",
			recency:  agentRecency(a),
			seen:     seenAt(a),
		}
	}
	chosen := chooseDefault(sessions)
	idn := rosterIdentity{
		Kind:     kind,
		Hostname: hostname,
		Workdir:  workdir,
		Activity: sessions[chosen].Activity,
		LastTxt:  "never",
		ID:       sessions[chosen].ID,
		recency:  sessions[chosen].recency,
	}
	if t := sessions[chosen].seen; !t.IsZero() {
		idn.LastTxt = t.Format("15:04:05")
	}
	// A lone listable agent is the teammate. Several roles of one install
	// collapse under the kind's name; the checkbox still posts one role id.
	if len(sessions) == 1 {
		idn.Title = sessions[0].Title
		idn.Tip = sessions[0].Tip
		idn.Kick = sessions[0].ID != AdminAgentID
		return idn
	}
	sessions[chosen].Default = true
	idn.Grouped = true
	idn.KindLabel = true
	idn.Title = kindNames[kind]
	if idn.Title == "" {
		idn.Title = kind
	}
	if idn.Title == "" {
		idn.Title = sessions[chosen].Title
	}
	idn.Tip = idn.Title + " — " + inviteTip(sessions[chosen].Title, sessions[chosen].ID)
	idn.Invite = "invites " + sessions[chosen].Title
	var live, earlier []rosterSession
	for _, s := range sessions {
		if s.Live {
			live = append(live, s)
		} else {
			earlier = append(earlier, s)
		}
	}
	sortSessions(live)
	sortSessions(earlier)
	idn.Live = live
	idn.Earlier = earlier
	if len(live) > 0 {
		idn.LiveLabel = countLabel(len(live), "live ")
	}
	if len(earlier) > 0 {
		prefix := ""
		if len(live) > 0 {
			prefix = "earlier "
		}
		idn.EarlierLabel = countLabel(len(earlier), prefix)
	}
	return idn
}

// chooseDefault picks who the identity checkbox invites.
//
// A listable app-level agent (empty client_session) wins: that is the Pi /
// Codex / DeepSeek case, one wait loop for the whole teammate. ListAgents
// omits that client once any live role exists, so a sibling group falls
// through to the most recently seen live role (working, idle, or blocked —
// the role whose wait loop is actually running). If every role is offline,
// the most recently seen role is still invited: its cursor is persisted, so
// the backlog reaches that session when it next activates, and the checkbox
// does not silently invite nobody.
func chooseDefault(members []rosterSession) int {
	if i := bestIndex(members, func(s rosterSession) bool { return s.appLevel }); i >= 0 {
		return i
	}
	if i := bestIndex(members, func(s rosterSession) bool { return s.Live }); i >= 0 {
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

// disambiguateKinds appends the host (then the workdir base) when two
// grouped installs would otherwise share a kind label.
func disambiguateKinds(rows []rosterIdentity) {
	colliding := func() map[string]int {
		n := map[string]int{}
		for i := range rows {
			if rows[i].KindLabel {
				n[strings.ToLower(rows[i].Title)]++
			}
		}
		return n
	}
	counts := colliding()
	for i := range rows {
		if !rows[i].KindLabel || counts[strings.ToLower(rows[i].Title)] < 2 {
			continue
		}
		extra := rows[i].Hostname
		if extra == "" {
			extra = path.Base(rows[i].Workdir)
		}
		if extra != "" && extra != "." && extra != "/" {
			rows[i].Title += " · " + extra
		}
	}
	counts = colliding()
	for i := range rows {
		if !rows[i].KindLabel || counts[strings.ToLower(rows[i].Title)] < 2 {
			continue
		}
		base := path.Base(rows[i].Workdir)
		if base == "" || base == "." || base == "/" || strings.HasSuffix(rows[i].Title, base) {
			continue
		}
		rows[i].Title += " · " + base
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
// teammate. A query matches the identity (title, kind, host, workdir) or a
// nested session title; session-only matches are shown and their disclosure
// is opened. selected ids that belong to the roster but are not visible as
// checkboxes are returned in Hidden.
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
		for _, s := range next.Live {
			rendered[s.ID] = true
		}
		for _, s := range next.Earlier {
			rendered[s.ID] = true
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
	match := func(s rosterSession) bool {
		return containsFold(s.Title, q) || containsFold(s.ID, q)
	}
	var live, earlier []rosterSession
	for _, s := range idn.Live {
		if match(s) {
			live = append(live, s)
		}
	}
	for _, s := range idn.Earlier {
		if match(s) {
			earlier = append(earlier, s)
		}
	}
	if len(live)+len(earlier) == 0 {
		return rosterIdentity{}, false
	}
	idn.Live = live
	idn.Earlier = earlier
	idn.ForceOpen = len(earlier) > 0
	idn.LiveLabel = ""
	idn.EarlierLabel = ""
	if len(live) > 0 {
		idn.LiveLabel = countLabel(len(live), "live ")
	}
	if len(earlier) > 0 {
		prefix := ""
		if len(live) > 0 {
			prefix = "earlier "
		}
		idn.EarlierLabel = countLabel(len(earlier), prefix)
	}
	return idn, true
}

func identityFieldsMatch(idn rosterIdentity, q string) bool {
	// Invite embeds the default session title, so it is not an identity
	// field: a query for some other session must narrow the disclosure
	// instead of showing every historical role.
	for _, f := range []string{idn.Title, idn.Kind, idn.Hostname, idn.Workdir, idn.ID} {
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
	for i := range idn.Live {
		idn.Live[i].Checked = selected[idn.Live[i].ID]
	}
	for i := range idn.Earlier {
		idn.Earlier[i].Checked = selected[idn.Earlier[i].ID]
	}
}

func rosterKnows(roster []rosterIdentity, id string) bool {
	for _, idn := range roster {
		if idn.ID == id {
			return true
		}
		for _, s := range idn.Live {
			if s.ID == id {
				return true
			}
		}
		for _, s := range idn.Earlier {
			if s.ID == id {
				return true
			}
		}
	}
	return false
}

func countLabel(n int, prefix string) string {
	word := "sessions"
	if n == 1 {
		word = "session"
	}
	return fmt.Sprintf("%d %s%s", n, prefix, word)
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
