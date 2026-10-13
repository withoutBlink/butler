package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"butler/internal/api"
	"butler/internal/auth"
	"butler/internal/hub"
	"butler/internal/metrics"
	"butler/internal/store"
	"butler/internal/ui"
)

const version = "0.1.0"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	args := os.Args[1:]
	cmd := "serve"
	var rest []string
	if len(args) > 0 {
		cmd = args[0]
		rest = args[1:]
	}
	switch cmd {
	case "serve":
		serve(log, rest)
	case "kind":
		kindCmd(rest)
	case "agent":
		agentCmd(rest)
	case "version":
		fmt.Println("butler-server " + version)
	default:
		fmt.Fprintln(os.Stderr, "usage: butler-server [serve|kind|agent|version] [flags]")
		os.Exit(2)
	}
}

func serve(log *slog.Logger, args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", envOr("BUTLER_LISTEN", "127.0.0.1:8787"), "listen address")
	dataDir := fs.String("data-dir", envOr("BUTLER_DATA_DIR", defaultDir(".local/share/butler")), "data directory")
	configDir := fs.String("config-dir", envOr("BUTLER_CONFIG_DIR", defaultDir(".config/butler")), "config directory")
	fs.Parse(args)

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		fatal(log, err)
	}
	if err := os.MkdirAll(*configDir, 0o755); err != nil {
		fatal(log, err)
	}
	tokenPath := filepath.Join(*configDir, "admin-token")
	adminToken, created, err := loadOrCreateToken(tokenPath)
	if err != nil {
		fatal(log, err)
	}
	if created {
		fmt.Fprintf(os.Stderr, "admin token (shown once, also stored at %s):\n%s\n", tokenPath, adminToken)
	} else {
		log.Info("admin token loaded", "path", tokenPath)
	}

	st, err := store.Open(filepath.Join(*dataDir, "butler.db"))
	if err != nil {
		fatal(log, err)
	}
	bootstrapAgent(context.Background(), log, st, "you", "You", "human")

	h := hub.New()
	m := &metrics.Metrics{}
	done := make(chan struct{})
	panel, err := ui.New(st, h, adminToken, log)
	if err != nil {
		fatal(log, err)
	}
	a := &api.API{
		Store:      st,
		Hub:        h,
		AdminToken: adminToken,
		Log:        log,
		Metrics:    m,
		Done:       done,
		Panel:      panel,
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           a.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Info("listening", "addr", *listen, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-sigCtx.Done()
	log.Info("shutting down")
	h.Close()
	close(done)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
	if err := st.Close(); err != nil {
		log.Error("close store", "err", err)
	}
}

func fatal(log *slog.Logger, err error) {
	log.Error("fatal", "err", err)
	os.Exit(1)
}

func bootstrapAgent(ctx context.Context, log *slog.Logger, st *store.Store, id, name, kind string) {
	if _, err := st.GetAgent(ctx, id); err == nil {
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		fatal(log, err)
	}
	if _, err := st.CreateAgent(ctx, id, name, kind, "", ""); err != nil {
		fatal(log, err)
	}
	log.Info("bootstrap agent created", "id", id, "kind", kind)
}

// parseSubFlags parses fs over args, rotating a leading positional argument
// past flags so `kind add <id> --name X` and `kind add --name X <id>` both
// work (Go's flag stops at the first positional argument).
func parseSubFlags(fs *flag.FlagSet, args []string) []string {
	rest := args
	for {
		fs.Parse(rest)
		positional := fs.Args()
		if len(positional) > 1 && strings.HasPrefix(positional[1], "-") {
			rotated := make([]string, 0, len(positional))
			rotated = append(rotated, positional[1:]...)
			rotated = append(rotated, positional[0])
			rest = rotated
			continue
		}
		return positional
	}
}

func openStore(dataDir string) (*store.Store, error) {
	return store.Open(filepath.Join(dataDir, "butler.db"))
}

func kindCmd(args []string) {
	if len(args) == 0 {
		kindUsage()
		os.Exit(2)
	}
	sub := args[0]
	fs := flag.NewFlagSet("kind "+sub, flag.ExitOnError)
	dataDir := fs.String("data-dir", envOr("BUTLER_DATA_DIR", defaultDir(".local/share/butler")), "data directory")
	name := fs.String("name", "", "display name")
	positional := parseSubFlags(fs, args[1:])

	st, err := openStore(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()
	ctx := context.Background()

	switch sub {
	case "add":
		if len(positional) == 0 {
			kindUsage()
			os.Exit(2)
		}
		id := positional[0]
		if !api.ValidAgentID(id) {
			fmt.Fprintf(os.Stderr, "error: invalid kind id %q (lowercase letters, digits, - and _; max 32 chars)\n", id)
			os.Exit(1)
		}
		display := strings.TrimSpace(*name)
		if display == "" {
			display = id
		}
		tok := auth.NewToken()
		if _, err := st.CreateKind(ctx, id, display, auth.Hash(tok)); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("kind %s created\nkind token (shown once — every instance of this kind registers with it):\n%s\n", id, tok)
	case "rotate":
		if len(positional) == 0 {
			kindUsage()
			os.Exit(2)
		}
		id := positional[0]
		tok := auth.NewToken()
		if err := st.RotateKindToken(ctx, id, auth.Hash(tok)); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("kind %s token rotated\ntoken (shown once): %s\n", id, tok)
	case "list":
		kinds, err := st.ListKinds(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("%-16s %-20s %s\n", "ID", "DISPLAY", "CREATED")
		for _, k := range kinds {
			fmt.Printf("%-16s %-20s %s\n", k.ID, k.DisplayName, time.Unix(k.CreatedAt, 0).UTC().Format(time.RFC3339))
		}
	default:
		kindUsage()
		os.Exit(2)
	}
}

func agentCmd(args []string) {
	if len(args) == 0 {
		agentUsage()
		os.Exit(2)
	}
	sub := args[0]
	fs := flag.NewFlagSet("agent "+sub, flag.ExitOnError)
	dataDir := fs.String("data-dir", envOr("BUTLER_DATA_DIR", defaultDir(".local/share/butler")), "data directory")
	positional := parseSubFlags(fs, args[1:])

	st, err := openStore(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()
	ctx := context.Background()

	switch sub {
	case "list":
		agents, err := st.ListAgents(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("%-24s %-10s %-18s %-30s %s\n", "NAME", "KIND", "HOST", "WORKDIR", "LAST_SEEN")
		for _, a := range agents {
			last := "never"
			if a.LastSeenAt != nil {
				last = time.Unix(*a.LastSeenAt, 0).UTC().Format(time.RFC3339)
			}
			fmt.Printf("%-24s %-10s %-18s %-30s %s\n", a.ID, a.Kind, a.Hostname, a.Workdir, last)
		}
	case "revoke":
		if len(positional) == 0 {
			agentUsage()
			os.Exit(2)
		}
		id := positional[0]
		if _, err := st.GetAgent(ctx, id); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		if err := st.RevokeAgentTokens(ctx, id); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("all instance tokens for %s revoked; its live processes must re-register\n", id)
	default:
		agentUsage()
		os.Exit(2)
	}
}

func kindUsage() {
	fmt.Fprint(os.Stderr, `usage (flags before or after the id):
  butler-server kind add <id> [--name X]
  butler-server kind rotate <id>
  butler-server kind list
`)
}

func agentUsage() {
	fmt.Fprint(os.Stderr, `agent instances register themselves with a kind token; manage kinds with "butler-server kind".
usage (flags before or after the id):
  butler-server agent list
  butler-server agent revoke <name>
`)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func defaultDir(rel string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Base(rel)
	}
	return filepath.Join(home, rel)
}

func loadOrCreateToken(path string) (string, bool, error) {
	if b, err := os.ReadFile(path); err == nil {
		t := strings.TrimSpace(string(b))
		if t != "" {
			return t, false, nil
		}
	}
	tok := auth.NewToken()
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", false, err
	}
	return tok, true, nil
}
