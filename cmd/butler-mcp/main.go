package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"butler/internal/mcpserver"
)

const version = "0.1.0"

func main() {
	// `butler-mcp relay ...` runs the codex relay daemon instead of the MCP
	// server (see relay.go); plain `butler-mcp [flags]` serves MCP on stdio.
	if len(os.Args) > 1 && os.Args[1] == "relay" {
		relayCmd(os.Args[2:])
		return
	}
	fs := flag.NewFlagSet("butler-mcp", flag.ExitOnError)
	serverURL := fs.String("server", envOr("BUTLER_SERVER", "http://127.0.0.1:8787"), "butler server URL")
	token := fs.String("token", envOr("BUTLER_TOKEN", ""), "kind token (the single token printed by `butler kind add <kind>`)")
	hostname := fs.String("hostname", envOr("BUTLER_HOSTNAME", defaultHostname()), "hostname reported to the butler server")
	workdir := fs.String("workdir", envOr("BUTLER_WORKDIR", defaultWorkdir()), "working directory reported to the butler server")
	stateFile := fs.String("state-file", envOr("BUTLER_STATE_FILE", ""), "instance state JSON written after registration so local tooling can act as this agent (default <workdir>/.butler/instance.json)")
	fs.Parse(os.Args[1:])
	if *token == "" {
		fmt.Fprintln(os.Stderr, "butler-mcp: --token (or BUTLER_TOKEN) is required — pass the kind token printed by `butler kind add <kind>`")
		os.Exit(2)
	}
	client, err := mcpserver.NewClient(*serverURL, *token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "butler-mcp: %v\n", err)
		os.Exit(2)
	}

	log.SetFlags(0)
	log.SetPrefix("butler-mcp: ")
	log.SetOutput(os.Stderr)

	// Register with the kind token: the server assigns (and returns) a stable
	// identity for this (kind, hostname, workdir) with an empty client_session,
	// plus an instance token. Codex threads register their own identities
	// later, on the first tool call that carries _meta.threadId. OpenCode,
	// Pi and DeepSeek keep using this identity.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	reg, err := client.Register(ctx, *hostname, *workdir)
	cancel()
	if err != nil {
		log.Fatalf("registration failed: %v", err)
	}
	client.SetToken(reg.Token)
	if reg.Replaced {
		log.Printf("registered as %s (kind %s, host %s, dir %s) — replaced previous instance", reg.Name, reg.Kind, reg.Hostname, reg.Workdir)
	} else {
		log.Printf("registered as %s (kind %s, host %s, dir %s)", reg.Name, reg.Kind, reg.Hostname, reg.Workdir)
	}

	statePath := *stateFile
	if statePath == "" {
		statePath = filepath.Join(*workdir, ".butler", "instance.json")
	}
	if err := mcpserver.WriteInstanceState(statePath, mcpserver.InstanceState{
		Agent:     reg.Name,
		Token:     reg.Token,
		Server:    *serverURL,
		Kind:      reg.Kind,
		PID:       os.Getpid(),
		PPID:      os.Getppid(),
		WrittenAt: time.Now().Unix(),
	}); err != nil {
		log.Printf("warning: state file %s not written: %v (local integrations such as the OpenCode relay plugin cannot act as this instance)", statePath, err)
	}

	srvOpts := (*mcp.ServerOptions)(nil)
	agent := mcpserver.New(client, reg.Name)
	if reg.Kind == "codex" {
		// Codex stamps threadId on every tools/call (not at process start,
		// and not via the environment). Bind each root thread to its own
		// identity so two sessions in one workdir do not share a token.
		agent.EnableCodexThreads(mcpserver.CodexThreadConfig{
			ServerURL: *serverURL,
			KindToken: *token,
			Hostname:  *hostname,
			Workdir:   *workdir,
			StateDir:  filepath.Join(*workdir, ".butler", "codex"),
		})
		srvOpts = &mcp.ServerOptions{Instructions: mcpserver.CodexInstructions}
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "butler", Version: version}, srvOpts)
	mcpserver.RegisterTools(srv, agent)
	if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Printf("server exited: %v", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func defaultHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

func defaultWorkdir() string {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return "/unknown"
	}
	return wd
}
