package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"butler/internal/api"
	"butler/internal/auth"
	"butler/internal/hub"
	"butler/internal/ids"
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
	case "reset":
		resetCmd(rest)
	case "version":
		fmt.Println("butler " + version)
	default:
		fmt.Fprintln(os.Stderr, "usage: butler [serve|kind|agent|reset|version] [flags]")
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
	bootstrapAgent(context.Background(), log, st, "admin", "Admin", "human")
	bootstrapGlobalSession(context.Background(), log, st)

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
	if _, err := st.CreateAgent(ctx, id, name, kind, "", "", ""); err != nil {
		fatal(log, err)
	}
	log.Info("bootstrap agent created", "id", id, "kind", kind)
}

// bootstrapGlobalSession ensures the default global session exists (created
// by "admin") and that every registered agent is a member. Join notices are
// posted only for agents that are not members yet — typically the first start
// after upgrading a database that predates the global session.
func bootstrapGlobalSession(ctx context.Context, log *slog.Logger, st *store.Store) {
	created, err := st.EnsureGlobalSession(ctx, "admin")
	if err != nil {
		fatal(log, err)
	}
	if created {
		log.Info("global session created", "id", store.GlobalSessionID)
	}
	agents, err := st.ListAgents(ctx)
	if err != nil {
		fatal(log, err)
	}
	for _, ag := range agents {
		if ag.ID == "admin" {
			continue // added by EnsureGlobalSession
		}
		joined, err := st.JoinSession(ctx, store.GlobalSessionID, ag.ID)
		if err != nil {
			log.Error("join global session", "err", err, "agent", ag.ID)
			continue
		}
		if joined {
			log.Info("agent joined global session", "agent", ag.ID)
		}
	}
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

// resetCmd wipes the database (sessions, messages, agents, kinds) by moving
// the DB files aside and recreating an empty schema. The admin token lives in
// its own file outside the database and is deliberately untouched. Kind
// tokens are stored in the DB, so they must be re-issued afterwards.
func resetCmd(args []string) {
	fs := flag.NewFlagSet("reset", flag.ExitOnError)
	dataDir := fs.String("data-dir", envOr("BUTLER_DATA_DIR", defaultDir(".local/share/butler")), "data directory")
	listen := fs.String("listen", envOr("BUTLER_LISTEN", "127.0.0.1:8787"), "listen address, checked to refuse reset while the server runs")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	fs.Parse(args)

	dbPath := filepath.Join(*dataDir, "butler.db")

	// Moving the DB out from under a running server corrupts its state.
	if conn, err := net.DialTimeout("tcp", *listen, 500*time.Millisecond); err == nil {
		conn.Close()
		fmt.Fprintf(os.Stderr, "error: something is listening on %s — stop the butler server before resetting\n", *listen)
		os.Exit(1)
	}

	if !*yes {
		fmt.Printf("reset moves %s aside (sessions, messages, agents and kinds are wiped) and recreates an empty database.\nthe admin token is NOT affected. kind tokens stop working and must be re-issued.\ncontinue? [y/N] ", dbPath)
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" && !strings.EqualFold(answer, "yes") {
			fmt.Println("aborted")
			return
		}
	}

	ts := time.Now().UTC().Format("20060102-150405")
	var moved []string
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src := dbPath + suffix
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := src + ".old-" + ts
		for n := 2; ; n++ {
			if _, err := os.Stat(dst); os.IsNotExist(err) {
				break
			}
			// Same-second reset: never clobber an earlier backup.
			dst = fmt.Sprintf("%s.old-%s-%d", src, ts, n)
		}
		if err := os.Rename(src, dst); err != nil {
			fmt.Fprintf(os.Stderr, "error: moving %s aside: %v\n", src, err)
			os.Exit(1)
		}
		moved = append(moved, dst)
	}
	if len(moved) == 0 {
		fmt.Printf("no database at %s — creating a fresh one\n", dbPath)
	} else {
		fmt.Printf("database moved aside: %s\n", strings.Join(moved, ", "))
	}

	st, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: recreating database: %v\n", err)
		os.Exit(1)
	}
	bootstrapAgent(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), st, "admin", "Admin", "human")
	if _, err := st.EnsureGlobalSession(context.Background(), "admin"); err != nil {
		fmt.Fprintf(os.Stderr, "error: creating global session: %v\n", err)
		os.Exit(1)
	}
	if err := st.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("empty database created (agent \"admin\" bootstrapped, global session created)")
	fmt.Println("admin token unchanged — it lives in its own file")
	fmt.Println(`next: re-issue kind tokens ("butler kind add <id>"), update your agents' configs, then start the server`)
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
		if !api.ValidKindID(id) {
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
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tDISPLAY\tCREATED")
		for _, k := range kinds {
			fmt.Fprintf(w, "%s\t%s\t%s\n", k.ID, k.DisplayName, time.Unix(k.CreatedAt, 0).UTC().Format(time.RFC3339))
		}
		w.Flush()
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
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tTITLE\tKIND\tHOST\tWORKDIR\tLAST_SEEN")
		for _, a := range agents {
			last := "never"
			if a.LastSeenAt != nil {
				last = time.Unix(*a.LastSeenAt, 0).UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.DisplayName, a.Kind, a.Hostname, a.Workdir, last)
		}
		w.Flush()
	case "revoke":
		if len(positional) == 0 {
			agentUsage()
			os.Exit(2)
		}
		id := positional[0]
		agent, err := st.GetAgent(ctx, id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		if agent.ID == "admin" {
			fmt.Fprintln(os.Stderr, "error: the admin agent cannot be revoked")
			os.Exit(1)
		}
		if err := st.RemoveAgent(ctx, id); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		// Post the eviction notice directly (the CLI has no hub access; the
		// server's waiters see it on their next poll, up to ~25s later).
		sids, err := st.SessionsForAgent(ctx, id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not list sessions: %v\n", err)
		}
		for _, sid := range sids {
			if _, err := st.InsertMessage(ctx, &store.Message{
				ID: ids.New(), SessionID: sid, SenderID: id, Kind: "system",
				Content: store.NoticeLabel(agent) + " was kicked by admin (tokens revoked; hidden until it re-registers)",
			}); err != nil {
				fmt.Fprintf(os.Stderr, "warning: eviction notice for %s: %v\n", sid, err)
			}
		}
		fmt.Printf("evicted %s: tokens revoked, hidden from the panel and sessions until it re-registers\n", id)
	default:
		agentUsage()
		os.Exit(2)
	}
}

func kindUsage() {
	fmt.Fprint(os.Stderr, `usage (flags before or after the id):
  butler kind add <id> [--name X]
  butler kind rotate <id>
  butler kind list
`)
}

func agentUsage() {
	fmt.Fprint(os.Stderr, `agent instances register themselves with a kind token; manage kinds with "butler kind".
usage (flags before or after the id):
  butler agent list
  butler agent revoke <name>   # evict: revoke tokens + hide until it re-registers
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
