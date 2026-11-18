package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"butler/internal/mcpserver"
	"butler/internal/store"
)

// relayCmd is the Codex counterpart of the OpenCode relay plugin
// (plugins/opencode): it long-polls the butler server and injects messages
// into Codex with `codex queue`. Each bound Codex thread has its own state
// file under <workdir>/.butler/codex/<threadId>.json and its own loop, so
// the queue target is that thread id. When nothing is bound (Codex before
// threadId was added to MCP _meta, or a thread that has not called a Butler
// tool yet) one loop reads instance.json and falls back to the newest
// rollout in the workdir, which is what older releases always did.
func relayCmd(args []string) {
	fs := flag.NewFlagSet("butler-mcp relay", flag.ExitOnError)
	workdir := fs.String("workdir", envOr("BUTLER_WORKDIR", defaultWorkdir()), "working directory whose .butler/codex thread bindings are relayed")
	stateFile := fs.String("state-file", envOr("BUTLER_STATE_FILE", ""), "relay only this state file (default: every <workdir>/.butler/codex/<threadId>.json, else <workdir>/.butler/instance.json)")
	thread := fs.String("thread", "", "pin delivery to this codex thread. With --state-file it overrides the file; in auto mode only that thread's state file is relayed")
	codexBin := fs.String("codex-bin", envOr("BUTLER_CODEX_BIN", "codex"), "codex CLI binary invoked to queue messages")
	codexHome := fs.String("codex-home", envOr("CODEX_HOME", defaultCodexHome()), "CODEX_HOME whose sessions/ directory is scanned when no thread is bound")
	once := fs.Bool("once", false, "exit after delivering one batch of messages (testing)")
	fs.Parse(args)

	log.SetFlags(0)
	log.SetPrefix("butler-relay: ")
	log.SetOutput(os.Stderr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := relayConfig{
		workdir:   *workdir,
		stateFile: *stateFile,
		thread:    *thread,
		codexBin:  *codexBin,
		codexHome: *codexHome,
		once:      *once,
	}
	if err := runRelay(ctx, cfg); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("%v", err)
	}
}

type relayConfig struct {
	workdir     string
	stateFile   string
	thread      string
	codexBin    string
	codexHome   string
	once        bool
	waitSeconds int
	scanEvery   time.Duration
	onReady     func()
}

type loopConfig struct {
	statePath   string
	threadPin   string
	codexBin    string
	codexHome   string
	workdir     string
	once        bool
	waitSeconds int
	onReady     func()
}

func runRelay(ctx context.Context, cfg relayConfig) error {
	if cfg.waitSeconds <= 0 {
		cfg.waitSeconds = 25
	}
	if cfg.scanEvery <= 0 {
		cfg.scanEvery = 2 * time.Second
	}
	if cfg.stateFile != "" {
		return relayLoop(ctx, loopFrom(cfg, cfg.stateFile, cfg.thread))
	}
	if cfg.once {
		return relayOnce(ctx, cfg)
	}
	return relayWatch(ctx, cfg)
}

func loopFrom(cfg relayConfig, path, pin string) loopConfig {
	return loopConfig{
		statePath:   path,
		threadPin:   pin,
		codexBin:    cfg.codexBin,
		codexHome:   cfg.codexHome,
		workdir:     cfg.workdir,
		once:        cfg.once,
		waitSeconds: cfg.waitSeconds,
		onReady:     cfg.onReady,
	}
}

// relayOnce delivers a single batch for every thread bound right now, or
// one guessed batch when none are.
func relayOnce(ctx context.Context, cfg relayConfig) error {
	paths, err := boundThreadFiles(codexThreadDir(cfg.workdir))
	if err != nil {
		return err
	}
	paths = filterThreadFiles(paths, cfg.thread)
	if len(paths) == 0 {
		return relayLoop(ctx, loopFrom(cfg, instanceStatePath(cfg.workdir), cfg.thread))
	}
	var wg sync.WaitGroup
	errc := make(chan error, len(paths))
	for _, path := range paths {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			if err := relayLoop(ctx, loopFrom(cfg, path, "")); err != nil && !errors.Is(err, context.Canceled) {
				errc <- err
			}
		}(path)
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		return err
	}
	return nil
}

type loopSlot struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (s loopSlot) running() bool {
	if s.done == nil {
		return false
	}
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// relayWatch scans for new thread bindings until ctx is cancelled. A
// fallback loop (instance.json, newest rollout unless --thread is set)
// runs only while no thread file matches.
func relayWatch(ctx context.Context, cfg relayConfig) error {
	var mu sync.Mutex
	running := map[string]loopSlot{}
	var fallback loopSlot
	var wg sync.WaitGroup

	start := func(path, pin string, slot *loopSlot) {
		if slot.running() {
			return
		}
		lctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		*slot = loopSlot{cancel: cancel, done: done}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done)
			if err := relayLoop(lctx, loopFrom(cfg, path, pin)); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("%s: %v", path, err)
			}
		}()
	}

	scan := func() error {
		paths, err := boundThreadFiles(codexThreadDir(cfg.workdir))
		if err != nil {
			return err
		}
		matched := filterThreadFiles(paths, cfg.thread)
		mu.Lock()
		defer mu.Unlock()
		if len(matched) == 0 {
			for path, slot := range running {
				slot.cancel()
				delete(running, path)
			}
			if !fallback.running() {
				log.Printf("no codex thread bindings; delivering from %s", instanceStatePath(cfg.workdir))
				start(instanceStatePath(cfg.workdir), cfg.thread, &fallback)
			}
			return nil
		}
		if fallback.running() {
			log.Printf("%d codex thread binding(s); stopping unscoped delivery", len(matched))
			fallback.cancel()
			fallback = loopSlot{}
		}
		want := map[string]bool{}
		for _, path := range matched {
			want[path] = true
			slot := running[path]
			if slot.running() {
				continue
			}
			log.Printf("relaying bound thread state %s", path)
			start(path, "", &slot)
			running[path] = slot
		}
		for path, slot := range running {
			if !want[path] {
				slot.cancel()
				delete(running, path)
			}
		}
		return nil
	}

	if err := scan(); err != nil {
		return err
	}
	tick := time.NewTicker(cfg.scanEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			mu.Lock()
			if fallback.cancel != nil {
				fallback.cancel()
			}
			for _, slot := range running {
				slot.cancel()
			}
			mu.Unlock()
			wg.Wait()
			log.Printf("shutting down")
			return nil
		case <-tick.C:
			if err := scan(); err != nil {
				log.Printf("scan codex threads: %v", err)
			}
		}
	}
}

func codexThreadDir(workdir string) string {
	return filepath.Join(workdir, ".butler", "codex")
}

func instanceStatePath(workdir string) string {
	return filepath.Join(workdir, ".butler", "instance.json")
}

// boundThreadFiles returns state files that name a Codex thread. Files that
// are not readable yet (a writer mid-rename is atomic, so this is a bad
// file) are skipped.
func boundThreadFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		st, err := readState(path)
		if err != nil || st.ThreadID == "" {
			continue
		}
		out = append(out, path)
	}
	sort.Strings(out)
	return out, nil
}

func filterThreadFiles(paths []string, thread string) []string {
	if thread == "" {
		return paths
	}
	var out []string
	for _, path := range paths {
		st, err := readState(path)
		if err != nil || st.ThreadID != thread {
			continue
		}
		out = append(out, path)
	}
	return out
}

func relayLoop(ctx context.Context, cfg loopConfig) error {
	waitFor := cfg.waitSeconds
	if waitFor <= 0 {
		waitFor = 25
	}
	state, err := waitStateFile(ctx, cfg.statePath)
	if err != nil {
		return err
	}
	client, err := mcpserver.NewClient(state.Server, state.Token)
	if err != nil {
		return fmt.Errorf("invalid server URL %q: %w", state.Server, err)
	}
	label := state.Agent
	if state.ThreadID != "" {
		label = state.Agent + " thread " + state.ThreadID
	}
	log.Printf("relaying as %s (state: %s)", label, cfg.statePath)

	var cursor int64 = -1
	backoff := time.Second
	failing := false
	lastFailLog := time.Now().Add(-time.Hour)
	ready := false

	for {
		if ctx.Err() != nil {
			return nil
		}
		if cursor < 0 {
			seq, err := client.LatestSeq(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if st, ok := takeRotated(client, cfg.statePath, state, err); ok {
					state = st
					continue
				}
				log.Printf("%s: initializing cursor: %v", label, err)
				sleep(ctx, backoff)
				backoff = minDuration(backoff*2, time.Minute)
				continue
			}
			cursor = seq
			log.Printf("%s: cursor initialized to %d (old messages are never replayed)", label, cursor)
			if !ready && cfg.onReady != nil {
				ready = true
				cfg.onReady()
			}
		}

		msgs, latest, err := client.Wait(ctx, cursor, waitFor)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if st, ok := takeRotated(client, cfg.statePath, state, err); ok {
				log.Printf("%s: token rotated — continuing as %s", label, st.Agent)
				state = st
				if st.ThreadID != "" {
					label = st.Agent + " thread " + st.ThreadID
				}
				continue
			}
			log.Printf("%s: wait: %v", label, err)
			sleep(ctx, backoff)
			backoff = minDuration(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second

		deliverables := filterRelay(msgs, state.Agent)
		if len(deliverables) == 0 {
			cursor = latest
			continue
		}

		if err := deliver(ctx, deliverables, deliveryConfig{
			client:      client,
			self:        state.Agent,
			boundThread: state.ThreadID,
			threadPin:   cfg.threadPin,
			codexBin:    cfg.codexBin,
			codexHome:   cfg.codexHome,
			workdir:     cfg.workdir,
		}); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Do not advance the cursor: the batch is retried (at-least-once).
			// Thread discovery often only succeeds once codex has been
			// started in the workdir.
			if !failing || time.Since(lastFailLog) > time.Minute {
				log.Printf("%s: delivery failed (will retry): %v", label, err)
				lastFailLog = time.Now()
				failing = true
			}
			sleep(ctx, 10*time.Second)
			continue
		}
		failing = false
		cursor = latest
		if cfg.once {
			log.Printf("%s: delivered one batch (--once), exiting", label)
			return nil
		}
	}
}

// takeRotated re-reads the state file after a 401. The last registrant owns
// the file; when its token differs, keep using this client with the new
// token. Same token means the file was not updated, so the caller backs off.
func takeRotated(client *mcpserver.Client, path string, state mcpserver.InstanceState, err error) (mcpserver.InstanceState, bool) {
	if !isUnauthorized(err) {
		return state, false
	}
	st, rerr := readState(path)
	if rerr != nil || st.Token == "" || st.Token == state.Token {
		return state, false
	}
	client.SetToken(st.Token)
	return st, true
}

type deliveryConfig struct {
	client      *mcpserver.Client
	self        string
	boundThread string
	threadPin   string
	codexBin    string
	codexHome   string
	workdir     string
}

// filterRelay drops messages the codex agent must not see: its own
// send_message output (prevents echo loops) and system notices.
func filterRelay(msgs []store.Message, self string) []store.Message {
	var out []store.Message
	for _, m := range msgs {
		if m.Kind == "system" || m.SenderID == self {
			continue
		}
		out = append(out, m)
	}
	return out
}

func deliver(ctx context.Context, msgs []store.Message, cfg deliveryConfig) error {
	threadID, err := deliveryThread(cfg.boundThread, cfg.threadPin, cfg.codexHome, cfg.workdir)
	if err != nil {
		return err
	}
	text := formatRelay(ctx, cfg.client, msgs, cfg.self)
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(qctx, cfg.codexBin, "queue", "--thread", threadID, "--message", text).CombinedOutput()
	if err != nil {
		return fmt.Errorf("codex queue: %v: %s", err, firstLine(string(out)))
	}
	log.Printf("delivered %d message(s) to codex thread %s", len(msgs), threadID)
	return nil
}

// deliveryThread chooses the codex thread a batch is queued to.
// An explicit pin (--thread on a single state file) wins, then the thread
// id recorded when that identity registered, then the newest rollout whose
// cwd matches the workdir.
func deliveryThread(bound, pin, codexHome, workdir string) (string, error) {
	if pin != "" {
		return pin, nil
	}
	if bound != "" {
		return bound, nil
	}
	return latestCodexThread(codexHome, workdir)
}

// formatRelay renders a batch of butler messages as one prompt for the codex
// agent, grouped per butler session. Messages that @-mention the agent are
// flagged as requiring a reply; the rest are framed as FYI so the agent can
// ignore them when they are not relevant.
func formatRelay(ctx context.Context, client *mcpserver.Client, msgs []store.Message, self string) string {
	titles := map[string]string{}
	if sessions, err := client.Sessions(ctx, true); err == nil {
		for _, s := range sessions {
			titles[s.ID] = s.Title
		}
	}
	var order []string
	groups := map[string][]store.Message{}
	for _, m := range msgs {
		if _, ok := groups[m.SessionID]; !ok {
			order = append(order, m.SessionID)
		}
		groups[m.SessionID] = append(groups[m.SessionID], m)
	}
	mentioned := 0
	for _, m := range msgs {
		if m.MentionsAgent(self) {
			mentioned++
		}
	}

	var b strings.Builder
	if len(order) == 1 {
		fmt.Fprintf(&b, "[butler relay] %d new message(s) in %q:\n\n", len(msgs), sessionLabel(titles, order[0]))
	} else {
		fmt.Fprintf(&b, "[butler relay] %d new message(s) in %d session(s):\n", len(msgs), len(order))
	}
	for _, sid := range order {
		if len(order) > 1 {
			fmt.Fprintf(&b, "\n— session %q:\n", sessionLabel(titles, sid))
		}
		for _, m := range groups[sid] {
			flag := "FYI:"
			if m.MentionsAgent(self) {
				flag = "ADDRESSED TO YOU (reply expected):"
			}
			fmt.Fprintf(&b, "%s %s: %s\n", flag, m.SenderID, clip(m.Content, 2000))
		}
	}
	if mentioned > 0 {
		fmt.Fprintf(&b, "\n%d of these message(s) @-mention you — respond to those through the butler MCP tools (send_message / check_messages). The rest are FYI; ignore them unless relevant. If you decide to act on any of them, ack briefly first (send_message \"on it\"), then do the work and report back — don't stay silent until finished.\n", mentioned)
	} else {
		b.WriteString("\nNone of these messages @-mention you — they are FYI; reply through the butler MCP tools (send_message / check_messages) only if relevant.\n")
	}
	return b.String()
}

func sessionLabel(titles map[string]string, sid string) string {
	if t := titles[sid]; t != "" {
		return t
	}
	if len(sid) > 12 {
		return sid[:12]
	}
	return sid
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "… (truncated — read_session has the full text)"
}

func readState(path string) (mcpserver.InstanceState, error) {
	st, err := mcpserver.ReadInstanceState(path)
	if err != nil {
		return st, err
	}
	if st.Agent == "" || st.Token == "" || st.Server == "" {
		return st, fmt.Errorf("%s: missing agent/token/server fields", path)
	}
	return st, nil
}

func waitStateFile(ctx context.Context, path string) (mcpserver.InstanceState, error) {
	logged := false
	for {
		if ctx.Err() != nil {
			return mcpserver.InstanceState{}, ctx.Err()
		}
		st, err := readState(path)
		if err == nil {
			return st, nil
		}
		if !os.IsNotExist(err) {
			return st, err
		}
		if !logged {
			log.Printf("waiting for %s — start codex with the butler MCP server configured first", path)
			logged = true
		}
		sleep(ctx, 5*time.Second)
	}
}

// latestCodexThread scans CODEX_HOME/sessions for the most recently modified
// rollout whose recorded working directory matches workdir — the codex
// equivalent of "the most recently active session". Used only when no
// thread binding and no --thread pin exist.
func latestCodexThread(codexHome, workdir string) (string, error) {
	root := filepath.Join(codexHome, "sessions")
	cutoff := time.Now().AddDate(0, 0, -30)
	best, bestMod := "", time.Time{}
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.ModTime().Before(cutoff) {
			return nil
		}
		id, cwd := rolloutMeta(path)
		if id == "" || cwd != workdir {
			return nil
		}
		if info.ModTime().After(bestMod) {
			best, bestMod = id, info.ModTime()
		}
		return nil
	})
	if best == "" {
		return "", fmt.Errorf("no codex session for workdir %s within the last 30 days (open codex there once)", workdir)
	}
	return best, nil
}

// rolloutMeta reads the session_meta record (first JSON value of the rollout
// file) and returns its thread id — `thread_id` on older rollouts, `id`
// otherwise — plus the recorded working directory.
func rolloutMeta(path string) (threadID, cwd string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	var rec struct {
		Payload struct {
			ID       string `json:"id"`
			ThreadID string `json:"thread_id"`
			Cwd      string `json:"cwd"`
		} `json:"payload"`
	}
	if err := json.NewDecoder(f).Decode(&rec); err != nil {
		return "", ""
	}
	if rec.Payload.ThreadID != "" {
		return rec.Payload.ThreadID, rec.Payload.Cwd
	}
	return rec.Payload.ID, rec.Payload.Cwd
}

func isUnauthorized(err error) bool {
	var e *mcpserver.Error
	return errors.As(err, &e) && e.StatusCode == 401
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func defaultCodexHome() string {
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".codex")
	}
	return ".codex"
}
