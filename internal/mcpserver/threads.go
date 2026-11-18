package mcpserver

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CodexInstructions is the MCP server instructions used when butler-mcp
// registers as the codex kind. Codex stamps the thread id on tools/call,
// not at process start, so the first Butler call is what binds the thread.
// A thread that never calls a tool is not bound; delivery to it is best-effort
// until that call happens.
const CodexInstructions = `Butler is the shared message bus for this project.

This Codex thread is registered with Butler on the first Butler tool call, using the thread id Codex attaches to that call. Until then, messages cannot be routed to this exact thread. At the start of the session, before other work, call list_sessions once (or get_context on the "global" session) so the thread is bound.

Resuming this thread keeps the same Butler identity. Forking starts a new one. A sub-agent shares the root thread's identity and does not appear as its own teammate. Reply to messages that @-mention you with the butler tools: ack briefly, do the work, then report back.`

// CodexBinding is the Butler identity a Codex tools/call should use.
// Bound is false when the call has no usable thread id (Codex before
// threadId was added to _meta, or any other harness): the process keeps
// its app-level identity.
type CodexBinding struct {
	// Key is the registration client_session.
	Key string
	// DeliverThread is the id `codex queue --thread` must use.
	DeliverThread string
	// SessionID is the root Codex session, stored beside the thread id.
	SessionID string
	// SubAgent is true when the call came from a child thread
	// (threadId != sessionId). Those fold into the root identity because
	// `codex queue` rejects many sub-agent threads.
	SubAgent bool
}

// BindCodexCall maps Codex _meta threadId and sessionId onto one Butler
// identity.
//
//   - no thread id, or an id that cannot be a client_session or a file name:
//     unbound
//   - thread id equals session id, or session id is absent: that thread is
//     its own identity (a fresh thread, a resume, or a fork)
//   - thread id differs from session id: a sub-agent; the identity and the
//     queue target are the root session id
func BindCodexCall(threadID, sessionID string) (CodexBinding, bool) {
	thread, ok := cleanCodexID(threadID)
	if !ok {
		return CodexBinding{}, false
	}
	session, sessionOK := cleanCodexID(sessionID)
	if sessionOK && session != thread {
		return CodexBinding{
			Key:           session,
			DeliverThread: session,
			SessionID:     session,
			SubAgent:      true,
		}, true
	}
	if !sessionOK {
		session = thread
	}
	return CodexBinding{
		Key:           thread,
		DeliverThread: thread,
		SessionID:     session,
	}, true
}

// cleanCodexID accepts the ids Codex actually stamps (UUIDs) and rejects
// anything that would be an unsafe client_session or state-file name.
func cleanCodexID(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 128 || s == "." || s == ".." || strings.HasPrefix(s, ".") {
		return "", false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return "", false
		}
	}
	return s, true
}

func codexDisplayName(threadID string) string {
	name := "Codex " + threadID
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

// CodexThreadConfig turns tools/call _meta into per-thread identities.
// Leave it unset for every kind other than codex: OpenCode, Pi and
// DeepSeek keep the single identity registered at startup.
type CodexThreadConfig struct {
	ServerURL string
	KindToken string
	Hostname  string
	Workdir   string
	// StateDir is where per-thread state files are written.
	// Default: <Workdir>/.butler/codex.
	StateDir string
}

type caller struct {
	client    *Client
	token     string
	agent     string
	cur       *seqCursor
	key       string
	threadID  string
	sessionID string
}

type seqCursor struct {
	mu          sync.Mutex
	lastSeq     int64
	initialized bool
}

func (c *seqCursor) get(ctx context.Context, client *Client) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.initialized {
		n, err := client.LatestSeq(ctx)
		if err != nil {
			return 0, err
		}
		c.lastSeq = n
		c.initialized = true
	}
	return c.lastSeq, nil
}

func (c *seqCursor) advance(seq int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if seq > c.lastSeq {
		c.lastSeq = seq
	}
}

type callerCtxKey struct{}

type inflightReg struct {
	done chan struct{}
	c    *caller
	err  error
}

type threadBook struct {
	cfg      CodexThreadConfig
	mu       sync.Mutex
	byKey    map[string]*caller
	inflight map[string]*inflightReg
}

func (s *Server) use(ctx context.Context) *caller {
	if c, ok := ctx.Value(callerCtxKey{}).(*caller); ok && c != nil {
		return c
	}
	return s.fallback
}

// EnableCodexThreads installs per-thread registration. Call it before any
// tool runs. A nil or empty config is ignored so other harnesses are unchanged.
func (s *Server) EnableCodexThreads(cfg CodexThreadConfig) {
	if strings.TrimSpace(cfg.KindToken) == "" || strings.TrimSpace(cfg.ServerURL) == "" {
		return
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		cfg.StateDir = filepath.Join(cfg.Workdir, ".butler", "codex")
	}
	s.threads = &threadBook{
		cfg:      cfg,
		byKey:    map[string]*caller{},
		inflight: map[string]*inflightReg{},
	}
}

func codexMeta(req *mcp.CallToolRequest) (threadID, sessionID string) {
	if req == nil || req.Params == nil {
		return "", ""
	}
	meta := req.Params.GetMeta()
	return metaString(meta, "threadId"), metaString(meta, "sessionId")
}

func metaString(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	v, ok := meta[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func (s *Server) prepareCall(ctx context.Context, req *mcp.CallToolRequest) (context.Context, error) {
	c := s.fallback
	if s.threads != nil {
		threadID, sessionID := codexMeta(req)
		bound, err := s.threads.bind(ctx, threadID, sessionID)
		if err != nil {
			return ctx, err
		}
		if bound != nil {
			c = bound
		}
	}
	return context.WithValue(ctx, callerCtxKey{}, c), nil
}

// rebind runs after a thread call was rejected with 401. Another process
// may already have written a fresh token for this thread; adopt that instead
// of registering again (a new registration would revoke the winner). If the
// file still holds the dead token, register once more.
func (s *Server) rebind(ctx context.Context, req *mcp.CallToolRequest) (context.Context, error) {
	current, _ := ctx.Value(callerCtxKey{}).(*caller)
	if s.threads == nil || current == nil || current.key == "" {
		return ctx, nil
	}
	threadID, sessionID := codexMeta(req)
	next, err := s.threads.refresh(ctx, current, threadID, sessionID)
	if err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, callerCtxKey{}, next), nil
}

func (b *threadBook) bind(ctx context.Context, threadID, sessionID string) (*caller, error) {
	binding, ok := BindCodexCall(threadID, sessionID)
	if !ok {
		if strings.TrimSpace(threadID) != "" {
			log.Printf("ignoring codex thread id %q (session %q): not a usable identity key", threadID, sessionID)
		}
		return nil, nil
	}
	b.mu.Lock()
	if c, ok := b.byKey[binding.Key]; ok {
		b.mu.Unlock()
		return c, nil
	}
	if w, ok := b.inflight[binding.Key]; ok {
		b.mu.Unlock()
		select {
		case <-w.done:
			return w.c, w.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	w := &inflightReg{done: make(chan struct{})}
	b.inflight[binding.Key] = w
	b.mu.Unlock()

	c, err := b.register(ctx, binding)
	b.mu.Lock()
	delete(b.inflight, binding.Key)
	if err == nil {
		b.byKey[binding.Key] = c
	}
	w.c, w.err = c, err
	close(w.done)
	b.mu.Unlock()
	return c, err
}

func (b *threadBook) refresh(ctx context.Context, current *caller, threadID, sessionID string) (*caller, error) {
	if next, ok := b.adoptStateFile(current); ok {
		log.Printf("codex thread %s adopted rotated token for %s", current.threadID, next.agent)
		return next, nil
	}
	b.mu.Lock()
	delete(b.byKey, current.key)
	b.mu.Unlock()
	log.Printf("codex thread %s token rejected; re-registering", current.threadID)
	return b.bind(ctx, threadID, sessionID)
}

// adoptStateFile switches current onto the token in its state file when a
// newer registrant has replaced ours. The cursor stays: the message stream
// is global, and this identity already consumed up to it.
func (b *threadBook) adoptStateFile(current *caller) (*caller, bool) {
	path, err := threadStatePath(b.cfg.StateDir, current.threadID)
	if err != nil {
		return nil, false
	}
	st, err := ReadInstanceState(path)
	if err != nil || st.Token == "" || st.Token == current.token || st.Agent == "" || st.Server == "" {
		return nil, false
	}
	client, err := NewClient(st.Server, st.Token)
	if err != nil {
		return nil, false
	}
	next := &caller{
		client:    client,
		token:     st.Token,
		agent:     st.Agent,
		cur:       current.cur,
		key:       current.key,
		threadID:  current.threadID,
		sessionID: current.sessionID,
	}
	b.mu.Lock()
	b.byKey[current.key] = next
	b.mu.Unlock()
	return next, true
}

func (b *threadBook) register(ctx context.Context, binding CodexBinding) (*caller, error) {
	client, err := NewClient(b.cfg.ServerURL, b.cfg.KindToken)
	if err != nil {
		return nil, err
	}
	reg, err := client.RegisterAs(ctx, RegisterParams{
		Hostname:      b.cfg.Hostname,
		Workdir:       b.cfg.Workdir,
		ClientSession: binding.Key,
		DisplayName:   codexDisplayName(binding.DeliverThread),
	})
	if err != nil {
		return nil, err
	}
	client.SetToken(reg.Token)
	c := &caller{
		client:    client,
		token:     reg.Token,
		agent:     reg.Name,
		cur:       &seqCursor{},
		key:       binding.Key,
		threadID:  binding.DeliverThread,
		sessionID: binding.SessionID,
	}
	st := InstanceState{
		Agent:     reg.Name,
		Token:     reg.Token,
		Server:    b.cfg.ServerURL,
		Kind:      reg.Kind,
		ThreadID:  binding.DeliverThread,
		SessionID: binding.SessionID,
		PID:       os.Getpid(),
		PPID:      os.Getppid(),
		WrittenAt: time.Now().Unix(),
	}
	path, perr := threadStatePath(b.cfg.StateDir, binding.DeliverThread)
	if perr != nil {
		log.Printf("codex thread %s registered as %s; state file skipped: %v", binding.DeliverThread, reg.Name, perr)
		return c, nil
	}
	if err := WriteInstanceState(path, st); err != nil {
		log.Printf("codex thread %s registered as %s; state file %s not written: %v", binding.DeliverThread, reg.Name, path, err)
		return c, nil
	}
	if binding.SubAgent {
		log.Printf("sub-agent folded into codex thread %s as %s", binding.DeliverThread, reg.Name)
	} else if reg.Replaced {
		log.Printf("codex thread %s re-registered as %s (previous instance replaced)", binding.DeliverThread, reg.Name)
	} else {
		log.Printf("codex thread %s registered as %s", binding.DeliverThread, reg.Name)
	}
	return c, nil
}

func threadStatePath(dir, threadID string) (string, error) {
	id, ok := cleanCodexID(threadID)
	if !ok {
		return "", fmt.Errorf("invalid codex thread id %q", threadID)
	}
	return filepath.Join(dir, id+".json"), nil
}

func unauthorizedToolResult(res *mcp.CallToolResult) bool {
	if res == nil || !res.IsError {
		return false
	}
	for _, c := range res.Content {
		tc, ok := c.(*mcp.TextContent)
		if ok && strings.Contains(tc.Text, "butler error (401)") {
			return true
		}
	}
	return false
}
