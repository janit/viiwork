package activity

import (
	"encoding/json"
	"sync"
	"testing"
)

// Subscribe closes the oldest subscriber's channel once it is at capacity, and
// emit sends to every subscriber. If emit sends to a snapshot taken under the
// lock and then released — as it once did — the two interleave and the send
// lands on a closed channel, which panics. A select's default case does not
// prevent that: only a closed *receive* is safe.
//
// Sixteen subscribers is not a stress figure. Each open dashboard is one, and
// the mesh stream opens one per connected client, so a handful of tabs reaches
// the cap on an ordinary day.
func TestEmitDoesNotPanicWhileSubscribersAreEvicted(t *testing.T) {
	for attempt := 0; attempt < 200; attempt++ {
		l := NewLog()
		for i := 0; i < maxSubscribers; i++ {
			l.Subscribe() // filled to capacity, and deliberately never drained
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				l.EmitRequest(int64(i), 0, "event %d", i)
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				l.Subscribe() // each one evicts an older subscriber
			}
		}()
		wg.Wait()
	}
}

func TestSubscribeEvictsOneAtCapacity(t *testing.T) {
	l := NewLog()
	subs := make([]chan []byte, 0, maxSubscribers)
	for i := 0; i < maxSubscribers; i++ {
		subs = append(subs, l.Subscribe())
	}
	if got := len(l.subscribers); got != maxSubscribers {
		t.Fatalf("holding %d subscribers, want %d", got, maxSubscribers)
	}

	l.Subscribe() // one over capacity
	if got := len(l.subscribers); got != maxSubscribers {
		t.Errorf("after overflow holding %d, want the cap %d", got, maxSubscribers)
	}

	// Exactly one of the originals must have been closed, which is how its
	// reader learns to stop. Which one is deliberately not asserted: the
	// implementation evicts an arbitrary map key, not the oldest.
	closed := 0
	for _, ch := range subs {
		select {
		case _, ok := <-ch:
			if !ok {
				closed++
			}
		default:
		}
	}
	if closed != 1 {
		t.Errorf("%d subscribers closed, want exactly 1", closed)
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	l := NewLog()
	ch := l.Subscribe()
	l.EmitRequest(1, 0, "before")
	if len(ch) != 1 {
		t.Fatalf("subscriber should have received 1 event, has %d", len(ch))
	}
	l.Unsubscribe(ch)
	l.EmitRequest(2, 0, "after")
	if len(ch) != 1 {
		t.Errorf("after Unsubscribe the channel should receive nothing more, has %d", len(ch))
	}
}

func TestBacklogMarksReplay(t *testing.T) {
	l := NewLog()
	l.EmitRequest(1, 0, "one")
	l.EmitRequest(2, 0, "two")
	b := l.Backlog()
	if len(b) != 2 {
		t.Fatalf("backlog has %d events, want 2", len(b))
	}
	for _, ev := range b {
		if !ev.Replay {
			t.Error("backlog events must be marked Replay: a consumer has to tell a replayed start from a live one, or it double-counts in-flight rows")
		}
	}
	// Recent is the same ring without the marking, and neither may hand out a
	// slice the caller can mutate into the log.
	if r := l.Recent(); len(r) != 2 || r[0].Replay {
		t.Error("Recent must not mark replay")
	}
	b[0].Message = "mutated"
	if l.Recent()[0].Message == "mutated" {
		t.Error("Backlog handed out the log's own backing array")
	}
}

func TestEventsRingIsBounded(t *testing.T) {
	l := NewLog()
	for i := 0; i < l.maxEvents+50; i++ {
		l.EmitRequest(int64(i), 0, "e%d", i)
	}
	if got := len(l.Recent()); got != l.maxEvents {
		t.Errorf("ring holds %d events, want it bounded at %d", got, l.maxEvents)
	}
	var ev Event
	if err := json.Unmarshal([]byte(`{"type":"request"}`), &ev); err != nil {
		t.Fatalf("Event must round-trip as JSON: %v", err)
	}
}
