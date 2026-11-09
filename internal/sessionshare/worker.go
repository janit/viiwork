package sessionshare

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/internal/supervisor"
)

// job is one session to save and send once its turn has settled.
type job struct {
	model, key string
	hash       uint64
	raw        string
	backend    Backend
	due        time.Time
	// slot is the slot the turn ran in, for a SlotSharer model.
	slot    supervisor.SlotClaim
	slotted bool
	// claimed: the slot was just claimed for another request, which has
	// not run yet, so it still holds the session although it is busy.
	claimed bool
}

// worker runs one backend's saves one at a time: a save pauses every slot of
// a Strata backend, so two in a row must not overlap, and a session's later
// turn replaces its waiting job instead of queueing a second save.
type worker struct {
	mu   sync.Mutex
	jobs map[string]job // by model + key
	wake chan struct{}
}

func newWorker() *worker {
	return &worker{jobs: map[string]job{}, wake: make(chan struct{}, 1)}
}

func (w *worker) add(j job) {
	w.mu.Lock()
	w.jobs[j.model+"\x00"+j.key] = j
	w.mu.Unlock()
	w.poke()
}

// defer puts back a job the worker took but did not run, unless a newer
// turn of its session has queued its own save meanwhile.
func (w *worker) putBack(j job) {
	w.mu.Lock()
	k := j.model + "\x00" + j.key
	if _, ok := w.jobs[k]; !ok {
		w.jobs[k] = j
	}
	w.mu.Unlock()
}

// drop forgets a waiting job: a new turn of the session has started.
func (w *worker) drop(model, key string) {
	w.mu.Lock()
	delete(w.jobs, model+"\x00"+key)
	w.mu.Unlock()
}

// takeSlot removes and returns the waiting job that would save session
// (its routing hash) from slot index, if there is one.
func (w *worker) takeSlot(model string, session uint64, index int) (job, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, j := range w.jobs {
		if j.model == model && j.hash == session && j.slotted && j.slot.Index == index {
			delete(w.jobs, k)
			return j, true
		}
	}
	return job{}, false
}

// takeAll removes and returns every waiting job, the earliest due first.
func (w *worker) takeAll() []job {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]job, 0, len(w.jobs))
	for k, j := range w.jobs {
		out = append(out, j)
		delete(w.jobs, k)
	}
	slices.SortFunc(out, func(a, b job) int { return a.due.Compare(b.due) })
	return out
}

func (w *worker) poke() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// next takes the job due first, if it is due by now, and says how long to
// wait otherwise (0 = nothing waiting).
func (w *worker) next(now time.Time) (job, bool, time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var first job
	var firstKey string
	for k, j := range w.jobs {
		if firstKey == "" || j.due.Before(first.due) {
			first, firstKey = j, k
		}
	}
	if firstKey == "" {
		return job{}, false, 0
	}
	if wait := first.due.Sub(now); wait > 0 {
		return job{}, false, wait
	}
	delete(w.jobs, firstKey)
	return first, true, 0
}

func (w *worker) run(ctx context.Context, do func(context.Context, job)) {
	for {
		j, ok, wait := w.next(time.Now())
		if ok {
			do(ctx, j)
			if ctx.Err() != nil {
				return
			}
			continue
		}
		var t *time.Timer
		var timer <-chan time.Time
		if wait > 0 {
			t = time.NewTimer(wait)
			timer = t.C
		}
		select {
		case <-ctx.Done():
		case <-w.wake:
		case <-timer:
		}
		if t != nil {
			t.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}
