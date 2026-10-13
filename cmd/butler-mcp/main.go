package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"butler/internal/mcpserver"
)

const version = "0.1.0"

func main() {
	fs := flag.NewFlagSet("butler-mcp", flag.ExitOnError)
	serverURL := fs.String("server", envOr("BUTLER_SERVER", "http://127.0.0.1:8787"), "butler server URL")
	token := fs.String("token", envOr("BUTLER_TOKEN", ""), "kind token (the single token printed by `butler-server kind add <kind>`)")
	hostname := fs.String("hostname", envOr("BUTLER_HOSTNAME", defaultHostname()), "hostname reported to the butler server")
	workdir := fs.String("workdir", envOr("BUTLER_WORKDIR", defaultWorkdir()), "working directory reported to the butler server")
	fs.Parse(os.Args[1:])
	if *token == "" {
		fmt.Fprintln(os.Stderr, "butler-mcp: --token (or BUTLER_TOKEN) is required — pass the kind token printed by `butler-server kind add <kind>`")
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
	// identity name for this (kind, hostname, workdir) triple and an instance
	// token used for everything that follows.
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

	srv := mcp.NewServer(&mcp.Implementation{Name: "butler", Version: version}, nil)
	mcpserver.RegisterTools(srv, mcpserver.New(client, reg.Name))
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
