package ui

import (
	"os"
	"strings"
	"testing"

	"butler/internal/store"
)

func TestGroupRosterByWorkdir(t *testing.T) {
	const dir = "/work/app"
	liveNew := agent("role-new", "Ship the roster", "opencode", "laptop", dir, "ses-new", 2_000)
	liveOld := agent("role-old-live", "Review the board", "opencode", "laptop", dir, "ses-old-live", 1_500)
	stale := agent("role-stale", "Ancient notes", "opencode", "laptop", dir, "ses-stale", 100)
	codex := agent("codex-1", "Codex", "codex", "laptop", dir, "", 1_800)
	pi := agent("pi-1", "Pi", "pi", "laptop", "/work/pi", "", 0)
	admin := agent("admin", "Admin", "human", "", "", "", 0)
	otherA := agent("other-a", "Older board", "opencode", "otherbox", "/work/other", "ses-a", 300)
	otherB := agent("other-b", "Even older", "opencode", "otherbox", "/work/other", "ses-b", 100)

	activity := map[string]store.ActivityView{
		liveNew.ID: {State: store.ActivityIdle},
		liveOld.ID: {State: store.ActivityWorking},
		stale.ID:   {State: store.ActivityOffline},
		codex.ID:   {State: store.ActivityIdle},
		pi.ID:      {State: store.ActivityOffline},
		admin.ID:   {State: store.ActivityIdle},
		otherA.ID:  {State: store.ActivityOffline},
		otherB.ID:  {State: store.ActivityOffline},
	}
	roster := groupRoster([]store.Agent{stale, liveOld, pi, otherB, codex, liveNew, admin, otherA}, activity, map[string]string{
		"opencode": "OpenCode",
	})
	byTitle := map[string]rosterIdentity{}
	for _, idn := range roster {
		byTitle[idn.Title] = idn
	}
	// Codex and the OpenCode sessions share a project. Pi is alone in its
	// project, so it is just Pi. Admin has no workdir and stays ungrouped.
	project, ok := byTitle["app"]
	if !ok || !project.Grouped || project.ID != "project:"+dir || project.Workdir != dir {
		t.Fatalf("project row: %+v titles %v", project, titles(roster))
	}
	if project.Kick || project.MemberLabel != "2 teammates" {
		t.Fatalf("project shape: %+v", project)
	}
	if memberIDs(project.Members) != "role-new,codex-1" {
		t.Fatalf("teammates: %s", memberIDs(project.Members))
	}
	if project.Members[0].Title != "OpenCode" || !strings.Contains(project.Members[0].Tip, "Ship the roster") {
		t.Fatalf("opencode teammate should name the live role: %+v", project.Members[0])
	}
	if !strings.Contains(project.Tip, dir) || !strings.Contains(project.Tip, "Codex") {
		t.Fatalf("project tip: %q", project.Tip)
	}
	piRow := byTitle["Pi"]
	if piRow.Grouped || piRow.ID != "pi-1" || len(piRow.Members) != 0 || piRow.Workdir != "/work/pi" {
		t.Fatalf("single-agent project: %+v", piRow)
	}
	adminRow := byTitle["Admin"]
	if adminRow.Grouped || adminRow.Workdir != "" || adminRow.Kick {
		t.Fatalf("admin: %+v", adminRow)
	}
	other := byTitle["OpenCode"]
	if other.Grouped || other.ID != "other-a" || other.Workdir != "/work/other" {
		t.Fatalf("sibling roles in their own project collapse to one teammate: %+v", other)
	}
}

func TestGroupRosterInviteRules(t *testing.T) {
	// A lone role is the teammate. One session must not become a disclosure.
	lone := agent("role-only", "Fix read marks", "opencode", "h", "/w", "ses", 50)
	roster := groupRoster([]store.Agent{lone}, map[string]store.ActivityView{
		lone.ID: {State: store.ActivityIdle},
	}, map[string]string{"opencode": "OpenCode"})
	if len(roster) != 1 || roster[0].Grouped || roster[0].Title != "Fix read marks" || roster[0].ID != "role-only" {
		t.Fatalf("lone role: %+v", roster)
	}

	// No kind display name → the kind id is the group label.
	a := agent("a", "A", "opencode", "h", "/w", "sa", 10)
	b := agent("b", "B", "opencode", "h", "/w", "sb", 20)
	roster = groupRoster([]store.Agent{a, b}, map[string]store.ActivityView{
		"a": {State: store.ActivityOffline},
		"b": {State: store.ActivityIdle},
	}, nil)
	if roster[0].Title != "opencode" || roster[0].ID != "b" {
		t.Fatalf("kind-id fallback: %+v", roster[0])
	}

	// Equal recency: the lower id wins, so the choice is stable.
	t1 := agent("m", "M", "opencode", "h", "/w", "sm", 20)
	t2 := agent("a", "A", "opencode", "h", "/w", "sa", 20)
	roster = groupRoster([]store.Agent{t1, t2}, map[string]store.ActivityView{
		"m": {State: store.ActivityIdle},
		"a": {State: store.ActivityIdle},
	}, nil)
	if roster[0].ID != "a" {
		t.Fatalf("tie break: got %s", roster[0].ID)
	}

	// A last_action in milliseconds outranks an older last_seen in seconds.
	recentAction := agent("act", "Act", "opencode", "h", "/w", "sact", 1_700_000_000)
	ms := int64(2_000_000_000_000)
	recentAction.LastActionAt = &ms
	older := agent("seen", "Seen", "opencode", "h", "/w", "sseen", 1_800_000_000)
	roster = groupRoster([]store.Agent{older, recentAction}, map[string]store.ActivityView{
		"act":  {State: store.ActivityIdle},
		"seen": {State: store.ActivityIdle},
	}, nil)
	if roster[0].ID != "act" {
		t.Fatalf("last_action should win recency, got %s", roster[0].ID)
	}

	// A live role is the teammate even if the app-level client is also in
	// the list. The client is not who the checkbox invites.
	client := agent("client", "client", "opencode", "h", "/w", "", 10)
	role := agent("role", "Live role", "opencode", "h", "/w", "ses", 99)
	roster = groupRoster([]store.Agent{role, client}, map[string]store.ActivityView{
		"client": {State: store.ActivityOffline},
		"role":   {State: store.ActivityWorking},
	}, map[string]string{"opencode": "OpenCode"})
	if len(roster) != 1 || roster[0].ID != "role" || roster[0].Title != "OpenCode" {
		t.Fatalf("live role should be the invite: %+v", roster)
	}

	// Untitled identity: the id is the label, and there is nothing else to show.
	untitled := agent("uuid-1", "", "codex", "h", "/w", "", 0)
	roster = groupRoster([]store.Agent{untitled}, nil, nil)
	if roster[0].Title != "uuid-1" || roster[0].Tip != "uuid-1" || roster[0].Grouped {
		t.Fatalf("untitled: %+v", roster[0])
	}
}

func TestGroupRosterSameHostDifferentWorkdir(t *testing.T) {
	a := agent("a", "A", "opencode", "laptop", "/work/alpha", "sa", 10)
	b := agent("b", "B", "opencode", "laptop", "/work/alpha", "sb", 20)
	c := agent("c", "C", "opencode", "laptop", "/work/beta", "sc", 10)
	d := agent("d", "D", "opencode", "laptop", "/work/beta", "sd", 30)
	roster := groupRoster([]store.Agent{a, b, c, d}, map[string]store.ActivityView{
		"a": {State: store.ActivityOffline},
		"b": {State: store.ActivityOffline},
		"c": {State: store.ActivityOffline},
		"d": {State: store.ActivityIdle},
	}, map[string]string{"opencode": "OpenCode"})
	if len(roster) != 2 {
		t.Fatalf("got %d rows", len(roster))
	}
	got := map[string]string{}
	for _, idn := range roster {
		got[idn.Workdir] = idn.ID
	}
	// Each directory has only OpenCode roles, so each is one teammate,
	// labeled by the kind, not a project disclosure.
	if got["/work/alpha"] != "b" || got["/work/beta"] != "d" {
		t.Fatalf("one teammate per project: %v", got)
	}
}

func TestCondenseParticipants(t *testing.T) {
	long := strings.Repeat("Very long participant title ", 4)
	people := []dashPerson{
		{Agent: store.Agent{ID: "old-b", DisplayName: long + "old-b"}, Activity: store.ActivityView{State: store.ActivityOffline}},
		{Agent: store.Agent{ID: "live-old", DisplayName: long + "live-old", LastSeenAt: unixPtr(100)}, Activity: store.ActivityView{State: store.ActivityIdle}},
		{Agent: store.Agent{ID: "old-a", DisplayName: long + "old-a"}, Activity: store.ActivityView{State: store.ActivityOffline}},
		{Agent: store.Agent{ID: "live-new", DisplayName: long + "live-new", LastSeenAt: unixPtr(200)}, Activity: store.ActivityView{State: store.ActivityWorking}},
		{Agent: store.Agent{ID: "old-c", DisplayName: long + "old-c"}, Activity: store.ActivityView{State: store.ActivityOffline}},
		{Agent: store.Agent{ID: "old-d", DisplayName: long + "old-d"}, Activity: store.ActivityView{State: store.ActivityOffline}},
	}
	// Give the offline ones a recency order: old-a newest among them.
	people[0].LastSeenAt = unixPtr(10)
	people[2].LastSeenAt = unixPtr(40)
	people[4].LastSeenAt = unixPtr(30)
	people[5].LastSeenAt = unixPtr(20)

	shown, more, label := condenseParticipants(people)
	if label != "+2 more" || len(shown) != dashParticipantLimit || len(more) != 2 {
		t.Fatalf("split: shown %d more %d label %q", len(shown), len(more), label)
	}
	if idsOf(shown) != "live-new,live-old,old-a,old-c" {
		t.Fatalf("shown order: %s", idsOf(shown))
	}
	if shown[0].Tip != long+"live-new (live-new)" {
		t.Fatalf("tip: %q", shown[0].Tip)
	}
	if idsOf(more) != "old-d,old-b" {
		t.Fatalf("more order: %s", idsOf(more))
	}

	small, rest, none := condenseParticipants(people[:2])
	if len(small) != 2 || rest != nil || none != "" {
		t.Fatalf("a short meeting stays open: %d %v %q", len(small), rest, none)
	}
}

func idsOf(people []dashPerson) string {
	out := make([]string, len(people))
	for i, p := range people {
		out[i] = p.ID
	}
	return strings.Join(out, ",")
}

func unixPtr(n int64) *int64 { return &n }

func TestRosterCSSIsCompact(t *testing.T) {
	b, err := os.ReadFile("static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	for _, want := range []string{
		".picks-scroll { max-height: 240px; overflow: auto;",
		".agents-scroll { max-height: 360px; overflow: auto;",
		".part-more .part-list { max-height: 140px; overflow: auto;",
		"text-overflow: ellipsis",
		"position: sticky",
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("style.css missing %q", want)
		}
	}
}

func TestRosterSortsLiveFirst(t *testing.T) {
	offline := agent("aaa", "Aaa early name", "codex", "h", "/w", "", 10)
	live := agent("zzz", "Zzz later name", "pi", "h", "/p", "", 500)
	roster := groupRoster([]store.Agent{offline, live}, map[string]store.ActivityView{
		offline.ID: {State: store.ActivityOffline},
		live.ID:    {State: store.ActivityIdle},
	}, nil)
	if len(roster) != 2 || roster[0].ID != "zzz" || roster[1].ID != "aaa" {
		t.Fatalf("live teammate should sort above an earlier offline name: %v", titles(roster))
	}
}

func TestFilterRosterKeepsSelection(t *testing.T) {
	long := strings.Repeat("Integration ", 12) + "parser"
	a := agent("long-1", long, "codex", "h", "/work/very/long/path", "", 50)
	b := agent("other", "Other person", "pi", "h", "/p", "", 10)
	groupA := agent("role-parser", "Port the parser", "opencode", "box", "/app", "s1", 5)
	groupB := agent("role-other", "Unrelated notes", "opencode", "box", "/app", "s2", 4)
	roster := groupRoster([]store.Agent{a, b, groupA, groupB}, map[string]store.ActivityView{
		a.ID:      {State: store.ActivityOffline},
		b.ID:      {State: store.ActivityOffline},
		groupA.ID: {State: store.ActivityOffline},
		groupB.ID: {State: store.ActivityOffline},
	}, map[string]string{"opencode": "OpenCode"})

	all := filterRoster(roster, "", nil)
	if all.Total != 3 || all.Shown != 3 || len(all.Hidden) != 0 {
		t.Fatalf("unfiltered: %+v", all)
	}
	// The full title is the tooltip, not something the filter throws away.
	var found bool
	for _, idn := range all.Roster {
		if idn.ID == "long-1" && idn.Tip == long+" (long-1)" && idn.Title == long {
			found = true
		}
	}
	if !found {
		t.Fatalf("long title missing from picker model: %+v", all.Roster)
	}

	narrow := filterRoster(roster, "Port the", map[string]bool{"other": true, "ghost": true})
	if narrow.Shown != 1 || narrow.Total != 3 {
		t.Fatalf("shown %d total %d", narrow.Shown, narrow.Total)
	}
	if len(narrow.Hidden) != 1 || narrow.Hidden[0] != "other" {
		t.Fatalf("hidden selection: %v", narrow.Hidden)
	}
	var opencode rosterIdentity
	for _, idn := range narrow.Roster {
		if idn.Title == "OpenCode" {
			opencode = idn
		}
		if idn.Title == "Other person" {
			t.Fatal("filter should drop Other person")
		}
	}
	if opencode.ID != "role-parser" || opencode.Grouped || !strings.Contains(opencode.Tip, "Port the parser") {
		t.Fatalf("session roles should be one teammate, inviting the recent one: %+v", opencode)
	}
}

func agent(id, name, kind, host, dir, session string, seen int64) store.Agent {
	a := store.Agent{
		ID: id, DisplayName: name, Kind: kind, Hostname: host, Workdir: dir, ClientSession: session,
	}
	if seen > 0 {
		s := seen
		a.LastSeenAt = &s
	}
	return a
}

func titles(roster []rosterIdentity) []string {
	out := make([]string, len(roster))
	for i, idn := range roster {
		out[i] = idn.Title
	}
	return out
}

func memberIDs(members []rosterMember) string {
	out := make([]string, len(members))
	for i, m := range members {
		out[i] = m.ID
	}
	return strings.Join(out, ",")
}
