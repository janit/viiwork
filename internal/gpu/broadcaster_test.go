package gpu

import "testing"

func TestBroadcasterSubscribe(t *testing.T) {
	b := NewBroadcaster()
	ch := b.Subscribe()
	if ch == nil { t.Fatal("expected non-nil channel") }
	if cap(ch) != 4 { t.Errorf("expected cap 4, got %d", cap(ch)) }
}

func TestBroadcasterBroadcast(t *testing.T) {
	b := NewBroadcaster()
	ch := b.Subscribe()
	b.Broadcast([]byte("hello"))
	msg := <-ch
	if string(msg) != "hello" { t.Errorf("expected hello, got %s", msg) }
}

func TestBroadcasterUnsubscribe(t *testing.T) {
	b := NewBroadcaster()
	ch := b.Subscribe()
	b.Unsubscribe(ch)
	b.Broadcast([]byte("after unsub"))
	select {
	case <-ch:
		t.Error("should not receive after unsubscribe")
	default:
	}
}

func TestBroadcasterSkipsSlowClient(t *testing.T) {
	b := NewBroadcaster()
	ch := b.Subscribe()
	for range 4 { b.Broadcast([]byte("msg")) }
	b.Broadcast([]byte("overflow"))
	if len(ch) != 4 { t.Errorf("expected 4 buffered, got %d", len(ch)) }
}

func TestBroadcasterClose(t *testing.T) {
	b := NewBroadcaster()
	ch := b.Subscribe()
	b.Close()
	_, ok := <-ch
	if ok { t.Error("expected channel to be closed") }
}
