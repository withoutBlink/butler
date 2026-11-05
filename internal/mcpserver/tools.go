package mcpserver

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"butler/internal/store"
)

const defaultWaitSeconds = 20

type Server struct {
	client      *Client
	agentName   string // server-generated identity, used for @mentions
	mu          sync.Mutex
	lastSeq     int64
	initialized bool
}

func New(client *Client, agentName string) *Server {
	return &Server{client: client, agentName: agentName}
}

func addTool[In, Out any](mcpServer *mcp.Server, tool *mcp.Tool, h func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, Out, error)) {
	mcp.AddTool(mcpServer, tool, h)
}

func RegisterTools(mcpServer *mcp.Server, s *Server) {

	addTool(mcpServer, &mcp.Tool{
		Name:        "list_sessions",
		Description: "List Butler messaging sessions. The \"global\" session has every connected agent as a member — use it to reach everyone. Also shows your other joined sessions (with unread hints) and active sessions you can join. Use this first to discover where to send messages.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, any, error) {
		return s.listSessions(ctx)
	})

	addTool(mcpServer, &mcp.Tool{
		Name:        "create_session",
		Description: "Create a new messaging session for a task and optionally invite other agents by name (names are generated per agent, e.g. \"codex-laptop\" — find them in list_sessions/read_session participant lists; \"you\" is the human on the panel). You are added as a participant automatically. Returns the session id to use with the other tools.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Title        string   `json:"title" jsonschema:"short title for the task or conversation, required"`
		Participants []string `json:"participants,omitempty" jsonschema:"agent ids to invite"`
	}) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(args.Title) == "" {
			return errResult("title is required"), nil, nil
		}
		sess, err := s.client.CreateSession(ctx, args.Title, args.Participants)
		if err != nil {
			return errResult(err), nil, nil
		}
		return textResult(fmt.Sprintf("created session %q (%s)\nyou are a participant; others can join it via list_sessions + join_session", sess.Title, sess.ID)), nil, nil
	})

	addTool(mcpServer, &mcp.Tool{
		Name:        "join_session",
		Description: "Join a session so you can read and send messages in it. Idempotent. After joining, call read_session to catch up on history.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Session string `json:"session" jsonschema:"session id from list_sessions, required"`
	}) (*mcp.CallToolResult, any, error) {
		if args.Session == "" {
			return errResult("session is required"), nil, nil
		}
		joined, err := s.client.JoinSession(ctx, args.Session)
		if err != nil {
			return errResult(err), nil, nil
		}
		if joined {
			return textResult("joined session " + args.Session + "\ncall read_session to catch up on its history"), nil, nil
		}
		return textResult("already a participant of session " + args.Session), nil, nil
	})

	addTool(mcpServer, &mcp.Tool{
		Name:        "read_session",
		Description: "Read a session: title, participants (with presence) and the most recent messages (default 50, newest last). Use to catch up on a conversation or check shared context.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Session string `json:"session" jsonschema:"session id, required"`
		LastN   int    `json:"last_n,omitempty" jsonschema:"number of recent messages to return, default 50"`
	}) (*mcp.CallToolResult, any, error) {
		if args.Session == "" {
			return errResult("session is required"), nil, nil
		}
		n := args.LastN
		if n <= 0 {
			n = 50
		}
		if n > 500 {
			n = 500
		}
		detail, err := s.client.SessionDetail(ctx, args.Session)
		if err != nil {
			return errResult(err), nil, nil
		}
		msgs, err := s.client.SessionMessagesLatest(ctx, args.Session, n)
		if err != nil {
			return errResult(err), nil, nil
		}
		return textResult(s.formatSession(detail, msgs)), nil, nil
	})

	addTool(mcpServer, &mcp.Tool{
		Name:        "send_message",
		Description: "Send a chat message to a session. You must be a participant (join_session first). Address specific agents with @mentions: \"@<agent id>\" or \"@<display name>\" (the title list_sessions/read_session show; case-insensitive, spaces allowed) for one agent, \"@<kind>\" for every agent of that kind in the session, or \"@all\" for all participants. Mentioned agents are expected to reply; unmentioned participants only get the message as FYI. Returns the message sequence number.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Session string `json:"session" jsonschema:"session id, required"`
		Content string `json:"content" jsonschema:"message text (markdown), required"`
	}) (*mcp.CallToolResult, any, error) {
		if args.Session == "" {
			return errResult("session is required"), nil, nil
		}
		if strings.TrimSpace(args.Content) == "" {
			return errResult("content is required"), nil, nil
		}
		msg, err := s.client.PostMessage(ctx, args.Session, args.Content)
		if err != nil {
			return errResult(err), nil, nil
		}
		s.advanceCursor(msg.Seq)
		return textResult(fmt.Sprintf("sent message seq=%d", msg.Seq)), nil, nil
	})

	addTool(mcpServer, &mcp.Tool{
		Name:        "wait_for_messages",
		Description: "Wait for new messages in your joined sessions (long-poll, near real time). Blocks up to timeout_seconds (default 20, max 60), then reports no new messages. Call this repeatedly to listen for replies from other agents and humans — it returns immediately when a message arrives. Messages are grouped per session; ones that @-mention you are marked \"(mentioned you)\" and expect a reply, the rest are FYI you may ignore if not relevant. Etiquette: when you decide to act on a message, ack briefly first (send_message), then do the work and report back — don't go silent until done. It only sees messages newer than your cursor; use read_session for older history.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		TimeoutSeconds int `json:"timeout_seconds,omitempty" jsonschema:"how long to block, default 20, max 60"`
	}) (*mcp.CallToolResult, any, error) {
		timeout := args.TimeoutSeconds
		if timeout <= 0 {
			timeout = defaultWaitSeconds
		}
		if timeout > 60 {
			timeout = 60
		}
		after, err := s.cursor(ctx)
		if err != nil {
			return errResult(err), nil, nil
		}
		msgs, latest, err := s.client.Wait(ctx, after, timeout)
		if err != nil {
			return errResult(err), nil, nil
		}
		s.advanceCursor(latest)
		if len(msgs) == 0 {
			return textResult("no new messages"), nil, nil
		}
		return textResult(s.formatNotification(ctx, msgs)), nil, nil
	})

	addTool(mcpServer, &mcp.Tool{
		Name:        "check_messages",
		Description: "Fetch new messages in your joined sessions without blocking (unlike wait_for_messages). Messages that @-mention you are marked and expect a reply; the rest are FYI. Useful when you want a quick non-blocking check between other work.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, any, error) {
		after, err := s.cursor(ctx)
		if err != nil {
			return errResult(err), nil, nil
		}
		msgs, latest, err := s.client.Catchup(ctx, after)
		if err != nil {
			return errResult(err), nil, nil
		}
		s.advanceCursor(latest)
		if len(msgs) == 0 {
			return textResult("no new messages"), nil, nil
		}
		return textResult(s.formatNotification(ctx, msgs)), nil, nil
	})

	addTool(mcpServer, &mcp.Tool{
		Name:        "get_context",
		Description: "Read the shared context items (key/value notes) of a session: durable session-level facts like decisions, file paths, task state. Different from chat history — this is structured info all participants share.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Session string `json:"session" jsonschema:"session id, required"`
	}) (*mcp.CallToolResult, any, error) {
		if args.Session == "" {
			return errResult("session is required"), nil, nil
		}
		items, err := s.client.Context(ctx, args.Session)
		if err != nil {
			return errResult(err), nil, nil
		}
		if len(items) == 0 {
			return textResult("no context items set for this session"), nil, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d context item(s):\n", len(items))
		for _, item := range items {
			fmt.Fprintf(&b, "%s = %s\n", item.Key, item.Value)
		}
		return textResult(b.String()), nil, nil
	})

	addTool(mcpServer, &mcp.Tool{
		Name:        "set_context",
		Description: "Set or update a shared context item (key/value note) on a session, visible to all participants. Use for durable facts other agents should know: decisions, links, task state. Updates are announced in the session message stream.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Session string `json:"session" jsonschema:"session id, required"`
		Key     string `json:"key" jsonschema:"short key, e.g. decision_auth_lib or current_status"`
		Value   string `json:"value" jsonschema:"value text for the key"`
	}) (*mcp.CallToolResult, any, error) {
		if args.Session == "" {
			return errResult("session is required"), nil, nil
		}
		if strings.TrimSpace(args.Key) == "" || strings.TrimSpace(args.Value) == "" {
			return errResult("key and value are required"), nil, nil
		}
		_, notifySeq, err := s.client.PutContext(ctx, args.Session, args.Key, args.Value)
		if err != nil {
			return errResult(err), nil, nil
		}
		s.advanceCursor(notifySeq)
		return textResult(fmt.Sprintf("context %s updated", args.Key)), nil, nil
	})

	addTool(mcpServer, &mcp.Tool{
		Name:        "list_tasks",
		Description: "List tasks on the Butler board. Filter with assignee (an agent id, a display name, a kind, \"me\" for yourself, or \"unassigned\") and/or status (todo, in_progress, blocked, review, done). Use this to find work assigned to you.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Assignee string `json:"assignee,omitempty" jsonschema:"agent id, display name, kind, \"me\", or \"unassigned\""`
		Status   string `json:"status,omitempty" jsonschema:"todo, in_progress, blocked, review, or done"`
		Session  string `json:"session,omitempty" jsonschema:"only tasks linked to this session id"`
	}) (*mcp.CallToolResult, any, error) {
		tasks, err := s.client.ListTasks(ctx, args.Assignee, args.Status, args.Session)
		if err != nil {
			return errResult(err), nil, nil
		}
		return textResult(formatTaskList(tasks)), nil, nil
	})

	addTool(mcpServer, &mcp.Tool{
		Name:        "get_task",
		Description: "Read one task: title, description, status, priority, assignee, creator, linked session, and its comment log. Use the task id from list_tasks or from an assignment message.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Task string `json:"task" jsonschema:"task id, required"`
	}) (*mcp.CallToolResult, any, error) {
		if args.Task == "" {
			return errResult("task is required"), nil, nil
		}
		view, err := s.client.GetTask(ctx, args.Task)
		if err != nil {
			return errResult(err), nil, nil
		}
		return textResult(formatTask(view)), nil, nil
	})

	addTool(mcpServer, &mcp.Tool{
		Name:        "create_task",
		Description: "Create a task on the Butler board. assignee is optional and resolves like an @mention (agent id, display name, or kind). Assigning someone posts a message that @mentions them, so their wait_for_messages / relay wakes them. chain is the ordered handoff list (same token style); reviewer is who may mark it done (default: you, the creator). Link a session when the discussion should happen there.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Title       string   `json:"title" jsonschema:"short title, required"`
		Description string   `json:"description,omitempty" jsonschema:"longer description of the work"`
		Assignee    string   `json:"assignee,omitempty" jsonschema:"agent id, display name, or kind"`
		Status      string   `json:"status,omitempty" jsonschema:"todo, in_progress, blocked, review, or done; default todo"`
		Priority    string   `json:"priority,omitempty" jsonschema:"low, normal, high, or urgent; default normal"`
		Session     string   `json:"session,omitempty" jsonschema:"session id to link, optional"`
		Chain       []string `json:"chain,omitempty" jsonschema:"ordered owners, each an agent id, display name, or kind"`
		Reviewer    string   `json:"reviewer,omitempty" jsonschema:"who may mark the task done; default is you"`
	}) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(args.Title) == "" {
			return errResult("title is required"), nil, nil
		}
		view, err := s.client.CreateTask(ctx, args.Title, args.Description, args.Assignee, args.Status, args.Priority, args.Session, args.Reviewer, args.Chain)
		if err != nil {
			return errResult(err), nil, nil
		}
		s.advanceCursor(view.NotifySeq)
		who := "unassigned"
		if view.AssigneeID != "" {
			who = view.AssigneeID
		}
		return textResult(fmt.Sprintf("created task %q (%s)\nstatus: %s · priority: %s · assignee: %s", view.Title, view.ID, view.Status, view.Priority, who)), nil, nil
	})

	addTool(mcpServer, &mcp.Tool{
		Name:        "update_task_status",
		Description: "Move a task to a new status: todo, in_progress, blocked, review, or done. Only the reviewer (or the human admin) can set done — otherwise hand the task off for review and use approve_task. The creator (and the assignee, when that is someone else) is notified.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Task   string `json:"task" jsonschema:"task id, required"`
		Status string `json:"status" jsonschema:"todo, in_progress, blocked, review, or done, required"`
	}) (*mcp.CallToolResult, any, error) {
		if args.Task == "" || strings.TrimSpace(args.Status) == "" {
			return errResult("task and status are required"), nil, nil
		}
		view, err := s.client.UpdateTaskStatus(ctx, args.Task, args.Status)
		if err != nil {
			return errResult(err), nil, nil
		}
		s.advanceCursor(view.NotifySeq)
		return textResult(fmt.Sprintf("task %s (%s) status: %s", view.ID, view.Title, view.Status)), nil, nil
	})

	addTool(mcpServer, &mcp.Tool{
		Name:        "comment_task",
		Description: "Add a progress comment to a task. The creator and the assignee are notified (a mention in the task's session, or the global session). Use this to report what you did, what blocked you, or what you need reviewed.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Task string `json:"task" jsonschema:"task id, required"`
		Body string `json:"body" jsonschema:"comment text, required"`
	}) (*mcp.CallToolResult, any, error) {
		if args.Task == "" || strings.TrimSpace(args.Body) == "" {
			return errResult("task and body are required"), nil, nil
		}
		c, seq, err := s.client.CommentTask(ctx, args.Task, args.Body)
		if err != nil {
			return errResult(err), nil, nil
		}
		s.advanceCursor(seq)
		return textResult(fmt.Sprintf("comment recorded on task %s (%s)", c.TaskID, c.ID)), nil, nil
	})

	addTool(mcpServer, &mcp.Tool{
		Name:        "claim_task",
		Description: "Claim a task: it is assigned to you and moved to in_progress. Use this when you pick up unassigned work, or when you take over a task. The creator is notified. If it is already yours and in progress, this does nothing.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Task string `json:"task" jsonschema:"task id, required"`
	}) (*mcp.CallToolResult, any, error) {
		if args.Task == "" {
			return errResult("task is required"), nil, nil
		}
		view, err := s.client.ClaimTask(ctx, args.Task)
		if err != nil {
			return errResult(err), nil, nil
		}
		s.advanceCursor(view.NotifySeq)
		return textResult(fmt.Sprintf("claimed task %q (%s)\nstatus: %s · assignee: %s", view.Title, view.ID, view.Status, view.AssigneeID)), nil, nil
	})

	addTool(mcpServer, &mcp.Tool{
		Name:        "handoff_task",
		Description: "Hand a task you own to the next agent. Omit `to` to follow the chain (the next owner, or the reviewer once the chain is done — that step sets status review). Pass `to` to name the next owner directly (id, display name, or kind). Status becomes todo for a new owner, or review when the task reaches its reviewer. A log line is recorded, the new owner is @mentioned, and their webhook fires as task.assigned with handoff true. Only the current owner can call this.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Task string `json:"task" jsonschema:"task id, required"`
		To   string `json:"to,omitempty" jsonschema:"next owner; omit to take the next chain step"`
		Note string `json:"note,omitempty" jsonschema:"optional note recorded in the log"`
	}) (*mcp.CallToolResult, any, error) {
		if args.Task == "" {
			return errResult("task is required"), nil, nil
		}
		view, err := s.client.HandoffTask(ctx, args.Task, args.To, args.Note)
		if err != nil {
			return errResult(err), nil, nil
		}
		s.advanceCursor(view.NotifySeq)
		who := "unassigned"
		if view.AssigneeID != "" {
			who = view.AssigneeID
		}
		return textResult(fmt.Sprintf("handed off task %q (%s)\nstatus: %s · owner: %s", view.Title, view.ID, view.Status, who)), nil, nil
	})

	addTool(mcpServer, &mcp.Tool{
		Name:        "approve_task",
		Description: "Mark a task done. Only the reviewer (or the human admin) can do this. If you are the owner and the work is ready, handoff_task instead so the reviewer can approve it.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Task string `json:"task" jsonschema:"task id, required"`
	}) (*mcp.CallToolResult, any, error) {
		if args.Task == "" {
			return errResult("task is required"), nil, nil
		}
		view, err := s.client.ApproveTask(ctx, args.Task)
		if err != nil {
			return errResult(err), nil, nil
		}
		s.advanceCursor(view.NotifySeq)
		return textResult(fmt.Sprintf("approved task %q (%s)\nstatus: %s", view.Title, view.ID, view.Status)), nil, nil
	})

	addTool(mcpServer, &mcp.Tool{
		Name:        "reject_task",
		Description: "Send a task in review back to an owner. Omit `to` to return it to the owner who handed it in (status becomes todo). Pass `to` to name someone else. Only the reviewer can call this. A log line is recorded and the new owner is @mentioned.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Task string `json:"task" jsonschema:"task id, required"`
		To   string `json:"to,omitempty" jsonschema:"who receives it back; omit for the previous owner"`
		Note string `json:"note,omitempty" jsonschema:"why it is coming back"`
	}) (*mcp.CallToolResult, any, error) {
		if args.Task == "" {
			return errResult("task is required"), nil, nil
		}
		view, err := s.client.RejectTask(ctx, args.Task, args.To, args.Note)
		if err != nil {
			return errResult(err), nil, nil
		}
		s.advanceCursor(view.NotifySeq)
		return textResult(fmt.Sprintf("sent back task %q (%s)\nstatus: %s · owner: %s", view.Title, view.ID, view.Status, view.AssigneeID)), nil, nil
	})

	addTool(mcpServer, &mcp.Tool{
		Name:        "set_activity",
		Description: "Report what you are doing right now: working, idle, or blocked (waiting on a review or a human). Add a short note when it helps (\"waiting on the diff\"). Butler shows that next to your name on the board and in chat. This does not comment on a task and does not notify anyone — call it whenever your state changes. If you skip it, Butler infers activity from your calls and from tasks you own. A claimed in_progress task whose owner goes quiet is marked stalled on its own.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		State string `json:"state" jsonschema:"working, idle, or blocked, required"`
		Note  string `json:"note,omitempty" jsonschema:"optional short note, max 200 characters"`
	}) (*mcp.CallToolResult, any, error) {
		state := strings.TrimSpace(args.State)
		if state == "" {
			return errResult("state is required"), nil, nil
		}
		view, err := s.client.SetActivity(ctx, state, args.Note)
		if err != nil {
			return errResult(err), nil, nil
		}
		line := "activity: " + view.State
		if view.Note != "" {
			line += " (" + view.Note + ")"
		}
		return textResult(line), nil, nil
	})

	addTool(mcpServer, &mcp.Tool{
		Name:        "rename_self",
		Description: "Change your own Butler display name (the name others @mention and assign tasks to). Call this whenever your human/owner or a teammate tells you that you have been renamed, so Butler matches your new name, e.g. \"you are now called Point\" → rename_self {display_name: \"Point\"}. Your agent id, tokens, sessions, tasks and history stay the same; every room you are in gets a notice \"Old is now New\". The old name stops resolving for @mentions and assignees right away. Fails with a conflict if another agent already uses the name (case-insensitive) or it equals an agent kind — pick another or ask your owner. Only renames you; never use it to rename someone else.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		DisplayName string `json:"display_name" jsonschema:"your new display name, required (max 200 chars, must not start with @)"`
	}) (*mcp.CallToolResult, any, error) {
		name := strings.TrimSpace(args.DisplayName)
		if name == "" {
			return errResult("display_name is required"), nil, nil
		}
		res, err := s.client.RenameSelf(ctx, name)
		if err != nil {
			return errResult(err), nil, nil
		}
		if res.Previous == res.DisplayName {
			return textResult(fmt.Sprintf("your display name is already %q (id %s); nothing changed", res.DisplayName, res.ID)), nil, nil
		}
		return textResult(fmt.Sprintf("renamed %q → %q (id %s unchanged)\nothers can now @mention you as @%s; the old name no longer resolves", res.Previous, res.DisplayName, res.ID, res.DisplayName)), nil, nil
	})
}

func formatTaskList(tasks []ListedTask) string {
	if len(tasks) == 0 {
		return "no tasks"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d task(s):\n", len(tasks))
	for _, t := range tasks {
		who := "-"
		if t.AssigneeID != "" {
			who = t.AssigneeID
		}
		extra := ""
		if t.AssigneeActivity != "" {
			extra += "  activity=" + t.AssigneeActivity
		}
		if t.Stalled {
			extra += "  STALLED"
		}
		fmt.Fprintf(&b, "  %s  [%s]  [%s]  assignee=%s%s  %q\n", t.ID, t.Status, t.Priority, who, extra, t.Title)
	}
	return b.String()
}

func formatTask(view *TaskView) string {
	var b strings.Builder
	who := "unassigned"
	if view.AssigneeID != "" {
		who = view.AssigneeID
	}
	session := "none"
	if view.SessionID != "" {
		session = view.SessionID
	}
	fmt.Fprintf(&b, "task %q (%s)\n", view.Title, view.ID)
	fmt.Fprintf(&b, "status: %s\npriority: %s\nowner: %s\ncreator: %s\nreviewer: %s\nsession: %s\n", view.Status, view.Priority, who, view.CreatorID, reviewerOf(view), session)
	if view.AssigneeActivity != "" {
		fmt.Fprintf(&b, "activity: %s\n", view.AssigneeActivity)
	}
	if view.Stalled {
		b.WriteString("stalled: yes\n")
	}
	if len(view.Chain) > 0 {
		fmt.Fprintf(&b, "chain: %s\n", strings.Join(view.Chain, " → "))
	}
	if view.Description != "" {
		fmt.Fprintf(&b, "\n%s\n", view.Description)
	}
	if len(view.Comments) == 0 {
		b.WriteString("\nno comments\n")
		return b.String()
	}
	fmt.Fprintf(&b, "\n%d comment(s):\n", len(view.Comments))
	for _, c := range view.Comments {
		ts := time.UnixMilli(c.CreatedAt).UTC().Format("15:04:05")
		fmt.Fprintf(&b, "  [%s] %s: %s\n", ts, c.AuthorID, c.Body)
	}
	return b.String()
}

func reviewerOf(view *TaskView) string {
	if view.ReviewerID != "" {
		return view.ReviewerID
	}
	if view.CreatorID != "" {
		return view.CreatorID + " (creator)"
	}
	return "creator"
}

func (s *Server) listSessions(ctx context.Context) (*mcp.CallToolResult, any, error) {
	joined, err := s.client.Sessions(ctx, true)
	if err != nil {
		return errResult(err), nil, nil
	}
	all, err := s.client.Sessions(ctx, false)
	if err != nil {
		return errResult(err), nil, nil
	}
	joinedSet := map[string]bool{}
	for _, sess := range joined {
		joinedSet[sess.ID] = true
	}
	other := []store.Session{}
	for _, sess := range all {
		if sess.Status == "active" && !joinedSet[sess.ID] {
			other = append(other, sess)
		}
	}
	sort.Slice(joined, func(i, j int) bool { return joined[i].CreatedAt > joined[j].CreatedAt })
	var b strings.Builder
	fmt.Fprintf(&b, "you are %s\n", s.agentName)
	if len(joined) == 0 {
		fmt.Fprintln(&b, "you have not joined any sessions; create one with create_session or join one below")
	} else {
		fmt.Fprintf(&b, "your sessions (%d):\n", len(joined))
		for _, sess := range joined {
			fmt.Fprintf(&b, "  %s  %q  [%s]\n", sess.ID, sess.Title, sess.Status)
		}
	}
	if len(other) > 0 {
		fmt.Fprintf(&b, "other active sessions you can join (%d):\n", len(other))
		for _, sess := range other {
			fmt.Fprintf(&b, "  %s  %q\n", sess.ID, sess.Title)
		}
	}
	return textResult(b.String()), nil, nil
}

func (s *Server) cursor(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.initialized {
		n, err := s.client.LatestSeq(ctx)
		if err != nil {
			return 0, err
		}
		s.lastSeq = n
		s.initialized = true
	}
	return s.lastSeq, nil
}

func (s *Server) advanceCursor(seq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq > s.lastSeq {
		s.lastSeq = seq
	}
}

func (s *Server) formatSession(detail *SessionDetail, msgs []store.Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "session %q (%s) [%s]\ncreated by %s\n", detail.Session.Title, detail.Session.ID, detail.Session.Status, detail.Session.CreatedBy)
	names := make([]string, 0, len(detail.Participants))
	for _, p := range detail.Participants {
		tag := p.ID
		if p.Hostname != "" {
			tag += " (" + p.Kind + "@" + p.Hostname + ")"
		} else if p.Kind != "" {
			tag += " (" + p.Kind + ")"
		}
		if p.Online {
			tag += "*"
		}
		if p.Activity.State != "" {
			tag += " [" + p.Activity.State + "]"
		}
		names = append(names, tag)
	}
	fmt.Fprintf(&b, "participants (online*): %s\n", strings.Join(names, ", "))
	if len(msgs) == 0 {
		b.WriteString("no messages yet\n")
		return b.String()
	}
	fmt.Fprintf(&b, "last %d message(s):\n", len(msgs))
	b.WriteString(s.formatMessages(msgs))
	return b.String()
}

func (s *Server) formatMessages(msgs []store.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&b, "%s\n", s.formatMessageLine(m))
	}
	return b.String()
}

// formatMessageLine renders one message as "[seq time] sender: content".
// Messages that @-mention this agent carry a "(mentioned you)" marker: they
// are addressed to it and expect a reply; unmarked messages are FYI.
func (s *Server) formatMessageLine(m store.Message) string {
	ts := time.UnixMilli(m.CreatedAt).UTC().Format("15:04:05")
	sender := m.SenderID
	if m.Kind != "chat" {
		sender += " (" + m.Kind + ")"
	}
	if m.MentionsAgent(s.agentName) {
		sender += " (mentioned you)"
	}
	return fmt.Sprintf("[%d %s] %s: %s", m.Seq, ts, sender, m.Content)
}

// formatNotification renders a wait/check batch: a summary line splitting
// messages that mention this agent (reply expected) from FYI, then the
// messages grouped per session — with titles when known — so the agent knows
// where to reply.
func (s *Server) formatNotification(ctx context.Context, msgs []store.Message) string {
	titles := map[string]string{}
	if sessions, err := s.client.Sessions(ctx, true); err == nil {
		for _, sess := range sessions {
			titles[sess.ID] = sess.Title
		}
	}
	mentions := 0
	for _, m := range msgs {
		if m.MentionsAgent(s.agentName) {
			mentions++
		}
	}
	fyis := len(msgs) - mentions

	var order []string
	groups := map[string][]store.Message{}
	for _, m := range msgs {
		if _, ok := groups[m.SessionID]; !ok {
			order = append(order, m.SessionID)
		}
		groups[m.SessionID] = append(groups[m.SessionID], m)
	}

	var b strings.Builder
	switch {
	case mentions > 0 && fyis > 0:
		fmt.Fprintf(&b, "%d new message(s): %d @-mention you (reply expected), %d are FYI (no reply needed unless relevant)\n", len(msgs), mentions, fyis)
	case mentions > 0:
		fmt.Fprintf(&b, "%d new message(s): all @-mention you (reply expected)\n", len(msgs))
	default:
		fmt.Fprintf(&b, "%d new message(s): none @-mention you — FYI, no reply needed unless relevant\n", len(msgs))
	}
	fmt.Fprintf(&b, "If you decide to act on any of these, ack briefly right away (send_message, e.g. \"on it\"), then do the work and report back — don't stay silent until finished; people are watching for a response.\n")
	for _, sid := range order {
		if t := titles[sid]; t != "" {
			fmt.Fprintf(&b, "\n— session %q (%s):\n", t, sid)
		} else {
			fmt.Fprintf(&b, "\n— session %s:\n", sid)
		}
		for _, m := range groups[sid] {
			fmt.Fprintf(&b, "%s\n", s.formatMessageLine(m))
		}
	}
	return b.String()
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

func errResult(v any) *mcp.CallToolResult {
	var text string
	switch e := v.(type) {
	case *Error:
		text = fmt.Sprintf("butler error (%d): %s", e.StatusCode, e.Body)
	case error:
		text = "butler error: " + e.Error()
	default:
		text = fmt.Sprintf("butler error: %v", v)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
		IsError: true,
	}
}
