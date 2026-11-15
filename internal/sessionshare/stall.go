package sessionshare

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/internal/kvshare"
)

// stallTransport bounds a push by its progress rather than by a fixed time:
// a put is cancelled once no byte of its body has moved for stall, or when
// the push as a whole runs out of its context (sendCap). A 2.6 GB replica
// takes 3.5 min at 100 Mbit/s and 14 at 25, so a fixed 5 min cancelled
// every first push of a long session on a slow link, and the next turn sent
// the whole file again (forge-white to forge-black on 2026-11-14, a degraded
// 1 GbE path: every push to forge-black failed for a day).
type stallTransport struct {
	kvshare.Transport
	stall time.Duration
}

func guardStall(t kvshare.Transport, stall time.Duration) kvshare.Transport {
	if t == nil {
		return nil
	}
	return stallTransport{t, stall}
}

func (t stallTransport) Put(ctx context.Context, addr, key string, delta io.Reader) (kvshare.PutResult, error) {
	ctx, r, stop := t.watch(ctx, delta)
	defer stop()
	return t.Transport.Put(ctx, addr, key, r)
}

func (t stallTransport) PutFile(ctx context.Context, addr, key string, meta kvshare.FileMeta, delta io.Reader) (kvshare.PutResult, error) {
	ctx, r, stop := t.watch(ctx, delta)
	defer stop()
	if mp, ok := t.Transport.(kvshare.MetaPutter); ok {
		return mp.PutFile(ctx, addr, key, meta, r)
	}
	return t.Transport.Put(ctx, addr, key, r)
}

// watch returns a context cancelled when r makes no progress for t.stall,
// and r as read through that watch. Once r has been read to its end the
// watch stops: the receiver's answer is bounded by ctx alone.
func (t stallTransport) watch(ctx context.Context, r io.Reader) (context.Context, io.Reader, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(t.stall, func() { cancel(errStalled) })
	stop := func() {
		timer.Stop()
		cancel(nil)
	}
	return ctx, &progressReader{r: r, timer: timer, stall: t.stall}, stop
}

var errStalled = stalledError{}

type stalledError struct{}

func (stalledError) Error() string { return "no progress sending the replica" }

type progressReader struct {
	r     io.Reader
	timer *time.Timer
	stall time.Duration
	once  sync.Once
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.timer.Reset(p.stall)
	}
	if err == io.EOF {
		p.once.Do(func() { p.timer.Stop() })
	}
	return n, err
}
