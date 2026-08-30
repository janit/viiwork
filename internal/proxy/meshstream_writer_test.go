package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/activity"
	"github.com/janit/viiwork/internal/peer"
	"github.com/janit/viiwork/meshapi"
)

// gatedRecorder is a ResponseWriter that can hold a write open, and that counts
// any write landing after the handler has returned.
//
// net/http's contract is that a ResponseWriter must not be touched once
// ServeHTTP returns. This handler runs a producer goroutine per peer plus the
// cluster snapshot loop, and the question is whether any of them can still be
// writing by then. The race detector only caught it when the scheduler
// interleaved the right way — roughly two runs in three — so this arranges the
// interleaving instead of hoping for it: the cluster write is held open, and
// the handler is given every chance to return underneath it.
type gatedRecorder struct {
	mu     sync.Mutex
	sealed bool
	after  int
	hdr    http.Header

	started  chan struct{} // signalled when a cluster write begins
	release  chan struct{} // closed to let held writes complete
	finished chan struct{} // signalled once a held write has actually landed
}

func newGatedRecorder() *gatedRecorder {
	return &gatedRecorder{
		hdr:      make(http.Header),
		started:  make(chan struct{}, 1),
		release:  make(chan struct{}),
		finished: make(chan struct{}, 1),
	}
}

func (g *gatedRecorder) Header() http.Header { return g.hdr }
func (g *gatedRecorder) WriteHeader(int)     {}
func (g *gatedRecorder) Flush()              {}

func (g *gatedRecorder) Write(b []byte) (int, error) {
	// Only the cluster snapshot is gated. In the buggy shape that write is
	// performed by the snapshot goroutine; in the fixed one the handler
	// performs it, which is the entire difference this test is looking for.
	gated := bytes.Contains(b, []byte("event: cluster"))
	if gated {
		select {
		case g.started <- struct{}{}:
		default:
		}
		<-g.release
	}

	g.mu.Lock()
	if g.sealed {
		g.after++
	}
	g.mu.Unlock()

	// Announce the landing, so the test can observe it rather than racing the
	// scheduler to read the counter first. Without this the assertion can run
	// before the released producer is scheduled at all, and the test passes
	// against the very bug it exists to catch.
	if gated {
		select {
		case g.finished <- struct{}{}:
		default:
		}
	}
	return len(b), nil
}

// seal is called by the serving goroutine the instant ServeHTTP returns, so
// anything counted afterwards is a write net/http would consider out of
// contract.
func (g *gatedRecorder) seal() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sealed = true
}

func (g *gatedRecorder) writesAfterSeal() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.after
}

func TestMeshStreamWritesNothingAfterHandlerReturns(t *testing.T) {
	log := activity.NewLog()
	log.EmitRequest(7, 0, "model-a → gpu-0")

	// Unreachable peers on purpose: followPeerActivity then sits on its
	// reconnect backoff, the producer most likely to outlive the request.
	peers := []*peer.PeerState{peer.NewPeerState("127.0.0.1:1"), peer.NewPeerState("127.0.0.1:2")}
	reg := peer.NewRegistry("node-a", "model-a", newTestBackendState(t), peers, time.Second)
	reg.SetLocation("hostA", "hostA:8080")
	h := NewMeshHandler(nil, reg, time.Second)
	h.SetActivity(log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newGatedRecorder()
	req := httptest.NewRequest("GET", meshapi.PathMeshStream, nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		rec.seal()
		close(done)
	}()

	select {
	case <-rec.started:
	case <-time.After(5 * time.Second):
		t.Fatal("no cluster snapshot was written; the test cannot observe what it is for")
	}

	// A cluster write is now held open. Cancel, and give the handler room to
	// return while that write is still outstanding — which is exactly what it
	// used to do, leaving the snapshot goroutine writing into a dead
	// ResponseWriter.
	cancel()
	select {
	case <-done:
		// Returned while a write is still in flight. Whether that write then
		// lands after the seal is the bug.
	case <-time.After(500 * time.Millisecond):
		// Still inside the write it owns, which is the correct behaviour.
	}

	close(rec.release)

	select {
	case <-rec.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the held cluster write never completed")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the held write was released")
	}

	if n := rec.writesAfterSeal(); n != 0 {
		t.Errorf("%d write(s) reached the ResponseWriter after ServeHTTP returned — net/http forbids this", n)
	}
}
