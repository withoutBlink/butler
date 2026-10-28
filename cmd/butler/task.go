package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"butler/internal/board"
	"butler/internal/store"
)

func taskCmd(args []string) {
	if len(args) == 0 {
		taskUsage()
		os.Exit(2)
	}
	sub := args[0]
	fs := flag.NewFlagSet("task "+sub, flag.ExitOnError)
	dataDir := fs.String("data-dir", envOr("BUTLER_DATA_DIR", defaultDir(".local/share/butler")), "data directory")
	status := fs.String("status", "", "filter or set status (todo, in_progress, blocked, review, done)")
	assignee := fs.String("assignee", "", "agent id, display name, or kind")
	session := fs.String("session", "", "linked session id")
	title := fs.String("title", "", "task title")
	description := fs.String("description", "", "task description")
	priority := fs.String("priority", "", "low, normal, high, or urgent")
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
		f := store.TaskFilter{Status: strings.TrimSpace(*status), SessionID: strings.TrimSpace(*session)}
		if f.Status != "" && !store.ValidTaskStatus(f.Status) {
			fmt.Fprintf(os.Stderr, "error: status must be one of todo, in_progress, blocked, review, done\n")
			os.Exit(1)
		}
		who := strings.TrimSpace(*assignee)
		switch who {
		case "":
		case "unassigned":
			f.Unassigned = true
		default:
			ag, err := st.ResolveAssignee(ctx, who)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			f.AssigneeID = ag.ID
		}
		tasks, err := st.ListTasks(ctx, f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		names := agentNames(ctx, st)
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tTITLE\tSTATUS\tPRIORITY\tASSIGNEE\tSESSION\tUPDATED")
		for _, t := range tasks {
			who := "-"
			if t.AssigneeID != "" {
				who = names[t.AssigneeID]
				if who == "" {
					who = t.AssigneeID
				}
			}
			sess := "-"
			if t.SessionID != "" {
				sess = t.SessionID
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Title, t.Status, t.Priority, who, sess,
				time.UnixMilli(t.UpdatedAt).UTC().Format(time.RFC3339))
		}
		w.Flush()
	case "show":
		if len(positional) == 0 {
			taskUsage()
			os.Exit(2)
		}
		t, err := st.GetTask(ctx, positional[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		names := agentNames(ctx, st)
		fmt.Printf("id:        %s\n", t.ID)
		fmt.Printf("title:     %s\n", t.Title)
		fmt.Printf("status:    %s\n", t.Status)
		fmt.Printf("priority:  %s\n", t.Priority)
		if t.AssigneeID == "" {
			fmt.Printf("assignee:  unassigned\n")
		} else {
			fmt.Printf("assignee:  %s\n", names[t.AssigneeID])
		}
		fmt.Printf("creator:   %s\n", names[t.CreatorID])
		if t.SessionID == "" {
			fmt.Printf("session:   none\n")
		} else {
			fmt.Printf("session:   %s\n", t.SessionID)
		}
		fmt.Printf("created:   %s\n", time.UnixMilli(t.CreatedAt).UTC().Format(time.RFC3339))
		fmt.Printf("updated:   %s\n", time.UnixMilli(t.UpdatedAt).UTC().Format(time.RFC3339))
		if t.Description != "" {
			fmt.Printf("\n%s\n", t.Description)
		}
		comments, err := st.ListTaskComments(ctx, t.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		if len(comments) == 0 {
			fmt.Println("\nno comments")
			return
		}
		fmt.Printf("\n%d comment(s):\n", len(comments))
		for _, c := range comments {
			who := names[c.AuthorID]
			if who == "" {
				who = c.AuthorID
			}
			fmt.Printf("  %s  %s\n  %s\n", time.UnixMilli(c.CreatedAt).UTC().Format(time.RFC3339), who, c.Body)
		}
	case "add":
		if strings.TrimSpace(*title) == "" {
			fmt.Fprintln(os.Stderr, "error: --title is required")
			os.Exit(1)
		}
		if err := ensureAdmin(ctx, st); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		// The running server is what fans the mention out to waiters; the
		// row and the message are written here, and webhooks are posted
		// before this command exits. A server that is up sees the message
		// on the assignee's next poll (same as `agent revoke`).
		if _, err := st.EnsureGlobalSession(ctx, "admin"); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		svc := board.New(st, nil, log)
		res, err := svc.Create(ctx, "admin", board.CreateInput{
			Title: *title, Description: *description, Status: *status,
			Priority: *priority, Assignee: *assignee, SessionID: *session,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		svc.Hooks.Wait()
		fmt.Printf("task %s created (%s", res.Task.ID, res.Task.Status)
		if res.Task.AssigneeID != "" {
			fmt.Printf(", assigned to %s", res.Task.AssigneeID)
		}
		fmt.Printf(")\n")
		if res.NotifySeq > 0 {
			fmt.Printf("notified in session via message seq %d\n", res.NotifySeq)
		}
	default:
		taskUsage()
		os.Exit(2)
	}
}

func webhookCmd(args []string) {
	if len(args) == 0 {
		webhookUsage()
		os.Exit(2)
	}
	sub := args[0]
	fs := flag.NewFlagSet("webhook "+sub, flag.ExitOnError)
	dataDir := fs.String("data-dir", envOr("BUTLER_DATA_DIR", defaultDir(".local/share/butler")), "data directory")
	agentID := fs.String("agent", "", "agent id")
	kindID := fs.String("kind", "", "kind id")
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
		hooks, err := st.ListWebhooks(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "SCOPE\tID\tURL\tSET")
		for _, h := range hooks {
			scope, id := "agent", h.AgentID
			if h.Kind != "" {
				scope, id = "kind", h.Kind
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", scope, id, h.URL, time.UnixMilli(h.CreatedAt).UTC().Format(time.RFC3339))
		}
		w.Flush()
	case "set":
		if (*agentID == "") == (*kindID == "") {
			fmt.Fprintln(os.Stderr, "error: set exactly one of --agent or --kind")
			os.Exit(1)
		}
		if len(positional) == 0 {
			webhookUsage()
			os.Exit(2)
		}
		raw := positional[0]
		if err := board.ValidWebhookURL(raw); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		if *agentID != "" {
			if _, err := st.SetAgentWebhook(ctx, *agentID, raw); err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("webhook set for agent %s\n", *agentID)
		} else {
			if _, err := st.SetKindWebhook(ctx, *kindID, raw); err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("webhook set for kind %s\n", *kindID)
		}
	case "delete":
		if (*agentID == "") == (*kindID == "") {
			fmt.Fprintln(os.Stderr, "error: set exactly one of --agent or --kind")
			os.Exit(1)
		}
		var err error
		if *agentID != "" {
			err = st.DeleteAgentWebhook(ctx, *agentID)
		} else {
			err = st.DeleteKindWebhook(ctx, *kindID)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("webhook deleted")
	default:
		webhookUsage()
		os.Exit(2)
	}
}

func agentNames(ctx context.Context, st *store.Store) map[string]string {
	names := map[string]string{}
	agents, err := st.ListAgents(ctx)
	if err != nil {
		return names
	}
	for _, a := range agents {
		if a.DisplayName != "" && a.DisplayName != a.ID {
			names[a.ID] = a.DisplayName + " (" + a.ID + ")"
		} else {
			names[a.ID] = a.ID
		}
	}
	return names
}

func ensureAdmin(ctx context.Context, st *store.Store) error {
	if _, err := st.GetAgent(ctx, "admin"); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	_, err := st.CreateAgent(ctx, "admin", "Admin", "human", "", "", "")
	return err
}

func taskUsage() {
	fmt.Fprint(os.Stderr, `usage (flags before or after the id):
  butler task list [--status S] [--assignee ID|name|kind|unassigned] [--session S]
  butler task show <id>
  butler task add --title T [--description D] [--assignee ID|name|kind] [--priority P] [--status S] [--session S]
`)
}

func webhookUsage() {
	fmt.Fprint(os.Stderr, `usage (flags before or after the url):
  butler webhook list
  butler webhook set --agent <id> <url>
  butler webhook set --kind <id> <url>
  butler webhook delete --agent <id>
  butler webhook delete --kind <id>
`)
}
