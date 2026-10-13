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

func addSub(subs map[string]map[chan struct{}]struct{}, key string) (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	if subs[key] == nil {
		subs[key] = make(map[chan struct{}]struct{})
	}
	subs[key][ch] = struct{}{}
	cancel := func() {
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
	ch, cancel := addSub(h.agentSubs, agentID)
	return ch, cancel
}

func (h *Hub) SubscribeSession(sessionID string) (<-chan struct{}, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, cancel := addSub(h.sessionSubs, sessionID)
	return ch, cancel
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
