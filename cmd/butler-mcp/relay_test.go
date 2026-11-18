package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"butler/internal/api"
	"butler/internal/auth"
	"butler/internal/hub"
	"butler/internal/mcpserver"
	"butler/internal/metrics"
	"butler/internal/store"
)

func TestDeliveryThread(t *testing.T) {
	home := t.TempDir()
	const work = "/work/proj"
	writeRollout(t, home, "rollout-new.jsonl", "guessed-thread", "", work, time.Now())

	got, err := deliveryThread("bound-thread", "", home, work)
	if err != nil || got != "bound-thread" {
		t.Fatalf("bound thread: %q %v", got, err)
	}
	got, err = deliveryThread("bound-thread", "pinned-thread", home, work)
	if err != nil || got != "pinned-thread" {
		t.Fatalf("pin: %q %v", got, err)
	}
	got, err = deliveryThread("", "", home, work)
	if err != nil || got != "guessed-thread" {
		t.Fatalf("guess: %q %v", got, err)
	}
	got, err = deliveryThread("", "pinned-thread", home, work)
	if err != nil || got != "pinned-thread" {
		t.Fatalf("pin without binding: %q %v", got, err)
	}
}

func TestLatestCodexThread(t *testing.T) {
	home := t.TempDir()
	const work = "/work/proj"
	now := time.Now()
	writeRollout(t, home, "rollout-stale.jsonl", "stale-thread", "", work, now.AddDate(0, 0, -31))
	writeRollout(t, home, "rollout-other.jsonl", "other-cwd", "", "/elsewhere", now.Add(time.Minute))
	writeRollout(t, home, "rollout-old-id.jsonl", "from-id", "", work, now.Add(-time.Hour))
	writeRollout(t, home, "rollout-legacy.jsonl", "ignored-id", "legacy-thread", work, now)

	got, err := latestCodexThread(home, work)
	if err != nil || got != "legacy-thread" {
		t.Fatalf("latest = %q, %v", got, err)
	}
}

func TestRelayOnceDeliversToEachBoundThread(t *testing.T) {
	env := newRelayEnv(t)
	const (
		threadA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		threadB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	)
	env.registerThread(threadA)
	env.registerThread(threadB)
	writeRollout(t, env.codexHome, "rollout-decoy.jsonl", "decoy-thread", "", env.work, time.Now())

	var mu sync.Mutex
	readyN := 0
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runRelay(ctx, relayConfig{
			workdir:     env.work,
			codexBin:    env.codexBin,
			codexHome:   env.codexHome,
			once:        true,
			waitSeconds: 5,
			onReady: func() {
				mu.Lock()
				readyN++
				if readyN == 2 {
					close(ready)
				}
				mu.Unlock()
			},
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("relay exited before both threads were ready: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for both thread loops")
	}
	env.post("hello both threads")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("relay did not deliver")
	}
	logText := env.queueLog()
	for _, id := range []string{threadA, threadB} {
		if !strings.Contains(logText, "queue --thread "+id) {
			t.Fatalf("queue log missing %s:\n%s", id, logText)
		}
	}
	if strings.Contains(logText, "decoy-thread") {
		t.Fatalf("bound relay guessed a rollout:\n%s", logText)
	}
}

func TestRelayOnceFallsBackWithoutThreadID(t *testing.T) {
	env := newRelayEnv(t)
	env.registerApp()
	now := time.Now()
	writeRollout(t, env.codexHome, "rollout-older.jsonl", "older-thread", "", env.work, now.Add(-time.Hour))
	writeRollout(t, env.codexHome, "rollout-newer.jsonl", "newer-thread", "", env.work, now)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runRelay(ctx, relayConfig{
			workdir:     env.work,
			codexBin:    env.codexBin,
			codexHome:   env.codexHome,
			once:        true,
			waitSeconds: 5,
			onReady:     func() { close(ready) },
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("relay exited early: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("fallback loop did not start")
	}
	env.post("hello fallback")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("fallback did not deliver")
	}
	logText := env.queueLog()
	if !strings.Contains(logText, "queue --thread newer-thread") {
		t.Fatalf("fallback should queue the newest rollout:\n%s", logText)
	}
	if strings.Contains(logText, "older-thread") {
		t.Fatalf("fallback queued the older rollout:\n%s", logText)
	}
}

func TestRelayExplicitThreadPin(t *testing.T) {
	env := newRelayEnv(t)
	env.registerApp()
	writeRollout(t, env.codexHome, "rollout-newer.jsonl", "newer-thread", "", env.work, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runRelay(ctx, relayConfig{
			workdir:     env.work,
			thread:      "pinned-thread",
			codexBin:    env.codexBin,
			codexHome:   env.codexHome,
			once:        true,
			waitSeconds: 5,
			onReady:     func() { close(ready) },
		})
	}()
	select {
	case <-ready:
	case <-time.After(8 * time.Second):
		t.Fatal("pinned loop did not start")
	}
	env.post("hello pin")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("pin did not deliver")
	}
	logText := env.queueLog()
	if !strings.Contains(logText, "queue --thread pinned-thread") || strings.Contains(logText, "newer-thread") {
		t.Fatalf("pin log:\n%s", logText)
	}
}

func TestRelayRereadsToken(t *testing.T) {
	env := newRelayEnv(t)
	threadID := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	env.registerThread(threadID)

	var logs bytes.Buffer
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runRelay(ctx, relayConfig{
			workdir:     env.work,
			codexBin:    env.codexBin,
			codexHome:   env.codexHome,
			once:        true,
			waitSeconds: 1,
			onReady:     func() { close(ready) },
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("relay exited early: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("loop did not start")
	}

	kind, err := mcpserver.NewClient(env.backend.URL, env.kinds["codex"])
	if err != nil {
		t.Fatal(err)
	}
	reg, err := kind.RegisterAs(context.Background(), mcpserver.RegisterParams{
		Hostname: "relay-host", Workdir: env.work, ClientSession: threadID, DisplayName: "Codex " + threadID,
	})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if err := mcpserver.WriteInstanceState(filepath.Join(env.work, ".butler", "codex", threadID+".json"), mcpserver.InstanceState{
		Agent: reg.Name, Token: reg.Token, Server: env.backend.URL, Kind: "codex",
		ThreadID: threadID, SessionID: threadID,
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(6 * time.Second)
	for !strings.Contains(logs.String(), "token rotated") {
		if time.Now().After(deadline) {
			t.Fatalf("relay did not pick up the rotated token:\n%s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	env.post("after rotation")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("relay did not deliver after rotation")
	}
	if !strings.Contains(env.queueLog(), "queue --thread "+threadID) {
		t.Fatalf("rotated relay queued the wrong thread:\n%s", env.queueLog())
	}
}

func TestRelayWatchPicksUpANewThread(t *testing.T) {
	env := newRelayEnv(t)
	threadID := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	// Registered, but the state file is withheld until the relay is already
	// in its unscoped fallback (no instance.json, so it is waiting).
	reg := env.preregister(threadID)
	writeRollout(t, env.codexHome, "rollout-decoy.jsonl", "decoy-thread", "", env.work, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	var once sync.Once
	errc := make(chan error, 1)
	go func() {
		errc <- runRelay(ctx, relayConfig{
			workdir:     env.work,
			codexBin:    env.codexBin,
			codexHome:   env.codexHome,
			waitSeconds: 5,
			scanEvery:   20 * time.Millisecond,
			onReady:     func() { once.Do(func() { close(ready) }) },
		})
	}()
	time.Sleep(50 * time.Millisecond)
	if err := mcpserver.WriteInstanceState(filepath.Join(env.work, ".butler", "codex", threadID+".json"), mcpserver.InstanceState{
		Agent: reg.Name, Token: reg.Token, Server: env.backend.URL, Kind: "codex",
		ThreadID: threadID, SessionID: threadID,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case err := <-errc:
		t.Fatalf("watch exited: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not start the bound thread")
	}
	env.post("watched thread")
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(env.queueLog(), "queue --thread "+threadID) {
		if time.Now().After(deadline) {
			t.Fatalf("watch log:\n%s", env.queueLog())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Contains(env.queueLog(), "decoy-thread") {
		t.Fatalf("watch fell back to a rollout:\n%s", env.queueLog())
	}
	cancel()
	select {
	case <-errc:
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not shut down")
	}
}

type relayEnv struct {
	t         *testing.T
	backend   *httptest.Server
	kinds     map[string]string
	work      string
	codexHome string
	codexBin  string
	logPath   string
	poster    *mcpserver.Client
}

func newRelayEnv(t *testing.T) *relayEnv {
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
	ctx := context.Background()
	if _, err := st.CreateAgent(ctx, "admin", "Admin", "human", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureGlobalSession(ctx, "admin"); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, id := range []string{"codex", "opencode"} {
		tok := auth.NewToken()
		if _, err := st.CreateKind(ctx, id, id, auth.Hash(tok)); err != nil {
			t.Fatal(err)
		}
		kinds[id] = tok
	}
	srv := httptest.NewServer(a.Routes())
	t.Cleanup(srv.Close)

	work := filepath.Join(dir, "proj")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	poster, err := mcpserver.NewClient(srv.URL, kinds["opencode"])
	if err != nil {
		t.Fatal(err)
	}
	preg, err := poster.Register(ctx, "relay-host", work)
	if err != nil {
		t.Fatal(err)
	}
	poster.SetToken(preg.Token)

	bin, logPath := writeFakeCodex(t, dir)
	return &relayEnv{
		t: t, backend: srv, kinds: kinds, work: work,
		codexHome: filepath.Join(dir, "codex-home"), codexBin: bin, logPath: logPath,
		poster: poster,
	}
}

func (e *relayEnv) registerApp() {
	e.t.Helper()
	e.writeIdentity("")
}

func (e *relayEnv) registerThread(threadID string) {
	e.t.Helper()
	reg := e.preregister(threadID)
	path := filepath.Join(e.work, ".butler", "codex", threadID+".json")
	if err := mcpserver.WriteInstanceState(path, mcpserver.InstanceState{
		Agent: reg.Name, Token: reg.Token, Server: e.backend.URL, Kind: "codex",
		ThreadID: threadID, SessionID: threadID,
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *relayEnv) preregister(threadID string) *mcpserver.RegisterResult {
	e.t.Helper()
	return e.writeIdentity(threadID)
}

func (e *relayEnv) writeIdentity(threadID string) *mcpserver.RegisterResult {
	e.t.Helper()
	c, err := mcpserver.NewClient(e.backend.URL, e.kinds["codex"])
	if err != nil {
		e.t.Fatal(err)
	}
	reg, err := c.RegisterAs(context.Background(), mcpserver.RegisterParams{
		Hostname: "relay-host", Workdir: e.work, ClientSession: threadID,
		DisplayName: "Codex " + threadID,
	})
	if err != nil {
		e.t.Fatalf("register %q: %v", threadID, err)
	}
	if threadID == "" {
		if err := mcpserver.WriteInstanceState(instanceStatePath(e.work), mcpserver.InstanceState{
			Agent: reg.Name, Token: reg.Token, Server: e.backend.URL, Kind: "codex",
		}); err != nil {
			e.t.Fatal(err)
		}
	}
	return reg
}

func (e *relayEnv) post(text string) {
	e.t.Helper()
	if _, err := e.poster.PostMessage(context.Background(), store.GlobalSessionID, text); err != nil {
		e.t.Fatalf("post: %v", err)
	}
}

func (e *relayEnv) queueLog() string {
	b, err := os.ReadFile(e.logPath)
	if err != nil {
		return ""
	}
	return string(b)
}

func writeFakeCodex(t *testing.T, dir string) (string, string) {
	t.Helper()
	logPath := filepath.Join(dir, "queue.log")
	bin := filepath.Join(dir, "codex")
	script := "#!/bin/sh\nprintf '%s %s %s\\n' \"$1\" \"$2\" \"$3\" >> '" + logPath + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

func writeRollout(t *testing.T, home, name, id, threadID, cwd string, mod time.Time) {
	t.Helper()
	dir := filepath.Join(home, "sessions", "2026", "11")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"id": id, "cwd": cwd}
	if threadID != "" {
		payload["thread_id"] = threadID
	}
	body, err := json.Marshal(map[string]any{"payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}
