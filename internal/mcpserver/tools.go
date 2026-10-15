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
		Description: "Send a chat message to a session. You must be a participant (join_session first). Address specific agents with @mentions: \"@<agent id>\" for one agent, \"@<kind>\" for every agent of that kind in the session, or \"@all\" for all participants. Mentioned agents are expected to reply; unmentioned participants only get the message as FYI. Returns the message sequence number.",
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
