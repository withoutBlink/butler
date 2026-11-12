package ui_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"butler/internal/store"
)

// TestDashboardGroupsSiblingRoster is the crowded panel: many OpenCode
// sessions under one install, a second install, and ordinary single-identity
// teammates. The picker and the agents table must show teammates, not every
// historical session, and checking a teammate must invite one real agent.
func TestDashboardGroupsSiblingRoster(t *testing.T) {
	srv, st, _, adminTok := newPanel(t)
	ctx := context.Background()
	if _, err := st.CreateKind(ctx, "opencode", "OpenCode", "kind-hash-opencode"); err != nil {
		t.Fatalf("kind: %v", err)
	}
	if _, err := st.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatalf("admin: %v", err)
	}
	if _, err := st.CreateAgent(ctx, "codex-1", "Codex", "codex", "laptop", "/work/app", ""); err != nil {
		t.Fatalf("codex: %v", err)
	}
	if _, err := st.CreateAgent(ctx, "pi-1", "Pi", "pi", "laptop", "/work/pi", ""); err != nil {
		t.Fatalf("pi: %v", err)
	}
	// Hidden once any live role exists. The dashboard must not offer it.
	const hidden = "client-HIDDEN-app"
	if _, err := st.CreateAgent(ctx, hidden, hidden, "opencode", "laptop", "/work/app", ""); err != nil {
		t.Fatalf("client: %v", err)
	}
	const evicted = "role-EVICTED"
	if _, err := st.CreateAgent(ctx, evicted, "Evicted session", "opencode", "laptop", "/work/app", "ses-evicted"); err != nil {
		t.Fatalf("evicted: %v", err)
	}
	if err := st.RemoveAgent(ctx, evicted); err != nil {
		t.Fatalf("remove: %v", err)
	}

	type role struct {
		id, title, session string
		seen               int64
	}
	now := time.Now().Unix()
	laptop := []role{
		{"role-new", "Ship the roster", "ses-new", now},
		{"role-older-live", "Review the board", "ses-review", now - 40},
		{"role-ancient", "Ancient notes", "ses-ancient", now - 8*3600},
		{"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "Fix read marks", "ses-uuid", now - 9*3600},
		{"role-history", "Port the parser", "ses-parser", now - 10*3600},
	}
	for _, r := range laptop {
		if _, err := st.CreateAgent(ctx, r.id, r.title, "opencode", "laptop", "/work/app", r.session); err != nil {
			t.Fatalf("create %s: %v", r.id, err)
		}
		seen := r.seen
		if err := st.SetContactTimes(ctx, r.id, &seen, nil); err != nil {
			t.Fatalf("seen %s: %v", r.id, err)
		}
	}
	other := []role{
		{"other-recent", "Other recent", "ses-o1", now - 3*3600},
		{"other-old", "Other old", "ses-o2", now - 6*3600},
	}
	if _, err := st.CreateAgent(ctx, "client-other", "client-other", "opencode", "otherbox", "/work/other", ""); err != nil {
		t.Fatalf("other client: %v", err)
	}
	for _, r := range other {
		if _, err := st.CreateAgent(ctx, r.id, r.title, "opencode", "otherbox", "/work/other", r.session); err != nil {
			t.Fatalf("create %s: %v", r.id, err)
		}
		seen := r.seen
		if err := st.SetContactTimes(ctx, r.id, &seen, nil); err != nil {
			t.Fatalf("seen %s: %v", r.id, err)
		}
	}
	seen := now
	for _, id := range []string{"codex-1", "pi-1"} {
		if err := st.SetContactTimes(ctx, id, &seen, nil); err != nil {
			t.Fatalf("seen %s: %v", id, err)
		}
	}

	client := newBrowser(t)
	login(t, client, srv.URL, adminTok)
	body := getDash(t, client, srv.URL)

	for _, banned := range []string{hidden, "client-other", evicted, "Evicted session", `class="sub"`} {
		if strings.Contains(body, banned) {
			t.Fatalf("dashboard should not contain %q", banned)
		}
	}
	form := section(body, `class="new-session-form"`, `</form>`)
	agents := section(body, `<section class="agents">`, `</section>`)

	// One project for /work/app (Codex + OpenCode), Pi alone, the other
	// OpenCode install alone. Historical sessions are not rows.
	if got := strings.Count(form, `class="roster-item`); got != 3 {
		t.Fatalf("picker rows: got %d want 3\n%s", got, form)
	}
	if got := strings.Count(form, `class="roster-item grouped"`); got != 1 {
		t.Fatalf("project groups: got %d want 1\n%s", got, form)
	}
	for _, want := range []string{
		`value="project:/work/app"`,
		`value="codex-1"`, `value="pi-1"`, `value="role-new"`,
		`>app</span>`, `>Codex</span>`, `>OpenCode</span>`, `>Pi</span>`,
		`2 teammates`,
		`Invites Ship the roster (role-new)`,
		`title="/work/app — invites `,
	} {
		if !strings.Contains(form, want) {
			t.Fatalf("picker missing %q\n%s", want, form)
		}
	}
	for _, banned := range []string{
		`value="role-ancient"`, `value="role-older-live"`, `value="role-history"`,
		`value="aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"`,
		`earlier sessions`, `live sessions`,
	} {
		if strings.Contains(form, banned) {
			t.Fatalf("picker should not list historical session %q\n%s", banned, form)
		}
	}
	if strings.Contains(sectionOf(form, "Pi"), "<details") {
		t.Fatalf("a one-agent project should not be a disclosure:\n%s", sectionOf(form, "Pi"))
	}

	// Header + admin + the app project + Pi + the other OpenCode.
	if got := strings.Count(agents, "<tr>"); got != 5 {
		t.Fatalf("agents rows: got %d\n%s", got, agents)
	}
	for _, id := range []string{"codex-1", "pi-1", "role-new", "other-recent"} {
		if !strings.Contains(agents, `action="/panel/agents/`+id+`/revoke"`) {
			t.Fatalf("agents table missing kick for %s\n%s", id, agents)
		}
	}
	if strings.Contains(agents, `action="/panel/agents/role-ancient/revoke"`) {
		t.Fatalf("historical session should not have its own kick row:\n%s", agents)
	}
	if strings.Count(body, "/revoke") != 4 {
		t.Fatalf("revoke count: got %d want 4", strings.Count(body, "/revoke"))
	}
	appRow := tableRow(agents, ">app</span>")
	if !strings.Contains(appRow, "OpenCode") || !strings.Contains(appRow, "Codex") {
		t.Fatalf("project row should list teammates:\n%s", appRow)
	}

	// Checking the project invites each teammate's live role, not history.
	sid := createSession(t, client, srv.URL, "invite the project", "project:/work/app")
	assertParticipants(t, st, sid, "admin", "codex-1", "role-new")
	assertNotParticipants(t, st, sid, hidden, "client-other", "role-older-live", "role-ancient", "role-history", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "pi-1", "other-recent")
	assertJoinNotices(t, st, sid, 2, "Codex", "Ship the roster")

	// Checking one nested teammate invites only that role.
	sid = createSession(t, client, srv.URL, "just opencode", "role-new")
	assertParticipants(t, st, sid, "admin", "role-new")
	assertNotParticipants(t, st, sid, "codex-1", hidden, "role-ancient")

	// Kick the representative role. The project keeps Codex, and OpenCode
	// falls through to its next live session.
	resp, err := client.PostForm(srv.URL+"/panel/agents/role-new/revoke", nil)
	if err != nil {
		t.Fatalf("kick: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("kick: %d", resp.StatusCode)
	}
	body = getDash(t, client, srv.URL)
	if strings.Contains(body, `value="role-new"`) || strings.Contains(body, `action="/panel/agents/role-new/revoke"`) {
		t.Fatalf("kicked role still listed:\n%s", body)
	}
	for _, want := range []string{`value="project:/work/app"`, `value="codex-1"`, `value="role-older-live"`, ">OpenCode</span>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("after kick, dashboard missing %q", want)
		}
	}
}

func getDash(t *testing.T, client *http.Client, base string) string {
	t.Helper()
	resp, err := client.Get(base + "/panel/")
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard: %d", resp.StatusCode)
	}
	return string(body)
}

func createSession(t *testing.T, client *http.Client, base, title string, agents ...string) string {
	t.Helper()
	resp, err := client.PostForm(base+"/panel/sessions", url.Values{"title": {title}, "agents": agents})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create %q: %d", title, resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/panel/sessions/") {
		t.Fatalf("redirect: %q", loc)
	}
	return strings.TrimPrefix(loc, "/panel/sessions/")
}

func assertParticipants(t *testing.T, st *store.Store, sid string, want ...string) {
	t.Helper()
	parts, err := st.Participants(ctxBackground(), sid)
	if err != nil {
		t.Fatalf("participants: %v", err)
	}
	got := map[string]bool{}
	for _, p := range parts {
		got[p.ID] = true
	}
	if len(got) != len(want) {
		t.Fatalf("session %s participants: got %v want %v", sid, got, want)
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("session %s missing %s in %v", sid, id, got)
		}
	}
}

func assertNotParticipants(t *testing.T, st *store.Store, sid string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		ok, err := st.IsParticipant(ctxBackground(), sid, id)
		if err != nil || ok {
			t.Fatalf("session %s should not include %s: %v %v", sid, id, ok, err)
		}
	}
}

func assertJoinNotices(t *testing.T, st *store.Store, sid string, n int, names ...string) {
	t.Helper()
	msgs, err := st.SessionLatestMessages(ctxBackground(), sid, 20)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	joins := 0
	for _, m := range msgs {
		if m.Kind == "system" && strings.Contains(m.Content, " joined the session") {
			joins++
		}
	}
	if joins != n {
		t.Fatalf("session %s join notices: got %d want %d: %+v", sid, joins, n, msgs)
	}
	for _, name := range names {
		found := false
		for _, m := range msgs {
			if m.Kind == "system" && m.Content == name+" joined the session" {
				found = true
			}
		}
		if !found {
			t.Fatalf("session %s missing join notice for %s: %+v", sid, name, msgs)
		}
	}
}

// TestDashboardScalesLongTitles is the crowded panel Andrew actually hits:
// many teammates and very long display titles. The new-session list and the
// agents table stay in scroll regions, labels ellipsize with the full text
// on the tooltip, and the filter narrows the list without dropping a
// checkbox that was already selected.
func TestDashboardScalesLongTitles(t *testing.T) {
	srv, st, _, adminTok := newPanel(t)
	ctx := context.Background()
	const marker = "ZZ-parser-marker"
	long := strings.Repeat("Very long integration title ", 6) + marker
	other := strings.Repeat("Unrelated planning notes ", 6) + "other-tail"
	if _, err := st.CreateAgent(ctx, "long-1", long, "codex", "host-with-a-very-long-hostname.example", "/workspace/agents/extremely/long/working/directory/that/should/not/stretch/the/table", ""); err != nil {
		t.Fatalf("long: %v", err)
	}
	if _, err := st.CreateAgent(ctx, "other-1", other, "pi", "h", "/w", ""); err != nil {
		t.Fatalf("other: %v", err)
	}
	for i := 0; i < 22; i++ {
		id := fmt.Sprintf("bulk-%02d", i)
		title := strings.Repeat("Bulk teammate with a long display name ", 3) + id
		if _, err := st.CreateAgent(ctx, id, title, "codex", "host-"+id, "/work/"+id, ""); err != nil {
			t.Fatalf("bulk %s: %v", id, err)
		}
	}

	client := newBrowser(t)
	login(t, client, srv.URL, adminTok)
	body := getDash(t, client, srv.URL)
	for _, want := range []string{
		`class="picks-scroll"`,
		`class="agents-scroll"`,
		`hx-get="/panel/picks"`,
		`class="clip"`,
		`title="` + long + ` (long-1)"`,
		`title="/workspace/agents/extremely/long/working/directory/that/should/not/stretch/the/table"`,
		`title="host-with-a-very-long-hostname.example"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, `class="sub"`) {
		t.Fatalf("long titles should not grow a UUID sub-line")
	}
	if got := strings.Count(body, `class="roster-item`); got < 24 {
		t.Fatalf("picker rows: got %d want at least 24", got)
	}

	resp, err := client.Get(srv.URL + "/panel/picks?pick=" + url.QueryEscape(marker) + "&agents=other-1")
	if err != nil {
		t.Fatalf("picks: %v", err)
	}
	filtered, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("picks: %d", resp.StatusCode)
	}
	page := string(filtered)
	if !strings.Contains(page, `value="long-1"`) || !strings.Contains(page, long) {
		t.Fatalf("filter missed the long title:\n%s", page)
	}
	if strings.Contains(page, other) || strings.Contains(page, `value="bulk-`) {
		t.Fatalf("filter should hide non-matching teammates:\n%s", page)
	}
	if !strings.Contains(page, `<input type="hidden" name="agents" value="other-1">`) {
		t.Fatalf("selected teammate hidden by the filter must stay in the form:\n%s", page)
	}
	if !strings.Contains(page, "1 of ") {
		t.Fatalf("filter should report how many teammates matched:\n%s", page)
	}

	sid := createSession(t, client, srv.URL, "kept the hidden pick", "other-1")
	assertParticipants(t, st, sid, "admin", "other-1")
}

func TestDashboardCondensesSessionParticipants(t *testing.T) {
	srv, st, _, adminTok := newPanel(t)
	ctx := context.Background()
	now := time.Now().Unix()
	long := func(name string) string {
		return strings.Repeat("Session member with a very long display name ", 3) + name
	}
	type member struct {
		id   string
		seen int64
	}
	members := []member{
		{"live-new", now},
		{"live-old", now - 15},
		{"off-1", now - 2*3600},
		{"off-2", now - 3*3600},
		{"off-3", now - 4*3600},
		{"off-4", now - 5*3600},
		{"off-5", now - 6*3600},
		{"off-6", now - 7*3600},
	}
	for _, m := range members {
		if _, err := st.CreateAgent(ctx, m.id, long(m.id), "codex", "h-"+m.id, "/work/"+m.id, ""); err != nil {
			t.Fatalf("create %s: %v", m.id, err)
		}
		seen := m.seen
		if err := st.SetContactTimes(ctx, m.id, &seen, nil); err != nil {
			t.Fatalf("seen %s: %v", m.id, err)
		}
	}
	if _, err := st.CreateSession(ctx, "crowd", "Crowd", "live-new", nil); err != nil {
		t.Fatalf("session: %v", err)
	}
	for _, m := range members[1:] {
		if _, err := st.JoinSession(ctx, "crowd", m.id); err != nil {
			t.Fatalf("join %s: %v", m.id, err)
		}
	}
	if _, err := st.CreateAgent(ctx, "pair-a", "Pair A", "pi", "h", "/p", ""); err != nil {
		t.Fatalf("pair: %v", err)
	}
	if _, err := st.CreateSession(ctx, "pair", "Pair", "pair-a", []string{"live-new"}); err != nil {
		t.Fatalf("pair session: %v", err)
	}

	client := newBrowser(t)
	login(t, client, srv.URL, adminTok)
	body := getDash(t, client, srv.URL)
	sessions := section(body, `<section class="sessions">`, `</section>`)
	crowd := tableRow(sessions, ">Crowd</a>")
	if crowd == "" {
		t.Fatalf("missing crowd row:\n%s", sessions)
	}
	if !strings.Contains(crowd, `4 more`) || !strings.Contains(crowd, `class="part-more"`) {
		t.Fatalf("crowd row should collapse the tail:\n%s", crowd)
	}
	detailsAt := strings.Index(crowd, `<details class="part-more"`)
	if detailsAt < 0 || strings.Index(crowd, long("live-new")) > detailsAt || strings.Index(crowd, long("live-old")) > detailsAt {
		t.Fatalf("live names should be visible before the disclosure:\n%s", crowd)
	}
	if strings.Index(crowd, long("off-6")) < detailsAt {
		t.Fatalf("oldest name should be inside +N more, not in the visible set:\n%s", crowd)
	}
	if !strings.Contains(crowd, `title="`+long("live-new")+` (live-new)"`) {
		t.Fatalf("full name should be the tooltip:\n%s", crowd)
	}
	pair := tableRow(sessions, ">Pair</a>")
	if strings.Contains(pair, `class="part-more"`) {
		t.Fatalf("a two-person meeting should list everyone:\n%s", pair)
	}
	if !strings.Contains(pair, "Pair A") || !strings.Contains(pair, long("live-new")) {
		t.Fatalf("pair row missing a name:\n%s", pair)
	}
}

func ctxBackground() context.Context { return context.Background() }

func section(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	s = s[i:]
	j := strings.Index(s, end)
	if j < 0 {
		return s
	}
	return s[:j]
}

func sectionOf(form, title string) string {
	parts := strings.Split(form, `class="roster-item`)
	for _, p := range parts[1:] {
		if strings.Contains(p, ">"+title+"</span>") {
			return p
		}
	}
	return ""
}

func detailsAround(s, text string) string {
	idx := strings.Index(s, text)
	if idx < 0 {
		return ""
	}
	pre := s[:idx]
	i := strings.LastIndex(pre, "<details")
	if i < 0 {
		return ""
	}
	j := strings.Index(pre[i:], ">")
	if j < 0 {
		return pre[i:]
	}
	return pre[i : i+j]
}

func tableRow(section, text string) string {
	parts := strings.Split(section, "<tr>")
	for _, p := range parts {
		if strings.Contains(p, text) {
			return p
		}
	}
	return ""
}
