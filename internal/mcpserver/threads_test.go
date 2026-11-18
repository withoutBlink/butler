package mcpserver_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"butler/internal/mcpserver"
	"butler/internal/store"
)

func TestBindCodexCall(t *testing.T) {
	root := "11111111-1111-4111-8111-111111111111"
	sub := "22222222-2222-4222-8222-222222222222"
	cases := []struct {
		thread, session string
		want            mcpserver.CodexBinding
		bound           bool
	}{
		{thread: root, session: root, bound: true, want: mcpserver.CodexBinding{Key: root, DeliverThread: root, SessionID: root}},
		{thread: root, bound: true, want: mcpserver.CodexBinding{Key: root, DeliverThread: root, SessionID: root}},
		{thread: "  " + root + "  ", session: root, bound: true, want: mcpserver.CodexBinding{Key: root, DeliverThread: root, SessionID: root}},
		{thread: sub, session: root, bound: true, want: mcpserver.CodexBinding{Key: root, DeliverThread: root, SessionID: root, SubAgent: true}},
		{thread: "", session: root},
		{thread: "../" + root},
		{thread: "thread id with spaces"},
		{thread: strings.Repeat("a", 129)},
		{thread: ".hidden"},
	}
	for _, tc := range cases {
		got, ok := mcpserver.BindCodexCall(tc.thread, tc.session)
		if ok != tc.bound {
			t.Errorf("BindCodexCall(%q, %q) bound=%v, want %v", tc.thread, tc.session, ok, tc.bound)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("BindCodexCall(%q, %q)=%+v, want %+v", tc.thread, tc.session, got, tc.want)
		}
	}
}

func TestCodexInstructionsMentionStartupCall(t *testing.T) {
	text := mcpserver.CodexInstructions
	for _, phrase := range []string{"list_sessions", "get_context", "sub-agent", "Forking"} {
		if !strings.Contains(text, phrase) {
			t.Fatalf("instructions missing %q:\n%s", phrase, text)
		}
	}
}

func TestCodexThreadMetaRegistersSeparateIdentities(t *testing.T) {
	backend, st, _, kinds := newButlerBackendStore(t)
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatalf("admin: %v", err)
	}
	if _, err := st.EnsureGlobalSession(ctx, "admin"); err != nil {
		t.Fatalf("global: %v", err)
	}

	const (
		host    = "laptop"
		work    = "/tmp/proj-codex"
		threadA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		threadB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
		sub     = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	)
	stateDir := t.TempDir()
	session, appID := connectCodex(t, backend.URL, kinds["codex"], host, work, stateDir)

	// No thread id (Codex before 0.123, or a non-codex client): the startup identity.
	out, isErr := callToolMeta(t, session, "list_sessions", nil, "", "")
	if isErr {
		t.Fatalf("unbound list_sessions: %s", out)
	}
	if got := youAre(t, out); got != appID {
		t.Fatalf("call without threadId should stay the app identity %s, got %s", appID, got)
	}
	if entries, _ := os.ReadDir(stateDir); len(entries) != 0 {
		t.Fatalf("unbound call wrote %d state files", len(entries))
	}

	outA, isErr := callToolMeta(t, session, "list_sessions", nil, threadA, threadA)
	if isErr {
		t.Fatalf("thread A: %s", outA)
	}
	idA := youAre(t, outA)
	outB, isErr := callToolMeta(t, session, "list_sessions", nil, threadB, threadB)
	if isErr {
		t.Fatalf("thread B: %s", outB)
	}
	idB := youAre(t, outB)
	if idA == idB || idA == appID || idB == appID {
		t.Fatalf("threads should be new identities, got A=%s B=%s app=%s", idA, idB, appID)
	}

	// A second call for the same thread reuses the token (does not revoke it).
	stA := readThreadState(t, stateDir, threadA)
	stB := readThreadState(t, stateDir, threadB)
	if stA.ThreadID != threadA || stA.SessionID != threadA || stA.Agent != idA {
		t.Fatalf("thread A state: %+v", stA)
	}
	if stB.ThreadID != threadB || stB.Agent != idB || stB.Token == stA.Token {
		t.Fatalf("thread B state: %+v", stB)
	}
	if info, err := os.Stat(filepath.Join(stateDir, threadA+".json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode: %v %v", info, err)
	}
	outA2, isErr := callToolMeta(t, session, "list_sessions", nil, threadA, threadA)
	if isErr {
		t.Fatalf("thread A again: %s", outA2)
	}
	if got := youAre(t, outA2); got != idA {
		t.Fatalf("resume should keep %s, got %s", idA, got)
	}
	stA2 := readThreadState(t, stateDir, threadA)
	if stA2.Token != stA.Token {
		t.Fatal("second call for the same thread rotated the token")
	}
	assertToken(t, backend.URL, stA.Token)
	assertToken(t, backend.URL, stB.Token)

	// A sub-agent folds into the root thread instead of minting a roster identity.
	outSub, isErr := callToolMeta(t, session, "list_sessions", nil, sub, threadA)
	if isErr {
		t.Fatalf("sub-agent: %s", outSub)
	}
	if got := youAre(t, outSub); got != idA {
		t.Fatalf("sub-agent should act as root %s, got %s", idA, got)
	}
	if _, err := os.Stat(filepath.Join(stateDir, sub+".json")); !os.IsNotExist(err) {
		t.Fatalf("sub-agent wrote its own state file: %v", err)
	}
	if stA3 := readThreadState(t, stateDir, threadA); stA3.Token != stA.Token {
		t.Fatal("folding a sub-agent rotated the root token")
	}

	// A second process registering the app-level identity revokes that token
	// only. The thread tokens stay valid — two Codex sessions in one workdir
	// no longer kick each other off.
	other, err := mcpserver.NewClient(backend.URL, kinds["codex"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Register(ctx, host, work); err != nil {
		t.Fatalf("second app registration: %v", err)
	}
	assertToken(t, backend.URL, stA.Token)
	assertToken(t, backend.URL, stB.Token)

	// Resuming thread A from another MCP server keeps the agent id and
	// replaces A's token, and does not touch thread B.
	resumed, _ := connectCodex(t, backend.URL, kinds["codex"], host, work, stateDir)
	outR, isErr := callToolMeta(t, resumed, "list_sessions", nil, threadA, threadA)
	if isErr {
		t.Fatalf("resume: %s", outR)
	}
	if got := youAre(t, outR); got != idA {
		t.Fatalf("resume agent = %s, want %s", got, idA)
	}
	stA4 := readThreadState(t, stateDir, threadA)
	if stA4.Token == stA.Token {
		t.Fatal("resume should replace the previous thread token")
	}
	if _, err := mcpserver.NewClient(backend.URL, stA.Token); err != nil {
		t.Fatal(err)
	}
	dead, err := mcpserver.NewClient(backend.URL, stA.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dead.Sessions(ctx, false); err == nil {
		t.Fatal("resumed thread should have revoked the previous token")
	}
	assertToken(t, backend.URL, stB.Token)
	assertToken(t, backend.URL, stA4.Token)
}

func TestCodexThreadAdoptRotatedToken(t *testing.T) {
	backend, _, _, kinds := newButlerBackendStore(t)
	const (
		host    = "laptop"
		work    = "/tmp/proj-adopt"
		threadA = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	)
	stateDir := t.TempDir()
	session, _ := connectCodex(t, backend.URL, kinds["codex"], host, work, stateDir)
	out, isErr := callToolMeta(t, session, "list_sessions", nil, threadA, threadA)
	if isErr {
		t.Fatalf("bind: %s", out)
	}
	id := youAre(t, out)
	before := readThreadState(t, stateDir, threadA)

	kind, err := mcpserver.NewClient(backend.URL, kinds["codex"])
	if err != nil {
		t.Fatal(err)
	}
	reg, err := kind.RegisterAs(context.Background(), mcpserver.RegisterParams{
		Hostname: host, Workdir: work, ClientSession: threadA, DisplayName: "Codex " + threadA,
	})
	if err != nil {
		t.Fatalf("external re-register: %v", err)
	}
	if reg.Name != id || reg.Token == before.Token {
		t.Fatalf("external register: %+v (was %s)", reg, before.Token)
	}
	if err := mcpserver.WriteInstanceState(filepath.Join(stateDir, threadA+".json"), mcpserver.InstanceState{
		Agent: reg.Name, Token: reg.Token, Server: backend.URL, Kind: "codex",
		ThreadID: threadA, SessionID: threadA,
	}); err != nil {
		t.Fatal(err)
	}

	out, isErr = callToolMeta(t, session, "list_sessions", nil, threadA, threadA)
	if isErr {
		t.Fatalf("call after rotation: %s", out)
	}
	if got := youAre(t, out); got != id {
		t.Fatalf("adopted identity = %s, want %s", got, id)
	}
	after := readThreadState(t, stateDir, threadA)
	if after.Token != reg.Token {
		t.Fatalf("call re-registered (%s) instead of adopting %s", after.Token, reg.Token)
	}
	assertToken(t, backend.URL, reg.Token)
}

func TestCodexThreadsHaveSeparateCursors(t *testing.T) {
	backend, st, _, kinds := newButlerBackendStore(t)
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureGlobalSession(ctx, "admin"); err != nil {
		t.Fatal(err)
	}
	poster, err := mcpserver.NewClient(backend.URL, kinds["opencode"])
	if err != nil {
		t.Fatal(err)
	}
	preg, err := poster.Register(ctx, "laptop", "/tmp/proj-cursors")
	if err != nil {
		t.Fatal(err)
	}
	poster.SetToken(preg.Token)

	const (
		threadA = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
		threadB = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	)
	stateDir := t.TempDir()
	session, _ := connectCodex(t, backend.URL, kinds["codex"], "laptop", "/tmp/proj-cursors", stateDir)
	for _, id := range []string{threadA, threadB} {
		out, isErr := callToolMeta(t, session, "check_messages", nil, id, id)
		if isErr {
			t.Fatalf("init %s: %s", id, out)
		}
	}
	msg, err := poster.PostMessage(ctx, store.GlobalSessionID, "cursor-split-marker")
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if msg.Seq == 0 {
		t.Fatal("expected a seq")
	}
	for _, id := range []string{threadA, threadB} {
		out, isErr := callToolMeta(t, session, "check_messages", nil, id, id)
		if isErr || !strings.Contains(out, "cursor-split-marker") {
			t.Fatalf("thread %s missed the message (shared cursor?): err=%v\n%s", id, isErr, out)
		}
	}
}

func TestCodexMetaIgnoredWithoutThreadBinding(t *testing.T) {
	backend, _, _, kinds := newButlerBackendStore(t)
	session, appID := connectMCPClient(t, backend.URL, kinds["opencode"], "laptop", "/tmp/opencode")
	out, isErr := callToolMeta(t, session, "list_sessions", nil, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if isErr {
		t.Fatalf("opencode list_sessions: %s", out)
	}
	if got := youAre(t, out); got != appID {
		t.Fatalf("non-codex server should ignore threadId: got %s, app %s", got, appID)
	}
}

func TestConcurrentSameThreadRegistersOnce(t *testing.T) {
	backend, _, _, kinds := newButlerBackendStore(t)
	const threadA = "abababab-abab-4aba-8aba-abababababab"
	stateDir := t.TempDir()
	session, _ := connectCodex(t, backend.URL, kinds["codex"], "laptop", "/tmp/proj-race", stateDir)

	var wg sync.WaitGroup
	start := make(chan struct{})
	const n = 6
	outs := make([]string, n)
	errs := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			out, isErr := callToolMeta(t, session, "list_sessions", nil, threadA, threadA)
			outs[i] = out
			errs[i] = isErr
		}(i)
	}
	close(start)
	wg.Wait()

	var id string
	for i := 0; i < n; i++ {
		if errs[i] {
			t.Fatalf("call %d: %s", i, outs[i])
		}
		got := youAre(t, outs[i])
		if id == "" {
			id = got
		} else if got != id {
			t.Fatalf("concurrent calls split the thread: %s vs %s", id, got)
		}
	}
	st := readThreadState(t, stateDir, threadA)
	if st.Agent != id {
		t.Fatalf("state agent %s, calls reported %s", st.Agent, id)
	}
	assertToken(t, backend.URL, st.Token)
}

func connectCodex(t *testing.T, backendURL, kindToken, hostname, workdir, stateDir string) (*mcp.ClientSession, string) {
	t.Helper()
	client, err := mcpserver.NewClient(backendURL, kindToken)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	reg, err := client.Register(context.Background(), hostname, workdir)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	client.SetToken(reg.Token)
	server := mcpserver.New(client, reg.Name)
	server.EnableCodexThreads(mcpserver.CodexThreadConfig{
		ServerURL: backendURL,
		KindToken: kindToken,
		Hostname:  hostname,
		Workdir:   workdir,
		StateDir:  stateDir,
	})
	mcpServer := mcp.NewServer(&mcp.Implementation{Name: "butler", Version: "test"}, nil)
	mcpserver.RegisterTools(mcpServer, server)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	go func() {
		_ = mcpServer.Run(context.Background(), serverTransport)
	}()
	session, err := mcpClient.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session, reg.Name
}

func callToolMeta(t *testing.T, session *mcp.ClientSession, name string, args map[string]any, threadID, sessionID string) (string, bool) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	params := &mcp.CallToolParams{Name: name, Arguments: args}
	if threadID != "" || sessionID != "" {
		meta := mcp.Meta{}
		if threadID != "" {
			meta["threadId"] = threadID
		}
		if sessionID != "" {
			meta["sessionId"] = sessionID
		}
		params.Meta = meta
	}
	res, err := session.CallTool(context.Background(), params)
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

func youAre(t *testing.T, out string) string {
	t.Helper()
	const prefix = "you are "
	if !strings.HasPrefix(out, prefix) {
		t.Fatalf("list_sessions output %q", out)
	}
	line, _, _ := strings.Cut(out[len(prefix):], "\n")
	return strings.TrimSpace(line)
}

func readThreadState(t *testing.T, dir, threadID string) mcpserver.InstanceState {
	t.Helper()
	st, err := mcpserver.ReadInstanceState(filepath.Join(dir, threadID+".json"))
	if err != nil {
		t.Fatalf("read state %s: %v", threadID, err)
	}
	return st
}

func assertToken(t *testing.T, serverURL, token string) {
	t.Helper()
	c, err := mcpserver.NewClient(serverURL, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Sessions(context.Background(), false); err != nil {
		t.Fatalf("token rejected: %v", err)
	}
}
