package board

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const webhookTimeout = 5 * time.Second

// Hooks delivers outbound task webhooks. Post returns immediately — the
// HTTP call, and a single retry, run off the request path. Wait blocks
// until those calls finish (the CLI, and tests).
type Hooks struct {
	Log     *slog.Logger
	Client  *http.Client
	Timeout time.Duration

	mu sync.Mutex
	wg sync.WaitGroup
}

func newHooks(log *slog.Logger) *Hooks {
	return &Hooks{
		Log:     log,
		Timeout: webhookTimeout,
		Client: &http.Client{
			// A 3xx is a failed delivery, not a success via a redirected GET.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Post starts one delivery. It never blocks the caller. A non-2xx response
// or a transport error is retried once; the second failure is logged.
func (h *Hooks) Post(url string, body []byte) {
	if h == nil || url == "" || len(body) == 0 {
		return
	}
	payload := append([]byte(nil), body...)
	h.mu.Lock()
	h.wg.Add(1)
	h.mu.Unlock()
	go func() {
		defer h.wg.Done()
		if err := h.once(url, payload); err != nil {
			h.Log.Warn("webhook failed, retrying once", "url", url, "err", err)
			if err2 := h.once(url, payload); err2 != nil {
				h.Log.Error("webhook delivery failed", "url", url, "err", err2)
			}
		}
	}()
}

// Wait blocks until every Post started so far has finished.
func (h *Hooks) Wait() {
	if h == nil {
		return
	}
	h.mu.Lock()
	wg := &h.wg
	h.mu.Unlock()
	wg.Wait()
}

func (h *Hooks) once(url string, body []byte) error {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = webhookTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "butler-webhook")
	resp, err := h.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
