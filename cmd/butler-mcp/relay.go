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
	"strings"
	"syscall"
	"time"

	"butler/internal/mcpserver"
	"butler/internal/store"
)

// relayCmd is the Codex counterpart of the OpenCode relay plugin
// (.opencode/plugins/butler): it acts as the agent identity recorded in the
// state file written at registration, long-polls the butler server for new
// messages, and injects them into a Codex session with `codex queue`. Codex
// has no in-process plugin API for submitting prompts, but its shared
// app-server daemon accepts queued messages for existing sessions: running
// sessions pick them up at the next turn boundary, closed sessions receive
// them on resume.
func relayCmd(args []string) {
	fs := flag.NewFlagSet("butler-mcp relay", flag.ExitOnError)
	workdir := fs.String("workdir", envOr("BUTLER_WORKDIR", defaultWorkdir()), "working directory of the identity (also used to find the codex session to deliver to)")
	stateFile := fs.String("state-file", envOr("BUTLER_STATE_FILE", ""), "instance state JSON written by butler-mcp at registration (default <workdir>/.butler/instance.json)")
	thread := fs.String("thread", "", "explicit codex thread id to deliver to (default: most recently active codex session in the workdir)")
	codexBin := fs.String("codex-bin", envOr("BUTLER_CODEX_BIN", "codex"), "codex CLI binary invoked to queue messages")
	codexHome := fs.String("codex-home", envOr("CODEX_HOME", defaultCodexHome()), "CODEX_HOME whose sessions/ directory is scanned for the target thread")
	once := fs.Bool("once", false, "exit after delivering one batch of messages (testing)")
	fs.Parse(args)

	statePath := *stateFile
	if statePath == "" {
		statePath = filepath.Join(*workdir, ".butler", "instance.json")
	}

	log.SetFlags(0)
	log.SetPrefix("butler-relay: ")
	log.SetOutput(os.Stderr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	state := waitStateFile(ctx, statePath)
	client, err := mcpserver.NewClient(state.Server, state.Token)
	if err != nil {
		log.Fatalf("invalid server URL %q: %v", state.Server, err)
	}
	log.Printf("relaying as %s -> codex sessions in %s (state: %s)", state.Agent, *workdir, statePath)

	var cursor int64 = -1 // set on first successful poll: never replay history
	backoff := time.Second
	failing := false
	lastFailLog := time.Now().Add(-time.Hour)

	for {
		if ctx.Err() != nil {
			log.Printf("shutting down")
			return
		}
		if cursor < 0 {
			seq, err := client.LatestSeq(ctx)
			if err != nil {
				log.Printf("initializing cursor: %v", err)
				sleep(ctx, backoff)
				backoff = minDuration(backoff*2, time.Minute)
				continue
			}
			cursor = seq
			log.Printf("cursor initialized to %d (old messages are never replayed)", cursor)
		}

		msgs, latest, err := client.Wait(ctx, cursor, 25)
		if err != nil {
			// The token rotates whenever a newer instance of this identity
			// re-registers (e.g. codex restarted): the state file is rewritten
			// by the last registrant — re-read it and continue.
			if isUnauthorized(err) {
				if st, rerr := readState(statePath); rerr == nil && st.Token != state.Token {
					log.Printf("token rotated — continuing as %s", st.Agent)
					state = st
					client.SetToken(st.Token)
					continue
				}
			}
			log.Printf("wait: %v", err)
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
			client:    client,
			self:      state.Agent,
			thread:    *thread,
			codexBin:  *codexBin,
			codexHome: *codexHome,
			workdir:   *workdir,
		}); err != nil {
			// Do not advance the cursor: the batch is retried on the next
			// poll (at-least-once delivery; thread discovery often only
			// succeeds once codex has been started in the workdir).
			if !failing || time.Since(lastFailLog) > time.Minute {
				log.Printf("delivery failed (will retry): %v", err)
				lastFailLog = time.Now()
				failing = true
			}
			sleep(ctx, 10*time.Second)
			continue
		}
		failing = false
		cursor = latest
		if *once {
			log.Printf("delivered one batch (--once), exiting")
			return
		}
	}
}

type deliveryConfig struct {
	client    *mcpserver.Client
	self      string
	thread    string
	codexBin  string
	codexHome string
	workdir   string
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
	threadID := cfg.thread
	if threadID == "" {
		var err error
		threadID, err = latestCodexThread(cfg.codexHome, cfg.workdir)
		if err != nil {
			return err
		}
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

type relayState struct {
	Agent  string `json:"agent"`
	Token  string `json:"token"`
	Server string `json:"server"`
	Kind   string `json:"kind"`
}

func readState(path string) (relayState, error) {
	var st relayState
	b, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("parse %s: %w", path, err)
	}
	if st.Agent == "" || st.Token == "" || st.Server == "" {
		return st, fmt.Errorf("%s: missing agent/token/server fields", path)
	}
	return st, nil
}

func waitStateFile(ctx context.Context, path string) relayState {
	for {
		if st, err := readState(path); err == nil {
			return st
		} else if !os.IsNotExist(err) {
			log.Fatalf("%v", err)
		}
		log.Printf("waiting for %s — start codex with the butler MCP server configured first", path)
		sleep(ctx, 5*time.Second)
	}
}

// latestCodexThread scans CODEX_HOME/sessions for the most recently modified
// rollout whose recorded working directory matches workdir — the codex
// equivalent of "the most recently active session".
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
