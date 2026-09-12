package supervisor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func granted(t *testing.T, tk *loadTicket, within time.Duration) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	return tk.wait(ctx) == nil
}

func TestLoadGateFIFO(t *testing.T) {
	g := newLoadGate()
	a, b, c := g.enqueue(), g.enqueue(), g.enqueue()

	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	for name, tk := range map[string]*loadTicket{"A": a, "B": b, "C": c} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := tk.wait(context.Background()); err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			tk.release()
		}()
	}
	wg.Wait()
	if len(order) != 3 || order[0] != "A" || order[1] != "B" || order[2] != "C" {
		t.Errorf("grant order = %v, want [A B C]", order)
	}
}

func TestLoadGateCancelledWaiterLeavesQueue(t *testing.T) {
	g := newLoadGate()
	a := g.enqueue()
	b := g.enqueue()
	c := g.enqueue()
	if !granted(t, a, time.Second) {
		t.Fatal("A not granted")
	}

	ctx, cancel := context.WithCancel(context.Background())
	errB := make(chan error, 1)
	go func() { errB <- b.wait(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-errB; !errors.Is(err, context.Canceled) {
		t.Fatalf("B wait = %v, want context.Canceled", err)
	}

	a.release()
	if !granted(t, c, time.Second) {
		t.Error("C must be granted after A releases, skipping the cancelled B")
	}
}

func TestLoadGateReleaseIsIdempotent(t *testing.T) {
	g := newLoadGate()
	holder := g.enqueue()
	waiting := g.enqueue()
	waiting.release()
	waiting.release()
	holder.release()
	holder.release()
	if !granted(t, g.enqueue(), time.Second) {
		t.Error("the gate must be grantable after idempotent releases")
	}
}
