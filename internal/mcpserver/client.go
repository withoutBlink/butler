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
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Kind        string `json:"kind"`
	Online      bool   `json:"online"`
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
