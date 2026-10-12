package metrics

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

type Metrics struct {
	Requests       atomic.Int64
	MessagesPosted atomic.Int64
	ContextUpdates atomic.Int64
	WaitWakeups    atomic.Int64
	WaitTimeouts   atomic.Int64
	WaitersActive  atomic.Int64
}

func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# TYPE butler_http_requests_total counter\nbutler_http_requests_total %d\n", m.Requests.Load())
	fmt.Fprintf(w, "# TYPE butler_messages_posted_total counter\nbutler_messages_posted_total %d\n", m.MessagesPosted.Load())
	fmt.Fprintf(w, "# TYPE butler_context_updates_total counter\nbutler_context_updates_total %d\n", m.ContextUpdates.Load())
	fmt.Fprintf(w, "# TYPE butler_wait_wakeups_total counter\nbutler_wait_wakeups_total %d\n", m.WaitWakeups.Load())
	fmt.Fprintf(w, "# TYPE butler_wait_timeouts_total counter\nbutler_wait_timeouts_total %d\n", m.WaitTimeouts.Load())
	fmt.Fprintf(w, "# TYPE butler_waiters_active gauge\nbutler_waiters_active %d\n", m.WaitersActive.Load())
}
