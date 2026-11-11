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
	if _, err := st.CreateAgent(ctx, "codex-1", "Codex", "codex", "laptop", "/work/codex", ""); err != nil {
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

	// Teammates, not one chip per session: admin is omitted, the two
	// OpenCode installs are groups, Codex and Pi stay single chips.
	if got := strings.Count(form, `class="roster-item`); got != 4 {
		t.Fatalf("picker identities: got %d want 4\n%s", got, form)
	}
	if got := strings.Count(form, `class="roster-item grouped"`); got != 2 {
		t.Fatalf("grouped identities: got %d want 2\n%s", got, form)
	}
	for _, want := range []string{
		`value="codex-1"`, `value="pi-1"`,
		`>Codex</span>`, `>Pi</span>`,
		`>OpenCode · laptop</span>`, `>OpenCode · otherbox</span>`,
		`Invites Ship the roster (role-new)`,
		`>invites Ship the roster</span>`,
		`2 live sessions`, `3 earlier sessions`, `2 sessions`,
		`value="role-new"`, `value="role-older-live"`, `value="role-ancient"`,
		`value="aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"`,
		`class="live" open`, `class="earlier"`,
	} {
		if !strings.Contains(form, want) {
			t.Fatalf("picker missing %q\n%s", want, form)
		}
	}
	if strings.Contains(form, `class="earlier" open`) {
		t.Fatalf("historical sessions should stay collapsed:\n%s", form)
	}
	// The identity checkbox and the nested default session both post role-new.
	// No other laptop role is that value, and the hidden client is not a value.
	if got := strings.Count(form, `value="role-new"`); got != 2 {
		t.Fatalf("role-new values: got %d want 2 (identity + its session)", got)
	}
	if strings.Contains(sectionOf(form, "Codex"), "<details") {
		t.Fatalf("single-identity chip should not nest sessions:\n%s", sectionOf(form, "Codex"))
	}
	if cls := detailsAround(form, ">Ancient notes</span>"); !strings.Contains(cls, `class="earlier"`) || strings.Contains(cls, "open") {
		t.Fatalf("ancient session details: %q", cls)
	}
	if cls := detailsAround(form, ">Ship the roster</span>"); !strings.Contains(cls, `class="live"`) || !strings.Contains(cls, "open") {
		t.Fatalf("live session details: %q", cls)
	}
	if cls := detailsAround(form, ">Other old</span>"); !strings.Contains(cls, `class="earlier"`) {
		t.Fatalf("other install should be collapsed history: %q", cls)
	}

	// One parent row per identity, kicks on the roles (including historical).
	if got := strings.Count(agents, "<tr>"); got != 6 {
		t.Fatalf("agents rows (header + 5 identities): got %d\n%s", got, agents)
	}
	for _, id := range []string{"codex-1", "pi-1", "role-new", "role-older-live", "role-ancient", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "role-history", "other-recent", "other-old"} {
		if !strings.Contains(agents, `action="/panel/agents/`+id+`/revoke"`) {
			t.Fatalf("agents table missing kick for %s\n%s", id, agents)
		}
	}
	if strings.Count(body, "/revoke") != 9 {
		t.Fatalf("revoke count: got %d want 9 (admin is not kickable)", strings.Count(body, "/revoke"))
	}
	if strings.Count(body, `id="kick-role-new"`) != 1 {
		t.Fatalf("group parent must not duplicate the default role's kick")
	}
	// The uuid is available on the role, not as a sub-line under the teammate.
	if !strings.Contains(agents, `title="Fix read marks (aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee)"`) {
		t.Fatalf("session id should stay available as a tooltip")
	}
	laptopRow := tableRow(agents, "OpenCode · laptop")
	if !strings.Contains(laptopRow, `p-idle`) || strings.Contains(laptopRow[:strings.Index(laptopRow, "OpenCode · laptop")], "p-offline") {
		t.Fatalf("group activity should be the live role's, got:\n%s", laptopRow)
	}

	// Identity checked, no specific extra session: invite only the default role.
	// Not the hidden client, and not every historical sibling.
	sid := createSession(t, client, srv.URL, "invite the teammate", "role-new")
	assertParticipants(t, st, sid, "admin", "role-new")
	assertNotParticipants(t, st, sid, hidden, "client-other", "role-older-live", "role-ancient", "role-history", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "other-recent", "other-old", "codex-1", "pi-1")
	assertJoinNotices(t, st, sid, 1, "Ship the roster")

	// A specific historical session, and not the identity: only that role.
	sid = createSession(t, client, srv.URL, "a particular session", "role-ancient")
	assertParticipants(t, st, sid, "admin", "role-ancient")
	assertNotParticipants(t, st, sid, "role-new", hidden)
	assertJoinNotices(t, st, sid, 1, "Ancient notes")

	// Identity plus another session invites both and drops nothing.
	sid = createSession(t, client, srv.URL, "teammate and a session", "role-new", "role-history")
	assertParticipants(t, st, sid, "admin", "role-new", "role-history")
	assertNotParticipants(t, st, sid, hidden, "role-ancient", "role-older-live")
	assertJoinNotices(t, st, sid, 2, "Ship the roster", "Port the parser")

	// Kick still addresses the role, including one that was collapsed.
	resp, err := client.PostForm(srv.URL+"/panel/agents/role-ancient/revoke", nil)
	if err != nil {
		t.Fatalf("kick: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("kick: %d", resp.StatusCode)
	}
	body = getDash(t, client, srv.URL)
	if strings.Contains(body, `value="role-ancient"`) || strings.Contains(body, `action="/panel/agents/role-ancient/revoke"`) {
		t.Fatalf("kicked role still listed:\n%s", body)
	}
	for _, want := range []string{`value="role-new"`, "Ship the roster", "Fix read marks", "OpenCode · laptop", `value="codex-1"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("after kick, dashboard missing %q", want)
		}
	}
	// One historical role gone, the install is still a group.
	if !strings.Contains(body, "2 earlier sessions") {
		t.Fatalf("group should keep the remaining history collapsed:\n%s", body)
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
