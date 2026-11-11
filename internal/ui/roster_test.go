package ui

import (
	"os"
	"strings"
	"testing"
	"time"

	"butler/internal/store"
)

func TestGroupRosterCrowdedSiblings(t *testing.T) {
	const (
		kind = "opencode"
		host = "laptop"
		dir  = "/work/app"
	)
	liveNew := agent("role-new", "Ship the roster", kind, host, dir, "ses-new", 2_000)
	liveOld := agent("role-old-live", "Review the board", kind, host, dir, "ses-old-live", 1_500)
	stale := agent("role-stale", "Ancient notes", kind, host, dir, "ses-stale", 100)
	stalled := agent("role-stalled", "Stuck migration", kind, host, dir, "ses-stalled", 200)
	codex := agent("codex-1", "Codex", "codex", "laptop", "/work/codex", "", 2_000)
	pi := agent("pi-1", "Pi", "pi", "laptop", "/work/pi", "", 0)
	// A second install of the same kind must not merge with the first, and
	// must not share an unqualified "OpenCode" label.
	otherA := agent("other-a", "Older board", kind, "otherbox", "/work/other", "ses-a", 300)
	otherB := agent("other-b", "Even older", kind, "otherbox", "/work/other", "ses-b", 100)

	activity := map[string]store.ActivityView{
		liveNew.ID: {State: store.ActivityIdle},
		liveOld.ID: {State: store.ActivityWorking},
		stale.ID:   {State: store.ActivityOffline},
		stalled.ID: {State: store.ActivityStalled},
		codex.ID:   {State: store.ActivityIdle},
		pi.ID:      {State: store.ActivityOffline},
		otherA.ID:  {State: store.ActivityOffline},
		otherB.ID:  {State: store.ActivityOffline},
	}
	// The hidden app-level client is not in this list — ListAgents already
	// removed it. Grouping must not invent it.
	roster := groupRoster([]store.Agent{stale, liveOld, pi, otherB, codex, liveNew, stalled, otherA}, activity, map[string]string{
		kind: "OpenCode",
	})

	if len(roster) != 4 {
		t.Fatalf("identities: got %d, want 4 (codex, pi, two opencode installs)", len(roster))
	}
	byTitle := map[string]rosterIdentity{}
	for _, idn := range roster {
		byTitle[idn.Title] = idn
	}
	codexRow, ok := byTitle["Codex"]
	if !ok || codexRow.Grouped || codexRow.ID != "codex-1" || codexRow.Tip != "Codex (codex-1)" || !codexRow.Kick {
		t.Fatalf("codex row: %+v", codexRow)
	}
	if len(codexRow.Live) != 0 || len(codexRow.Earlier) != 0 {
		t.Fatalf("single identity should not nest sessions: %+v", codexRow)
	}
	piRow := byTitle["Pi"]
	if piRow.Grouped || piRow.ID != "pi-1" || piRow.Kick != true || piRow.LastTxt != "never" {
		t.Fatalf("pi row: %+v", piRow)
	}

	group, ok := byTitle["OpenCode · laptop"]
	if !ok {
		t.Fatalf("missing disambiguated laptop group, titles: %v", titles(roster))
	}
	if group.ID != liveNew.ID || group.Activity.State != store.ActivityIdle {
		t.Fatalf("identity checkbox should invite the freshest live role, got %+v", group)
	}
	if group.Tip != "OpenCode · laptop — Invites Ship the roster (role-new)" || group.Invite != "invites Ship the roster" {
		t.Fatalf("tip: %q invite: %q", group.Tip, group.Invite)
	}
	if group.Kick {
		t.Fatal("a group parent is not itself kickable")
	}
	if group.LiveLabel != "2 live sessions" || group.EarlierLabel != "2 earlier sessions" {
		t.Fatalf("labels: live %q earlier %q", group.LiveLabel, group.EarlierLabel)
	}
	if sessionIDs(group.Live) != "role-new,role-old-live" {
		t.Fatalf("live order: %s", sessionIDs(group.Live))
	}
	if !group.Live[0].Default || group.Live[1].Default {
		t.Fatalf("default flag: %+v", group.Live)
	}
	// Stalled is not "live" — it is a quiet owner, collapsed with history.
	if sessionIDs(group.Earlier) != "role-stalled,role-stale" {
		t.Fatalf("earlier order: %s", sessionIDs(group.Earlier))
	}
	if group.Kind != "opencode" || group.Hostname != "laptop" || group.Workdir != "/work/app" {
		t.Fatalf("group coordinates: %+v", group)
	}
	wantSeen := time.Unix(2_000, 0).UTC().Format("15:04:05")
	if group.LastTxt != wantSeen {
		t.Fatalf("seen: got %q want %q", group.LastTxt, wantSeen)
	}

	other, ok := byTitle["OpenCode · otherbox"]
	if !ok {
		t.Fatalf("missing other install, titles: %v", titles(roster))
	}
	if other.ID != "other-a" || other.LiveLabel != "" || other.EarlierLabel != "2 sessions" {
		t.Fatalf("all-offline group: %+v", other)
	}
	if len(other.Live) != 0 || !other.Earlier[0].Default {
		t.Fatalf("offline default: %+v", other.Earlier)
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

	// When the app-level agent is actually listable it is the invite, even
	// next to a newer live role. ListAgents does not return it in that
	// situation; this locks the rule the picker falls back from.
	client := agent("client", "client", "opencode", "h", "/w", "", 10)
	role := agent("role", "Live role", "opencode", "h", "/w", "ses", 99)
	roster = groupRoster([]store.Agent{role, client}, map[string]store.ActivityView{
		"client": {State: store.ActivityOffline},
		"role":   {State: store.ActivityWorking},
	}, map[string]string{"opencode": "OpenCode"})
	if roster[0].ID != "client" || roster[0].Title != "OpenCode" {
		t.Fatalf("listable client should be the invite: %+v", roster[0])
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
		t.Fatalf("got %d groups", len(roster))
	}
	got := map[string]string{}
	for _, idn := range roster {
		got[idn.Title] = idn.ID
	}
	if got["OpenCode · laptop · alpha"] != "b" || got["OpenCode · laptop · beta"] != "d" {
		t.Fatalf("workdir disambiguation: %v", got)
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
	if !opencode.ForceOpen || len(opencode.Earlier) != 1 || opencode.Earlier[0].ID != "role-parser" {
		t.Fatalf("session match should open just that session: %+v", opencode)
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

func sessionIDs(sessions []rosterSession) string {
	out := ""
	for i, s := range sessions {
		if i > 0 {
			out += ","
		}
		out += s.ID
	}
	return out
}
