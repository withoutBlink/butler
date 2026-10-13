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
func registerInstance(t *testing.T, backendURL, kindToken, hostname, workdir string) *mcpserver.Client {
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
	return client
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
	if reg1.Name != "codex-myhost-proj" || reg1.Kind != "codex" || reg1.Hostname != "myhost" || reg1.Workdir != "/tmp/proj" {
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
	alpha, alphaName := connectMCPClient(t, backend.URL, kinds["codex"], "alpha", "/tmp/proj")
	if alphaName != "codex-alpha-proj" {
		t.Fatalf("unexpected agent name %q", alphaName)
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

	beta := registerInstance(t, backend.URL, kinds["opencode"], "beta", "/tmp/proj")
	if _, err := beta.JoinSession(ctx, sid); err != nil {
		t.Fatalf("beta join: %v", err)
	}
	if _, err := beta.PostMessage(ctx, sid, "hi alpha, beta here"); err != nil {
		t.Fatalf("beta post: %v", err)
	}

	out, isErr = callTool(t, alpha, "wait_for_messages", map[string]any{"timeout_seconds": 5})
	if isErr || !strings.Contains(out, "opencode-beta-proj: hi alpha, beta here") {
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
		"codex-alpha-proj (codex@alpha)*",
		"opencode-beta-proj (opencode@beta)*",
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
	if isErr || !strings.Contains(out, "e2e test") || !strings.Contains(out, "you are codex-alpha-proj") {
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
