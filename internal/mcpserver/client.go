package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"butler/internal/store"
)

type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

type Error struct {
	StatusCode int
	Body       string
}

func (e *Error) Error() string {
	return fmt.Sprintf("butler: %d: %s", e.StatusCode, e.Body)
}

func NewClient(serverURL, token string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(serverURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse server url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("server url must be http or https")
	}
	return &Client{
		base:  u,
		token: token,
		http:  &http.Client{Timeout: 70 * time.Second},
	}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	u, err := url.Parse(strings.TrimRight(c.base.String(), "/") + path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		msg := string(raw)
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return &Error{StatusCode: resp.StatusCode, Body: msg}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func (c *Client) Sessions(ctx context.Context, joinedOnly bool) ([]store.Session, error) {
	path := "/api/v1/sessions"
	if joinedOnly {
		path += "?joined=1"
	}
	var resp struct {
		Sessions []store.Session `json:"sessions"`
	}
	if err := c.do(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

type SessionDetail struct {
	Session      store.Session `json:"session"`
	Participants []AgentInfo   `json:"participants"`
}

type AgentInfo struct {
	ID          string             `json:"id"`
	DisplayName string             `json:"display_name"`
	Kind        string             `json:"kind"`
	Hostname    string             `json:"hostname"`
	Workdir     string             `json:"workdir"`
	Online      bool               `json:"online"`
	Activity    store.ActivityView `json:"activity"`
}

type RegisterResult struct {
	Name     string `json:"name"`
	Token    string `json:"token"`
	Kind     string `json:"kind"`
	Hostname string `json:"hostname"`
	Workdir  string `json:"workdir"`
	Replaced bool   `json:"replaced"`
}

// Register exchanges a kind token plus hostname and working directory for a
// stable agent name and a fresh instance token. When the identity already
// existed, the server replaces the previous instance (its token is revoked)
// and Replaced is true. The caller must SetToken with the returned token
// before issuing other requests.
func (c *Client) Register(ctx context.Context, hostname, workdir string) (*RegisterResult, error) {
	body := map[string]string{"hostname": hostname, "workdir": workdir}
	var res RegisterResult
	if err := c.do(ctx, "POST", "/api/v1/register", body, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// SetToken swaps the credential used on every request: the kind token during
// registration, the instance token afterwards. Call it before serving tools
// (do() reads the token on every request).
func (c *Client) SetToken(token string) {
	c.token = token
}

func (c *Client) SessionDetail(ctx context.Context, sid string) (*SessionDetail, error) {
	var resp SessionDetail
	if err := c.do(ctx, "GET", "/api/v1/sessions/"+url.PathEscape(sid), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) CreateSession(ctx context.Context, title string, participants []string) (*store.Session, error) {
	body := map[string]any{"title": title}
	if len(participants) > 0 {
		body["participants"] = participants
	}
	var sess store.Session
	if err := c.do(ctx, "POST", "/api/v1/sessions", body, &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

func (c *Client) JoinSession(ctx context.Context, sid string) (bool, error) {
	var resp struct {
		Joined bool `json:"joined"`
	}
	if err := c.do(ctx, "POST", "/api/v1/sessions/"+url.PathEscape(sid)+"/join", nil, &resp); err != nil {
		return false, err
	}
	return resp.Joined, nil
}

type messageList struct {
	Messages  []store.Message `json:"messages"`
	LatestSeq int64           `json:"latest_seq"`
}

func (c *Client) SessionMessagesLatest(ctx context.Context, sid string, n int) ([]store.Message, error) {
	var resp messageList
	path := "/api/v1/sessions/" + url.PathEscape(sid) + "/messages?latest=" + strconv.Itoa(n)
	if err := c.do(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Messages, nil
}

func (c *Client) PostMessage(ctx context.Context, sid, content string) (*store.Message, error) {
	body := map[string]string{"content": content}
	var msg store.Message
	if err := c.do(ctx, "POST", "/api/v1/sessions/"+url.PathEscape(sid)+"/messages", body, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

func (c *Client) Wait(ctx context.Context, after int64, timeoutSecs int) ([]store.Message, int64, error) {
	var resp messageList
	path := "/api/v1/wait?after=" + strconv.FormatInt(after, 10) + "&timeout=" + strconv.Itoa(timeoutSecs)
	if err := c.do(ctx, "GET", path, nil, &resp); err != nil {
		return nil, 0, err
	}
	return resp.Messages, resp.LatestSeq, nil
}

func (c *Client) Catchup(ctx context.Context, after int64) ([]store.Message, int64, error) {
	var resp messageList
	path := "/api/v1/messages?after=" + strconv.FormatInt(after, 10)
	if err := c.do(ctx, "GET", path, nil, &resp); err != nil {
		return nil, 0, err
	}
	return resp.Messages, resp.LatestSeq, nil
}

func (c *Client) LatestSeq(ctx context.Context) (int64, error) {
	var resp struct {
		LatestSeq int64 `json:"latest_seq"`
	}
	if err := c.do(ctx, "GET", "/api/v1/seq", nil, &resp); err != nil {
		return 0, err
	}
	return resp.LatestSeq, nil
}

func (c *Client) Context(ctx context.Context, sid string) ([]store.ContextItem, error) {
	var resp struct {
		Items []store.ContextItem `json:"items"`
	}
	if err := c.do(ctx, "GET", "/api/v1/sessions/"+url.PathEscape(sid)+"/context", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

func (c *Client) PutContext(ctx context.Context, sid, key, value string) (*store.ContextItem, int64, error) {
	body := map[string]string{"value": value}
	var resp struct {
		store.ContextItem
		NotifySeq int64 `json:"notify_seq"`
	}
	path := "/api/v1/sessions/" + url.PathEscape(sid) + "/context/" + url.PathEscape(key)
	if err := c.do(ctx, "PUT", path, body, &resp); err != nil {
		return nil, 0, err
	}
	return &resp.ContextItem, resp.NotifySeq, nil
}

// TaskView is a task as the API returns it from a write or a get: the task
// itself, its comment log (GET), and the seq of the mention that announced
// the write (so the caller's wait cursor can skip its own notice).
type TaskView struct {
	store.Task
	Comments         []store.TaskComment `json:"comments"`
	NotifySeq        int64               `json:"notify_seq,omitempty"`
	Stalled          bool                `json:"stalled"`
	AssigneeActivity string              `json:"assignee_activity,omitempty"`
}

// ListedTask is one row of GET /api/v1/tasks, including the derived stall
// flag and the owner's activity. Those fields are absent on an older server.
type ListedTask struct {
	store.Task
	Stalled          bool   `json:"stalled"`
	AssigneeActivity string `json:"assignee_activity,omitempty"`
}

func (c *Client) ListTasks(ctx context.Context, assignee, status, session string) ([]ListedTask, error) {
	q := url.Values{}
	if assignee != "" {
		q.Set("assignee", assignee)
	}
	if status != "" {
		q.Set("status", status)
	}
	if session != "" {
		q.Set("session", session)
	}
	path := "/api/v1/tasks"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var resp struct {
		Tasks []ListedTask `json:"tasks"`
	}
	if err := c.do(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	if resp.Tasks == nil {
		resp.Tasks = []ListedTask{}
	}
	return resp.Tasks, nil
}

// SetActivity reports the caller's own working, idle, or blocked state.
// The server does not notify anyone; it only updates the indicator.
func (c *Client) SetActivity(ctx context.Context, state, note string) (store.ActivityView, error) {
	var resp struct {
		Activity store.ActivityView `json:"activity"`
	}
	if err := c.do(ctx, "POST", "/api/v1/agents/activity", map[string]string{"state": state, "note": note}, &resp); err != nil {
		return store.ActivityView{}, err
	}
	return resp.Activity, nil
}

func (c *Client) GetTask(ctx context.Context, id string) (*TaskView, error) {
	var out TaskView
	if err := c.do(ctx, "GET", "/api/v1/tasks/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	if out.Comments == nil {
		out.Comments = []store.TaskComment{}
	}
	return &out, nil
}

func (c *Client) CreateTask(ctx context.Context, title, description, assignee, status, priority, session, reviewer string, chain []string) (*TaskView, error) {
	body := map[string]any{
		"title": title, "description": description, "assignee": assignee,
		"status": status, "priority": priority, "session_id": session,
		"reviewer": reviewer,
	}
	if len(chain) > 0 {
		body["chain"] = chain
	}
	var out TaskView
	if err := c.do(ctx, "POST", "/api/v1/tasks", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) UpdateTaskStatus(ctx context.Context, id, status string) (*TaskView, error) {
	var out TaskView
	body := map[string]string{"status": status}
	if err := c.do(ctx, "PATCH", "/api/v1/tasks/"+url.PathEscape(id), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CommentTask(ctx context.Context, id, body string) (*store.TaskComment, int64, error) {
	var out struct {
		store.TaskComment
		NotifySeq int64 `json:"notify_seq"`
	}
	if err := c.do(ctx, "POST", "/api/v1/tasks/"+url.PathEscape(id)+"/comments", map[string]string{"body": body}, &out); err != nil {
		return nil, 0, err
	}
	return &out.TaskComment, out.NotifySeq, nil
}

func (c *Client) HandoffTask(ctx context.Context, id, to, note string) (*TaskView, error) {
	var out TaskView
	body := map[string]string{"to": to, "note": note}
	if err := c.do(ctx, "POST", "/api/v1/tasks/"+url.PathEscape(id)+"/handoff", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ApproveTask(ctx context.Context, id string) (*TaskView, error) {
	var out TaskView
	if err := c.do(ctx, "POST", "/api/v1/tasks/"+url.PathEscape(id)+"/approve", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) RejectTask(ctx context.Context, id, to, note string) (*TaskView, error) {
	var out TaskView
	body := map[string]string{"to": to, "note": note}
	if err := c.do(ctx, "POST", "/api/v1/tasks/"+url.PathEscape(id)+"/reject", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ClaimTask(ctx context.Context, id string) (*TaskView, error) {
	var out TaskView
	if err := c.do(ctx, "POST", "/api/v1/tasks/"+url.PathEscape(id)+"/claim", map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RenameResult is the rename endpoint's echo.
type RenameResult struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Previous    string `json:"previous"`
}

// RenameSelf changes the caller's own display name (POST /api/v1/agents/rename
// with only display_name). The id never changes.
func (c *Client) RenameSelf(ctx context.Context, displayName string) (*RenameResult, error) {
	var out RenameResult
	if err := c.do(ctx, "POST", "/api/v1/agents/rename", map[string]string{"display_name": displayName}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
