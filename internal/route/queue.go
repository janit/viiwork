package route

import (
	"context"
	"errors"
	"time"
)

var (
	ErrQueueFull    = errors.New("route: queue full")
	ErrQueueTimeout = errors.New("route: no free slot before the queue timeout")
)

// dispatchTick catches slots no event announces, such as a local backend
// turning healthy, without a hook from the supervisor into the router.
const dispatchTick = 250 * time.Millisecond

type waiter struct {
	req      Request // without Exclude: a queued request may go anywhere
	enqueued time.Time
	result   chan waitResult // buffered 1
	done     bool            // dispatched, completed with an error, or left
}

type waitResult struct {
	lease *Lease
	err   error
}

// Acquire takes a slot now, or waits for one in the model's origin queue.
//
// Model-not-found and host-not-serving answer at once, and so does a forward
// with no free slot: a receiver is strict and never queues (spec section 4).
// The queue is FIFO per model and bounded by QueueMax; beyond it, ErrQueueFull.
// A waiter gives up after its budget (Request.QueueBudget, else QueueTimeout)
// with ErrQueueTimeout, or when ctx ends with ctx's error.
func (r *Router) Acquire(ctx context.Context, req Request) (*Lease, time.Duration, error) {
	r.mu.Lock()
	l, err := r.pickLocked(req)
	switch {
	case err == nil:
		r.mu.Unlock()
		return l, 0, nil
	case !errors.Is(err, ErrNoFreeSlot), req.Forwarded:
		r.mu.Unlock()
		return nil, 0, err
	case req.QueueBudget < 0:
		r.mu.Unlock()
		return nil, 0, ErrQueueTimeout
	case r.c.QueueMax <= 0 || len(r.queues[req.Model]) >= r.c.QueueMax:
		r.mu.Unlock()
		return nil, 0, ErrQueueFull
	}
	w := &waiter{
		req:      Request{Model: req.Model, Host: req.Host},
		enqueued: time.Now(),
		result:   make(chan waitResult, 1),
	}
	r.queues[req.Model] = append(r.queues[req.Model], w)
	r.mu.Unlock()

	budget := req.QueueBudget
	if budget <= 0 {
		budget = r.c.QueueTimeout
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case res := <-w.result:
		return res.lease, time.Since(w.enqueued), res.err
	case <-timer.C:
		if res, ok := r.leave(w); ok {
			// A lease assigned at the moment the timeout fired is used, not leaked.
			return res.lease, time.Since(w.enqueued), res.err
		}
		return nil, time.Since(w.enqueued), ErrQueueTimeout
	case <-ctx.Done():
		if res, ok := r.leave(w); ok && res.lease != nil {
			res.lease.Release()
		}
		return nil, time.Since(w.enqueued), ctx.Err()
	}
}

// leave removes a waiter that stopped waiting. If dispatch completed it first,
// it returns that result.
func (r *Router) leave(w *waiter) (waitResult, bool) {
	r.mu.Lock()
	if !w.done {
		w.done = true
		r.removeWaiterLocked(w)
		r.mu.Unlock()
		return waitResult{}, false
	}
	r.mu.Unlock()
	return <-w.result, true
}

func (r *Router) removeWaiterLocked(w *waiter) {
	ws := r.queues[w.req.Model]
	for i, x := range ws {
		if x == w {
			ws = append(ws[:i], ws[i+1:]...)
			break
		}
	}
	if len(ws) == 0 {
		delete(r.queues, w.req.Model)
		return
	}
	r.queues[w.req.Model] = ws
}

// QueueTimeout is the configured wait before ErrQueueTimeout. The handler
// names it in the 429 and spends what is left of it on a final acquisition
// after a failed dispatch (Decision 17).
func (r *Router) QueueTimeout() time.Duration { return r.c.QueueTimeout }

// QueueLen is the model's current waiters: the "queued" of /v1/capacity.
func (r *Router) QueueLen(model string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.queues[model])
}

// Wake dispatches queued requests now. P6 calls it when a capacity report
// arrives and when a member joins.
func (r *Router) Wake() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dispatchLocked()
}

// released is called by every lease release.
func (r *Router) released() { r.Wake() }

// Run dispatches on a 250 ms tick while any queue is non-empty, until ctx ends.
func (r *Router) Run(ctx context.Context) {
	t := time.NewTicker(dispatchTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.mu.Lock()
			if len(r.queues) > 0 {
				r.dispatchLocked()
			}
			r.mu.Unlock()
		}
	}
}

// dispatchLocked scans every model's waiters in arrival order and hands a slot
// to the first waiter that has one, repeating until a full scan hands out
// nothing. Scanning past a waiter with no candidate is what keeps a request
// pinned to a full host from stalling unpinned requests behind it (Decision 7).
// A waiter whose model or pinned host is gone completes with that error.
func (r *Router) dispatchLocked() {
	for progressed := true; progressed; {
		progressed = false
		for _, ws := range r.queues {
			for _, w := range append([]*waiter(nil), ws...) {
				l, err := r.pickLocked(w.req)
				switch {
				case err == nil:
					r.completeLocked(w, waitResult{lease: l})
					progressed = true
				case errors.Is(err, ErrModelNotFound), errors.Is(err, ErrHostNotServing):
					r.completeLocked(w, waitResult{err: err})
					progressed = true
				}
			}
		}
	}
}

func (r *Router) completeLocked(w *waiter, res waitResult) {
	w.done = true
	r.removeWaiterLocked(w)
	w.result <- res
}
