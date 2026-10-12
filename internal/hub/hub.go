package hub

import "sync"

type Hub struct {
	mu     sync.Mutex
	subs   map[string]map[chan struct{}]struct{}
	closed bool
}

func New() *Hub {
	return &Hub{subs: make(map[string]map[chan struct{}]struct{})}
}

func (h *Hub) Subscribe(agentID string) (<-chan struct{}, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan struct{}, 1)
	if h.subs[agentID] == nil {
		h.subs[agentID] = make(map[chan struct{}]struct{})
	}
	h.subs[agentID][ch] = struct{}{}
	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if subs, ok := h.subs[agentID]; ok {
			delete(subs, ch)
			if len(subs) == 0 {
				delete(h.subs, agentID)
			}
		}
	}
	return ch, cancel
}

func (h *Hub) Notify(agentIDs []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	for _, id := range agentIDs {
		for ch := range h.subs[id] {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for id, subs := range h.subs {
		for ch := range subs {
			close(ch)
		}
		delete(h.subs, id)
	}
}

func (h *Hub) Closed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}
