package hub

import (
	"testing"
	"time"
)

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
