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
	tokens := map[string]string{}
	for _, id := range []string{"alpha", "beta"} {
		tok := auth.NewToken()
		if _, err := st.CreateAgent(context.Background(), id, id, "test", auth.Hash(tok)); err != nil {
			t.Fatalf("create agent %s: %v", id, err)
		}
		tokens[id] = tok
	}
	srv := httptest.NewServer(a.Routes())
	t.Cleanup(srv.Close)
	return srv, "test-admin-token", tokens
}

func connectMCPClient(t *testing.T, backendURL, token string) *mcp.ClientSession {
	t.Helper()
	client, err := mcpserver.NewClient(backendURL, token)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "butler", Version: "test"}, nil)
	mcpserver.RegisterTools(server, mcpserver.New(client))
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
	return session
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

func TestToolE2E(t *testing.T) {
	backend, _, tokens := newButlerBackend(t)
	alpha := connectMCPClient(t, backend.URL, tokens["alpha"])

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

	betaClient, err := mcpserver.NewClient(backend.URL, tokens["beta"])
	if err != nil {
		t.Fatalf("beta client: %v", err)
	}
	if _, err := betaClient.JoinSession(ctx, sid); err != nil {
		t.Fatalf("beta join: %v", err)
	}
	if _, err := betaClient.PostMessage(ctx, sid, "hi alpha, beta here"); err != nil {
		t.Fatalf("beta post: %v", err)
	}

	out, isErr = callTool(t, alpha, "wait_for_messages", map[string]any{"timeout_seconds": 5})
	if isErr || !strings.Contains(out, "beta: hi alpha, beta here") {
		t.Fatalf("wait after activity: %q isErr=%v", out, isErr)
	}
	if !strings.Contains(out, "system") {
		t.Fatalf("expected join system message in %q", out)
	}

	out, isErr = callTool(t, alpha, "read_session", map[string]any{"session": sid})
	if isErr {
		t.Fatalf("read_session: %s", out)
	}
	for _, want := range []string{"e2e test", "alpha*", "beta*", "hello from alpha", "hi alpha, beta here"} {
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
	if isErr || !strings.Contains(out, "e2e test") {
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
