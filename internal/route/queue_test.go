package route

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/supervisor"
)

// realFixture uses the real clock: queue tests wait for real time.
func realFixture(queueMax int, timeout time.Duration) *fixture {
	f := &fixture{local: newFakeLocal(), reports: &fakeReports{}, clock: &clock{now: time.Now()}}
	f.router = New(Config{Self: "self", Local: f.local, Remote: f.reports, StaleAfter: 3 * time.Second, QueueMax: queueMax, QueueTimeout: timeout})
	return f
}

type acquired struct {
	lease  *Lease
	queued time.Duration
	err    error
	at     time.Time
}

func acquireAsync(f *fixture, ctx context.Context, req Request) <-chan acquired {
	ch := make(chan acquired, 1)
	go func() {
		l, q, err := f.router.Acquire(ctx, req)
		ch <- acquired{l, q, err, time.Now()}
	}()
	return ch
}

func waitQueue(t *testing.T, f *fixture, model string, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for f.router.QueueLen(model) != n {
		if time.Now().After(deadline) {
			t.Fatalf("queue length for %s = %d, want %d", model, f.router.QueueLen(model), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func recv(t *testing.T, ch <-chan acquired, within time.Duration) acquired {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(within):
		t.Fatalf("no result within %v", within)
		return acquired{}
	}
}

func TestQueueQ1FIFO(t *testing.T) {
	f := realFixture(64, 5*time.Second)
	f.local.add("m", newFakeBackend("m/0", 1))
	held := f.pick(t, Request{Model: "m"})
	var chans []<-chan acquired
	for i := 0; i < 3; i++ {
		chans = append(chans, acquireAsync(f, context.Background(), Request{Model: "m"}))
		waitQueue(t, f, "m", i+1)
	}
	held.Release()
	for i, ch := range chans {
		a := recv(t, ch, 2*time.Second)
		if a.err != nil || a.queued <= 0 {
			t.Fatalf("waiter %d: err=%v queued=%v", i, a.err, a.queued)
		}
		for j := i + 1; j < len(chans); j++ {
			select {
			case <-chans[j]:
				t.Fatalf("waiter %d served before waiter %d released", j, i)
			default:
			}
		}
		a.lease.Release()
	}
}

func TestQueueQ2FreeSlot(t *testing.T) {
	f := realFixture(64, time.Second)
	f.local.add("m", newFakeBackend("m/0", 1))
	l, q, err := f.router.Acquire(context.Background(), Request{Model: "m"})
	if err != nil || q != 0 {
		t.Errorf("Acquire = %v, queued %v", err, q)
	}
	l.Release()
}

func TestQueueQ3Timeout(t *testing.T) {
	f := realFixture(64, 100*time.Millisecond)
	f.local.add("m", newFakeBackend("m/0", 1))
	f.pick(t, Request{Model: "m"})
	start := time.Now()
	_, _, err := f.router.Acquire(context.Background(), Request{Model: "m"})
	if !errors.Is(err, ErrQueueTimeout) || time.Since(start) < 100*time.Millisecond {
		t.Errorf("err = %v after %v", err, time.Since(start))
	}
	if n := f.router.QueueLen("m"); n != 0 {
		t.Errorf("QueueLen = %d after the timeout", n)
	}
}

func TestQueueQ4Q5Full(t *testing.T) {
	f := realFixture(2, 5*time.Second)
	f.local.add("m", newFakeBackend("m/0", 1))
	held := f.pick(t, Request{Model: "m"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	acquireAsync(f, ctx, Request{Model: "m"})
	acquireAsync(f, ctx, Request{Model: "m"})
	waitQueue(t, f, "m", 2)
	start := time.Now()
	if _, _, err := f.router.Acquire(context.Background(), Request{Model: "m"}); !errors.Is(err, ErrQueueFull) || time.Since(start) > 50*time.Millisecond {
		t.Errorf("Q4: err = %v after %v", err, time.Since(start))
	}
	cancel()
	held.Release()

	none := realFixture(0, 5*time.Second)
	none.local.add("m", newFakeBackend("m/0", 1))
	none.pick(t, Request{Model: "m"})
	if _, _, err := none.router.Acquire(context.Background(), Request{Model: "m"}); !errors.Is(err, ErrQueueFull) {
		t.Errorf("Q5: err = %v", err)
	}
}

func TestQueueQ6Cancel(t *testing.T) {
	f := realFixture(64, 5*time.Second)
	b := newFakeBackend("m/0", 1)
	f.local.add("m", b)
	held := f.pick(t, Request{Model: "m"})
	ctx, cancel := context.WithCancel(context.Background())
	ch := acquireAsync(f, ctx, Request{Model: "m"})
	waitQueue(t, f, "m", 1)
	cancel()
	a := recv(t, ch, time.Second)
	if !errors.Is(a.err, context.Canceled) || f.router.QueueLen("m") != 0 {
		t.Errorf("err=%v QueueLen=%d", a.err, f.router.QueueLen("m"))
	}
	held.Release()
	time.Sleep(20 * time.Millisecond)
	if b.InFlight() != 0 {
		t.Errorf("in-flight = %d after release, a lease leaked", b.InFlight())
	}
}

func TestQueueQ7FreshReportWakes(t *testing.T) {
	f := realFixture(64, 5*time.Second)
	f.reports.set(report("P", time.Now().Add(-time.Hour), time.Millisecond, capM(1, 0, 1)))
	ch := acquireAsync(f, context.Background(), Request{Model: "m"})
	waitQueue(t, f, "m", 1)
	f.reports.set(report("P", time.Now(), time.Millisecond, capM(1, 0, 1)))
	f.router.Wake()
	a := recv(t, ch, time.Second)
	if a.err != nil || a.lease.Target().Node != "P" {
		t.Errorf("err=%v target=%+v", a.err, a.lease)
	}
}

func TestQueueQ15RefusedPeerWaitsForNewerReport(t *testing.T) {
	f := realFixture(64, 5*time.Second)
	f.reports.set(report("P", time.Now(), time.Millisecond, capM(1, 0, 1)))
	l, err := f.router.Pick(Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	l.Refused()
	l.Release()
	ch := acquireAsync(f, context.Background(), Request{Model: "m"})
	waitQueue(t, f, "m", 1)
	f.router.Wake()
	if n := f.router.QueueLen("m"); n != 1 {
		t.Fatalf("dispatched to the refused peer by the old report: queue length %d", n)
	}
	f.reports.set(report("P", time.Now(), time.Millisecond, capM(1, 0, 1)))
	f.router.Wake()
	if a := recv(t, ch, time.Second); a.err != nil || a.lease.Target().Node != "P" {
		t.Errorf("err=%v lease=%+v", a.err, a.lease)
	}
}

func TestQueueQ8TickCatchesHealthyBackend(t *testing.T) {
	f := realFixture(64, 5*time.Second)
	b := newFakeBackend("m/0", 1)
	b.set(supervisor.StateStarting)
	f.local.add("m", b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.router.Run(ctx)
	ch := acquireAsync(f, context.Background(), Request{Model: "m"})
	waitQueue(t, f, "m", 1)
	b.set(supervisor.StateHealthy)
	if a := recv(t, ch, 500*time.Millisecond); a.err != nil {
		t.Errorf("err = %v", a.err)
	}
}

func TestQueueQ9PinnedWaiterDoesNotBlock(t *testing.T) {
	f := realFixture(64, 5*time.Second)
	f.local.add("m", newFakeBackend("m/0", 1))
	f.reports.set(report("B", time.Now(), time.Millisecond, capM(1, 1, 1)))
	held := f.pick(t, Request{Model: "m", Host: "self"})
	pinned := acquireAsync(f, context.Background(), Request{Model: "m", Host: "B"})
	waitQueue(t, f, "m", 1)
	free := acquireAsync(f, context.Background(), Request{Model: "m"})
	waitQueue(t, f, "m", 2)
	held.Release()
	if a := recv(t, free, time.Second); a.err != nil || !a.lease.Target().Local {
		t.Errorf("unpinned waiter: %+v", a)
	}
	select {
	case a := <-pinned:
		t.Errorf("the pinned waiter must keep waiting, got %+v", a)
	default:
	}
}

func TestQueueQ10PinnedHostGone(t *testing.T) {
	f := realFixture(64, 5*time.Second)
	f.local.add("m", newFakeBackend("m/0", 1))
	f.reports.set(report("B", time.Now(), time.Millisecond, capM(1, 1, 1)))
	ch := acquireAsync(f, context.Background(), Request{Model: "m", Host: "B"})
	waitQueue(t, f, "m", 1)
	f.reports.set()
	f.router.Wake()
	if a := recv(t, ch, time.Second); !errors.Is(a.err, ErrHostNotServing) {
		t.Errorf("err = %v", a.err)
	}
}

func TestQueueQ11ForwardedNeverQueues(t *testing.T) {
	f := realFixture(64, 5*time.Second)
	f.local.add("m", newFakeBackend("m/0", 1))
	f.pick(t, Request{Model: "m"})
	start := time.Now()
	if _, _, err := f.router.Acquire(context.Background(), Request{Model: "m", Forwarded: true}); !errors.Is(err, ErrNoFreeSlot) || time.Since(start) > 50*time.Millisecond {
		t.Errorf("err = %v after %v", err, time.Since(start))
	}
	if f.router.QueueLen("m") != 0 {
		t.Error("a forward must never queue")
	}
}

func TestQueueQ12Stress(t *testing.T) {
	f := realFixture(64, 5*time.Second)
	b := newFakeBackend("m/0", 3)
	f.local.add("m", b)
	var maxSeen atomic.Int64
	var failures atomic.Int64
	stop := time.Now().Add(2 * time.Second)
	var wg sync.WaitGroup
	for g := 0; g < 50; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(stop) {
				l, _, err := f.router.Acquire(context.Background(), Request{Model: "m"})
				if err != nil {
					failures.Add(1)
					return
				}
				if n := int64(b.InFlight()); n > maxSeen.Load() {
					maxSeen.Store(n)
				}
				time.Sleep(5 * time.Millisecond)
				l.Release()
			}
		}()
	}
	wg.Wait()
	if maxSeen.Load() > 3 || failures.Load() != 0 {
		t.Errorf("max in flight %d (want <= 3), failures %d", maxSeen.Load(), failures.Load())
	}
}

func TestQueueQ13Q14Budget(t *testing.T) {
	f := realFixture(64, 5*time.Second)
	f.local.add("m", newFakeBackend("m/0", 1))
	f.pick(t, Request{Model: "m"})
	start := time.Now()
	_, _, err := f.router.Acquire(context.Background(), Request{Model: "m", QueueBudget: 100 * time.Millisecond})
	if took := time.Since(start); !errors.Is(err, ErrQueueTimeout) || took < 100*time.Millisecond || took > time.Second {
		t.Errorf("Q13: err = %v after %v", err, took)
	}
	start = time.Now()
	_, _, err = f.router.Acquire(context.Background(), Request{Model: "m", QueueBudget: -1})
	if !errors.Is(err, ErrQueueTimeout) || time.Since(start) > 50*time.Millisecond || f.router.QueueLen("m") != 0 {
		t.Errorf("Q14: err = %v after %v", err, time.Since(start))
	}
}
