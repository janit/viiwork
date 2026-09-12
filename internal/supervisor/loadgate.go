package supervisor

import (
	"context"
	"sync"
)

// loadGate serializes model loads across the whole node: one backend loads at
// a time, as v1's StartAll did, because concurrent loads thrash disk I/O.
// Tickets are granted in enqueue order. Enqueueing is synchronous and waiting
// separate, which lets the node supervisor fix the load order before any loop
// goroutine runs. Respawns do not take a ticket (Decision 5).
type loadGate struct {
	mu     sync.Mutex
	holder *loadTicket
	queue  []*loadTicket
}

type loadTicket struct {
	gate    *loadGate
	granted chan struct{} // closed when the ticket holds the gate
	done    bool          // released or left the queue
}

func newLoadGate() *loadGate { return &loadGate{} }

// enqueue joins the queue now; the ticket is granted at once if the gate is free.
func (g *loadGate) enqueue() *loadTicket {
	t := &loadTicket{gate: g, granted: make(chan struct{})}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.queue = append(g.queue, t)
	g.grantLocked()
	return t
}

func (g *loadGate) grantLocked() {
	if g.holder != nil || len(g.queue) == 0 {
		return
	}
	g.holder = g.queue[0]
	g.queue = g.queue[1:]
	close(g.holder.granted)
}

// wait returns nil once the ticket holds the gate. If ctx ends first it
// returns the context's error and the ticket leaves the queue, passing the
// gate on if it had just been granted.
func (t *loadTicket) wait(ctx context.Context) error {
	select {
	case <-t.granted:
		return nil
	case <-ctx.Done():
		t.release()
		return ctx.Err()
	}
}

// release frees the gate if the ticket holds it, or leaves the queue if it
// does not. It is idempotent.
func (t *loadTicket) release() {
	g := t.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if t.done {
		return
	}
	t.done = true
	if g.holder == t {
		g.holder = nil
		g.grantLocked()
		return
	}
	for i, q := range g.queue {
		if q == t {
			g.queue = append(g.queue[:i], g.queue[i+1:]...)
			break
		}
	}
}
