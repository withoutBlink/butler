package hub

import "sync"

type Hub struct {
	mu          sync.Mutex
	agentSubs   map[string]map[chan struct{}]struct{}
	sessionSubs map[string]map[chan struct{}]struct{}
	closed      bool
}

func New() *Hub {
	return &Hub{
		agentSubs:   make(map[string]map[chan struct{}]struct{}),
		sessionSubs: make(map[string]map[chan struct{}]struct{}),
	}
}

// addSub registers a subscriber and returns its wake channel plus a cancel
// that removes it. The caller must hold h.mu: the maps are shared by every
// request goroutine (a long-poll wait or an SSE stream each subscribe and
// cancel), and Notify mutates them too. The cancel returned here closes over
// the same maps and is called later, from that request's goroutine, so it has
// to take the lock itself — an unlocked delete here races with Notify and with
// other cancels, and the runtime answers with "concurrent map writes", a fatal
// error that kills the whole server (it took the process down repeatedly:
// every agent then sees connection refused until someone restarts it).
func (h *Hub) addSub(subs map[string]map[chan struct{}]struct{}, key string) (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	if subs[key] == nil {
		subs[key] = make(map[chan struct{}]struct{})
	}
	subs[key][ch] = struct{}{}
	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if m, ok := subs[key]; ok {
			delete(m, ch)
			if len(m) == 0 {
				delete(subs, key)
			}
		}
	}
	return ch, cancel
}

func notifySubs(subs map[string]map[chan struct{}]struct{}, keys []string) {
	for _, key := range keys {
		for ch := range subs[key] {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

func (h *Hub) Subscribe(agentID string) (<-chan struct{}, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.addSub(h.agentSubs, agentID)
}

func (h *Hub) SubscribeSession(sessionID string) (<-chan struct{}, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.addSub(h.sessionSubs, sessionID)
}

func (h *Hub) Notify(agentIDs []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	notifySubs(h.agentSubs, agentIDs)
}

func (h *Hub) NotifySession(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	notifySubs(h.sessionSubs, []string{sessionID})
}

func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for key, subs := range h.agentSubs {
		for ch := range subs {
			close(ch)
		}
		delete(h.agentSubs, key)
	}
	for key, subs := range h.sessionSubs {
		for ch := range subs {
			close(ch)
		}
		delete(h.sessionSubs, key)
	}
}

func (h *Hub) Closed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}
