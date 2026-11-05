package hub

import (
	"sync"
	"testing"
	"time"
)

// TestConcurrentSubscribeCancelNotify hammers the three paths that touch the
// hub's subscriber maps at once: every long-poll wait and every SSE stream
// subscribes and then cancels from its own request goroutine while Notify
// broadcasts from others. The cancel used to delete from those maps without
// the hub lock, which the runtime answers with "concurrent map writes" — a
// fatal error, not a panic, so it cannot be recovered per request: the whole
// server dies (agents then see connection refused until someone restarts it).
// Cancel is also called twice here: it must stay a no-op once the key is gone.
func TestConcurrentSubscribeCancelNotify(t *testing.T) {
	h := New()
	defer h.Close()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, cancel := h.Subscribe("a")
				_, cancelSession := h.SubscribeSession("s")
				_, cancelBoard := h.SubscribeBoard()
				h.Notify([]string{"a", "b"})
				h.NotifySession("s")
				h.NotifyBoard()
				cancelBoard()
				cancelSession()
				cancel()
				cancel()
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestNotifyWakesSubscriber(t *testing.T) {
	h := New()
	ch, cancel := h.Subscribe("a")
	defer cancel()
	go func() {
		time.Sleep(10 * time.Millisecond)
		h.Notify([]string{"a"})
	}()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no wakeup")
	}
}

func TestNotifyUnrelated(t *testing.T) {
	h := New()
	ch, cancel := h.Subscribe("a")
	defer cancel()
	h.Notify([]string{"b"})
	select {
	case <-ch:
		t.Fatal("unexpected wakeup")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestNotifyNonBlocking(t *testing.T) {
	h := New()
	ch, cancel := h.Subscribe("a")
	defer cancel()
	h.Notify([]string{"a"})
	h.Notify([]string{"a"})
	h.Notify([]string{"a"})
	select {
	case <-ch:
	default:
		t.Fatal("expected buffered wakeup")
	}
	select {
	case <-ch:
		t.Fatal("expected extra notifications to be dropped")
	default:
	}
}

func TestCancel(t *testing.T) {
	h := New()
	ch, cancel := h.Subscribe("a")
	cancel()
	h.Notify([]string{"a"})
	select {
	case <-ch:
		t.Fatal("unexpected wakeup after cancel")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestCloseWakesSubscribers(t *testing.T) {
	h := New()
	ch, _ := h.Subscribe("a")
	h.Close()
	_, ok := <-ch
	if ok {
		t.Fatal("expected closed channel")
	}
	if !h.Closed() {
		t.Fatal("expected closed flag")
	}
}

func TestSessionSubscriptions(t *testing.T) {
	h := New()
	ch, cancel := h.SubscribeSession("s1")
	defer cancel()
	h.NotifySession("s2")
	select {
	case <-ch:
		t.Fatal("unexpected wakeup for other session")
	case <-time.After(50 * time.Millisecond):
	}
	h.NotifySession("s1")
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("expected session wakeup")
	}
	h.Notify([]string{"a"})
	select {
	case <-ch:
		t.Fatal("agent notify should not wake session subscriber")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSessionSubscriberWokenByClose(t *testing.T) {
	h := New()
	ch, _ := h.SubscribeSession("s1")
	h.Close()
	_, ok := <-ch
	if ok {
		t.Fatal("expected closed channel")
	}
}

func TestBoardSubscriber(t *testing.T) {
	h := New()
	ch, cancel := h.SubscribeBoard()
	defer cancel()
	h.NotifySession("s1")
	h.Notify([]string{"a"})
	select {
	case <-ch:
		t.Fatal("session and agent wakes must not wake the board")
	case <-time.After(50 * time.Millisecond):
	}
	h.NotifyBoard()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("expected board wakeup")
	}
}

func TestWaitingAgents(t *testing.T) {
	h := New()
	defer h.Close()
	if h.WaitingAgents()["a"] {
		t.Fatal("no waiter yet")
	}
	_, cancel := h.Subscribe("a")
	if !h.WaitingAgents()["a"] {
		t.Fatal("a subscribed long-poll should count as waiting")
	}
	cancel()
	if h.WaitingAgents()["a"] {
		t.Fatal("cancel should drop the waiter")
	}
}
