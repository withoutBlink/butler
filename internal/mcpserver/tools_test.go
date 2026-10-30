package mcpserver_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"butler/internal/api"
	"butler/internal/auth"
	"butler/internal/hub"
	"butler/internal/mcpserver"
	"butler/internal/metrics"
	"butler/internal/store"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// isUUID reports whether s is a canonical version-4 UUID — what every agent
// identity is now (the id is the identity, the display name is a label).
func isUUID(s string) bool { return uuidRe.MatchString(s) }

// newButlerBackend boots the API with codex/opencode kinds; the returned map
// holds the single kind token of each.
func newButlerBackend(t *testing.T) (*httptest.Server, string, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "butler.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		st.Close()
	})
	a := &api.API{
		Store:      st,
		Hub:        hub.New(),
		AdminToken: "test-admin-token",
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:    &metrics.Metrics{},
		Done:       done,
	}
	kinds := map[string]string{}
	for _, id := range []string{"codex", "opencode"} {
		tok := auth.NewToken()
		if _, err := st.CreateKind(context.Background(), id, id, auth.Hash(tok)); err != nil {
			t.Fatalf("create kind %s: %v", id, err)
		}
		kinds[id] = tok
	}
	srv := httptest.NewServer(a.Routes())
	t.Cleanup(srv.Close)
	return srv, "test-admin-token", kinds
}

// registerInstance exchanges a kind token for an instance token on a fresh
// client, exactly like cmd/butler-mcp does at startup.
func registerInstance(t *testing.T, backendURL, kindToken, hostname, workdir string) (*mcpserver.Client, string) {
	t.Helper()
	client, err := mcpserver.NewClient(backendURL, kindToken)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	reg, err := client.Register(context.Background(), hostname, workdir)
	if err != nil {
		t.Fatalf("register %s: %v", hostname, err)
	}
	client.SetToken(reg.Token)
	return client, reg.Name
}

// connectMCPClient registers an instance and returns a connected MCP session
// plus the server-generated agent name.
func connectMCPClient(t *testing.T, backendURL, kindToken, hostname, workdir string) (*mcp.ClientSession, string) {
	t.Helper()
	client, err := mcpserver.NewClient(backendURL, kindToken)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	reg, err := client.Register(context.Background(), hostname, workdir)
	if err != nil {
		t.Fatalf("register %s: %v", hostname, err)
	}
	client.SetToken(reg.Token)
	server := mcp.NewServer(&mcp.Implementation{Name: "butler", Version: "test"}, nil)
	mcpserver.RegisterTools(server, mcpserver.New(client, reg.Name))
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	go func() {
		_ = server.Run(context.Background(), serverTransport)
	}()
	session, err := mcpClient.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session, reg.Name
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func TestClientRegister(t *testing.T) {
	backend, _, kinds := newButlerBackend(t)
	ctx := context.Background()

	c1, err := mcpserver.NewClient(backend.URL, kinds["codex"])
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	reg1, err := c1.Register(ctx, "myhost", "/tmp/proj")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	c1.SetToken(reg1.Token)
	if !isUUID(reg1.Name) || reg1.Kind != "codex" || reg1.Hostname != "myhost" || reg1.Workdir != "/tmp/proj" {
		t.Fatalf("bad register result: %+v", reg1)
	}
	if reg1.Replaced {
		t.Fatal("first registration should not replace anything")
	}

	// Same registration key: same name, a fresh token — and the previous
	// instance is replaced (its token stops authenticating).
	c2, err := mcpserver.NewClient(backend.URL, kinds["codex"])
	if err != nil {
		t.Fatalf("new client 2: %v", err)
	}
	reg2, err := c2.Register(ctx, "myhost", "/tmp/proj")
	if err != nil {
		t.Fatalf("register 2: %v", err)
	}
	c2.SetToken(reg2.Token)
	if reg2.Name != reg1.Name {
		t.Fatalf("name should be stable: %q vs %q", reg2.Name, reg1.Name)
	}
	if reg2.Token == reg1.Token {
		t.Fatal("each registration should mint a fresh instance token")
	}
	if !reg2.Replaced {
		t.Fatal("re-registration should replace the previous instance")
	}
	_, err = c1.Sessions(ctx, false)
	if err == nil {
		t.Fatal("replaced instance token should be revoked")
	}
	var apiErr *mcpserver.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Fatalf("expected 401 for replaced token, got %v", err)
	}
	if _, err := c2.Sessions(ctx, false); err != nil {
		t.Fatalf("new instance token should authenticate: %v", err)
	}

	// A different workdir is a different identity.
	c3, err := mcpserver.NewClient(backend.URL, kinds["codex"])
	if err != nil {
		t.Fatalf("new client 3: %v", err)
	}
	reg3, err := c3.Register(ctx, "myhost", "/tmp/other")
	if err != nil {
		t.Fatalf("register 3: %v", err)
	}
	if reg3.Name == reg1.Name {
		t.Fatalf("different workdir should be a different agent: %q", reg3.Name)
	}

	// Bad kind token: 401.
	c4, err := mcpserver.NewClient(backend.URL, "bogus-token")
	if err != nil {
		t.Fatalf("new client 4: %v", err)
	}
	_, err = c4.Register(ctx, "h", "/w")
	if err == nil {
		t.Fatal("expected error for bad kind token")
	}
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Fatalf("expected 401 Error, got %v", err)
	}
}

func TestToolE2E(t *testing.T) {
	backend, _, kinds := newButlerBackend(t)
	alpha, alphaID := connectMCPClient(t, backend.URL, kinds["codex"], "alpha", "/tmp/proj")
	if !isUUID(alphaID) {
		t.Fatalf("unexpected agent id %q", alphaID)
	}

	ctx := context.Background()

	toolsRes, err := alpha.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	want := map[string]bool{
		"list_sessions": false, "create_session": false, "join_session": false,
		"read_session": false, "send_message": false, "wait_for_messages": false,
		"check_messages": false, "get_context": false, "set_context": false,
		"list_tasks": false, "get_task": false, "create_task": false,
		"update_task_status": false, "comment_task": false, "claim_task": false,
		"handoff_task": false, "approve_task": false, "reject_task": false,
		"rename_self": false,
	}
	if len(toolsRes.Tools) != len(want) {
		t.Fatalf("expected %d tools, got %d", len(want), len(toolsRes.Tools))
	}
	for _, tool := range toolsRes.Tools {
		if _, ok := want[tool.Name]; !ok {
			t.Fatalf("unexpected tool %q", tool.Name)
		}
		want[tool.Name] = true
		if tool.Description == "" {
			t.Fatalf("tool %q missing description", tool.Name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("missing tool %q", name)
		}
	}

	out, isErr := callTool(t, alpha, "create_session", map[string]any{"title": "e2e test"})
	if isErr {
		t.Fatalf("create_session: %s", out)
	}
	re := regexp.MustCompile(`\(([0-9a-f]{32})\)`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no session id in %q", out)
	}
	sid := m[1]

	out, isErr = callTool(t, alpha, "send_message", map[string]any{"session": sid, "content": "hello from alpha"})
	if isErr || !strings.Contains(out, "seq=1") {
		t.Fatalf("send_message: %q isErr=%v", out, isErr)
	}

	start := time.Now()
	out, isErr = callTool(t, alpha, "wait_for_messages", map[string]any{"timeout_seconds": 1})
	if isErr || !strings.Contains(out, "no new messages") {
		t.Fatalf("wait before activity: %q isErr=%v", out, isErr)
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatalf("wait returned too fast: %v", time.Since(start))
	}

	beta, betaID := registerInstance(t, backend.URL, kinds["opencode"], "beta", "/tmp/proj")
	if _, err := beta.JoinSession(ctx, sid); err != nil {
		t.Fatalf("beta join: %v", err)
	}
	if _, err := beta.PostMessage(ctx, sid, "hi alpha, beta here"); err != nil {
		t.Fatalf("beta post: %v", err)
	}

	out, isErr = callTool(t, alpha, "wait_for_messages", map[string]any{"timeout_seconds": 5})
	if isErr || !strings.Contains(out, betaID+": hi alpha, beta here") {
		t.Fatalf("wait after activity: %q isErr=%v", out, isErr)
	}
	if !strings.Contains(out, "system") {
		t.Fatalf("expected join system message in %q", out)
	}

	out, isErr = callTool(t, alpha, "read_session", map[string]any{"session": sid})
	if isErr {
		t.Fatalf("read_session: %s", out)
	}
	for _, want := range []string{
		"e2e test",
		alphaID + " (codex@alpha)*",
		betaID + " (opencode@beta)*",
		"hello from alpha",
		"hi alpha, beta here",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("read_session missing %q in:\n%s", want, out)
		}
	}

	out, isErr = callTool(t, alpha, "set_context", map[string]any{"session": sid, "key": "decision", "value": "use sqlite"})
	if isErr || !strings.Contains(out, "decision") {
		t.Fatalf("set_context: %q isErr=%v", out, isErr)
	}
	out, isErr = callTool(t, alpha, "get_context", map[string]any{"session": sid})
	if isErr || !strings.Contains(out, "decision = use sqlite") {
		t.Fatalf("get_context: %q isErr=%v", out, isErr)
	}

	out, isErr = callTool(t, alpha, "check_messages", nil)
	if isErr || !strings.Contains(out, "no new messages") {
		t.Fatalf("check_messages: %q isErr=%v", out, isErr)
	}

	out, isErr = callTool(t, alpha, "list_sessions", nil)
	if isErr || !strings.Contains(out, "e2e test") || !strings.Contains(out, "you are "+alphaID) {
		t.Fatalf("list_sessions: %q isErr=%v", out, isErr)
	}

	out, isErr = callTool(t, alpha, "read_session", map[string]any{"session": "deadbeef"})
	_ = out
	if !isErr {
		t.Fatal("read_session on unknown session should error")
	}

	out, isErr = callTool(t, alpha, "wait_for_messages", map[string]any{"timeout_seconds": 1})
	if isErr {
		t.Fatalf("final wait: %s", out)
	}
}

func TestClientErrorShape(t *testing.T) {
	backend, _, _ := newButlerBackend(t)
	client, err := mcpserver.NewClient(backend.URL, "bogus-token")
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = client.Sessions(context.Background(), false)
	if err == nil {
		t.Fatal("expected error for bad token")
	}
	var apiErr *mcpserver.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Fatalf("expected 401 Error, got %v", err)
	}
}

// newButlerBackendStore is newButlerBackend but also returns the store, for
// tests that bootstrap state the way `butler serve` does.
func newButlerBackendStore(t *testing.T) (*httptest.Server, *store.Store, string, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "butler.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		st.Close()
	})
	a := &api.API{
		Store:      st,
		Hub:        hub.New(),
		AdminToken: "test-admin-token",
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:    &metrics.Metrics{},
		Done:       done,
	}
	kinds := map[string]string{}
	for _, id := range []string{"codex", "opencode"} {
		tok := auth.NewToken()
		if _, err := st.CreateKind(context.Background(), id, id, auth.Hash(tok)); err != nil {
			t.Fatalf("create kind %s: %v", id, err)
		}
		kinds[id] = tok
	}
	srv := httptest.NewServer(a.Routes())
	t.Cleanup(srv.Close)
	return srv, st, "test-admin-token", kinds
}

func TestWaitMentionFormatting(t *testing.T) {
	backend, st, _, kinds := newButlerBackendStore(t)
	ctx := context.Background()

	// Bootstrap "admin" + the global session the way `butler serve` does:
	// every agent that registers joins it automatically.
	if _, err := st.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatalf("create you: %v", err)
	}
	if _, err := st.EnsureGlobalSession(ctx, "admin"); err != nil {
		t.Fatalf("ensure global: %v", err)
	}

	alpha, alphaID := connectMCPClient(t, backend.URL, kinds["codex"], "alpha", "/tmp/proj")
	beta, _ := connectMCPClient(t, backend.URL, kinds["opencode"], "beta", "/tmp/proj")

	// Registration auto-joined both agents: the global session shows up.
	out, isErr := callTool(t, alpha, "list_sessions", nil)
	if isErr || !strings.Contains(out, "global") {
		t.Fatalf("list_sessions should include the global session: %q", out)
	}

	// Initialize alpha's cursor before beta posts.
	out, isErr = callTool(t, alpha, "wait_for_messages", map[string]any{"timeout_seconds": 1})
	if isErr || !strings.Contains(out, "no new messages") {
		t.Fatalf("wait before activity: %q isErr=%v", out, isErr)
	}

	// beta posts a mention and an FYI message in the global session.
	out, isErr = callTool(t, beta, "send_message", map[string]any{"session": store.GlobalSessionID, "content": "@" + alphaID + " please review the diff"})
	if isErr {
		t.Fatalf("beta send mention: %s", out)
	}
	out, isErr = callTool(t, beta, "send_message", map[string]any{"session": store.GlobalSessionID, "content": "heads up: build is green"})
	if isErr {
		t.Fatalf("beta send fyi: %s", out)
	}

	out, isErr = callTool(t, alpha, "wait_for_messages", map[string]any{"timeout_seconds": 3})
	if isErr {
		t.Fatalf("wait: %s", out)
	}
	for _, want := range []string{
		"2 new message(s)",
		"1 @-mention you (reply expected)",
		"1 are FYI",
		"(mentioned you): @" + alphaID + " please review the diff",
		"heads up: build is green",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("wait output missing %q in:\n%s", want, out)
		}
	}
	// The FYI line must NOT carry the marker.
	if strings.Contains(out, "(mentioned you): heads up") {
		t.Fatalf("FYI message should not be marked as a mention:\n%s", out)
	}
	// Session grouping shows the title so the agent knows where to reply.
	if !strings.Contains(out, "— session \"Global\" (global):") {
		t.Fatalf("wait output should group by session with title:\n%s", out)
	}

	// read_session history also marks messages that mention the reader.
	out, isErr = callTool(t, alpha, "read_session", map[string]any{"session": store.GlobalSessionID})
	if isErr || !strings.Contains(out, "(mentioned you)") {
		t.Fatalf("read_session should mark the mention: %q isErr=%v", out, isErr)
	}
}

func TestTaskTools(t *testing.T) {
	backend, st, _, kinds := newButlerBackendStore(t)
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatalf("admin: %v", err)
	}
	if _, err := st.EnsureGlobalSession(ctx, "admin"); err != nil {
		t.Fatalf("global: %v", err)
	}
	alpha, alphaID := connectMCPClient(t, backend.URL, kinds["codex"], "alpha", "/tmp/proj")
	beta, betaID := connectMCPClient(t, backend.URL, kinds["opencode"], "beta", "/tmp/proj")

	// Park both cursors at "now" so the assignment is the next thing beta sees.
	for _, who := range []struct {
		name string
		s    *mcp.ClientSession
	}{{"alpha", alpha}, {"beta", beta}} {
		out, isErr := callTool(t, who.s, "wait_for_messages", map[string]any{"timeout_seconds": 1})
		if isErr || !strings.Contains(out, "no new messages") {
			t.Fatalf("%s prime wait: %q isErr=%v", who.name, out, isErr)
		}
	}

	out, isErr := callTool(t, alpha, "create_task", map[string]any{
		"title": "Review the diff", "description": "Look at the board patch",
		"assignee": betaID, "priority": "high",
	})
	if isErr {
		t.Fatalf("create_task: %s", out)
	}
	m := regexp.MustCompile(`\(([0-9a-f-]{36})\)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no task id in %q", out)
	}
	taskID := m[1]
	if !strings.Contains(out, "Review the diff") || !strings.Contains(out, betaID) {
		t.Fatalf("create output: %s", out)
	}

	out, isErr = callTool(t, beta, "wait_for_messages", map[string]any{"timeout_seconds": 3})
	if isErr || !strings.Contains(out, "(mentioned you)") || !strings.Contains(out, "Review the diff") || !strings.Contains(out, taskID) {
		t.Fatalf("assignee was not woken: %q isErr=%v", out, isErr)
	}

	out, isErr = callTool(t, beta, "list_tasks", map[string]any{"assignee": "me", "status": "todo"})
	if isErr || !strings.Contains(out, taskID) || !strings.Contains(out, "Review the diff") {
		t.Fatalf("list_tasks: %q isErr=%v", out, isErr)
	}
	out, isErr = callTool(t, beta, "get_task", map[string]any{"task": taskID})
	if isErr || !strings.Contains(out, "Look at the board patch") || !strings.Contains(out, "no comments") {
		t.Fatalf("get_task: %q isErr=%v", out, isErr)
	}
	out, isErr = callTool(t, beta, "claim_task", map[string]any{"task": taskID})
	if isErr || !strings.Contains(out, "in_progress") || !strings.Contains(out, betaID) {
		t.Fatalf("claim_task: %q isErr=%v", out, isErr)
	}
	out, isErr = callTool(t, beta, "comment_task", map[string]any{"task": taskID, "body": "reading it now"})
	if isErr || !strings.Contains(out, "comment recorded") {
		t.Fatalf("comment_task: %q isErr=%v", out, isErr)
	}
	out, isErr = callTool(t, beta, "update_task_status", map[string]any{"task": taskID, "status": "review"})
	if isErr || !strings.Contains(out, "review") {
		t.Fatalf("update_task_status: %q isErr=%v", out, isErr)
	}

	// The creator (alpha) is told about the claim and the comment.
	out, isErr = callTool(t, alpha, "wait_for_messages", map[string]any{"timeout_seconds": 3})
	if isErr || !strings.Contains(out, "(mentioned you)") || !strings.Contains(out, taskID) {
		t.Fatalf("creator was not notified: %q isErr=%v", out, isErr)
	}
	out, isErr = callTool(t, alpha, "get_task", map[string]any{"task": taskID})
	if isErr || !strings.Contains(out, "reading it now") || !strings.Contains(out, "review") {
		t.Fatalf("get after updates: %q isErr=%v", out, isErr)
	}
	if !strings.Contains(out, alphaID) {
		t.Fatalf("get should name the creator: %s", out)
	}
}

func TestHandoffTools(t *testing.T) {
	backend, st, _, kinds := newButlerBackendStore(t)
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatalf("admin: %v", err)
	}
	if _, err := st.EnsureGlobalSession(ctx, "admin"); err != nil {
		t.Fatalf("global: %v", err)
	}
	alpha, alphaID := connectMCPClient(t, backend.URL, kinds["codex"], "alpha", "/tmp/proj")
	beta, betaID := connectMCPClient(t, backend.URL, kinds["opencode"], "beta", "/tmp/proj")
	reviewer, reviewerID := connectMCPClient(t, backend.URL, kinds["codex"], "reviewer", "/tmp/review")

	out, isErr := callTool(t, alpha, "create_task", map[string]any{
		"title": "Pass it along", "assignee": alphaID,
		"chain": []any{alphaID, betaID}, "reviewer": reviewerID,
	})
	if isErr {
		t.Fatalf("create_task: %s", out)
	}
	m := regexp.MustCompile(`\(([0-9a-f-]{36})\)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no task id in %q", out)
	}
	taskID := m[1]

	out, isErr = callTool(t, alpha, "handoff_task", map[string]any{"task": taskID, "note": "your turn"})
	if isErr || !strings.Contains(out, "todo") || !strings.Contains(out, betaID) {
		t.Fatalf("handoff: %q isErr=%v", out, isErr)
	}
	out, isErr = callTool(t, alpha, "approve_task", map[string]any{"task": taskID})
	if !isErr || !strings.Contains(out, "reviewer") {
		t.Fatalf("owner approve should fail: %q isErr=%v", out, isErr)
	}
	out, isErr = callTool(t, beta, "handoff_task", map[string]any{"task": taskID})
	if isErr || !strings.Contains(out, "review") || !strings.Contains(out, reviewerID) {
		t.Fatalf("handoff to review: %q isErr=%v", out, isErr)
	}
	out, isErr = callTool(t, reviewer, "reject_task", map[string]any{"task": taskID, "note": "one more pass"})
	if isErr || !strings.Contains(out, "todo") || !strings.Contains(out, betaID) {
		t.Fatalf("reject: %q isErr=%v", out, isErr)
	}
	out, isErr = callTool(t, beta, "handoff_task", map[string]any{"task": taskID})
	if isErr || !strings.Contains(out, "review") {
		t.Fatalf("handoff to review again: %q isErr=%v", out, isErr)
	}
	out, isErr = callTool(t, reviewer, "approve_task", map[string]any{"task": taskID})
	if isErr || !strings.Contains(out, "done") {
		t.Fatalf("approve: %q isErr=%v", out, isErr)
	}
}

// TestRenameSelfTool: the rename_self tool renames the calling agent only,
// keeps its id, posts the "Old is now New" notice in global, and surfaces a
// taken name as a 409 tool error. The new name then works as an assignee.
func TestRenameSelfTool(t *testing.T) {
	backend, st, _, kinds := newButlerBackendStore(t)
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatalf("admin: %v", err)
	}
	if _, err := st.EnsureGlobalSession(ctx, "admin"); err != nil {
		t.Fatalf("global: %v", err)
	}
	alpha, alphaID := connectMCPClient(t, backend.URL, kinds["codex"], "alpha", "/tmp/proj")
	beta, betaID := connectMCPClient(t, backend.URL, kinds["opencode"], "beta", "/tmp/proj")

	out, isErr := callTool(t, alpha, "rename_self", map[string]any{"display_name": "Point"})
	if isErr || !strings.Contains(out, `"Point"`) || !strings.Contains(out, alphaID) {
		t.Fatalf("rename_self: %q isErr=%v", out, isErr)
	}
	a, err := st.GetAgent(ctx, alphaID)
	if err != nil || a.DisplayName != "Point" {
		t.Fatalf("stored name: %v %v", a, err)
	}
	if b, _ := st.GetAgent(ctx, betaID); b.DisplayName == "Point" {
		t.Fatalf("rename_self renamed the wrong agent")
	}
	msgs, err := st.SessionLatestMessages(ctx, store.GlobalSessionID, 100)
	if err != nil {
		t.Fatalf("global messages: %v", err)
	}
	found := false
	for _, m := range msgs {
		if m.Kind == "system" && m.Content == alphaID+" is now Point" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no rename notice in global: %+v", msgs)
	}

	out, isErr = callTool(t, alpha, "rename_self", map[string]any{"display_name": "Point"})
	if isErr || !strings.Contains(out, "already") {
		t.Fatalf("same-name rename_self: %q isErr=%v", out, isErr)
	}
	out, isErr = callTool(t, beta, "rename_self", map[string]any{"display_name": "point"})
	if !isErr || !strings.Contains(out, "409") {
		t.Fatalf("taken name should be a 409 tool error: %q isErr=%v", out, isErr)
	}
	out, isErr = callTool(t, beta, "rename_self", map[string]any{"display_name": "  "})
	if !isErr {
		t.Fatalf("blank name should fail: %q", out)
	}
	out, isErr = callTool(t, beta, "create_task", map[string]any{"title": "for Point", "assignee": "Point"})
	if isErr || !strings.Contains(out, alphaID) {
		t.Fatalf("assign by the new name: %q isErr=%v", out, isErr)
	}
}
