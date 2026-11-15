package sessionshare

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/kvshare"
)

// readAllTransport reads a put's body to its end, as a receiver would.
type readAllTransport struct{ kvshare.Transport }

func (readAllTransport) Put(ctx context.Context, _, _ string, delta io.Reader) (kvshare.PutResult, error) {
	buf := make([]byte, 64)
	for {
		if err := ctx.Err(); err != nil {
			return kvshare.PutResult{}, context.Cause(ctx)
		}
		if _, err := delta.Read(buf); err == io.EOF {
			return kvshare.PutResult{}, nil
		} else if err != nil {
			return kvshare.PutResult{}, err
		}
	}
}

// slowReader yields one byte per interval, n bytes in all.
type slowReader struct {
	n     int
	every time.Duration
}

func (r *slowReader) Read(b []byte) (int, error) {
	if r.n == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.every)
	r.n--
	b[0] = 'x'
	return 1, nil
}

func TestStallGuardLetsASlowSendFinish(t *testing.T) {
	tr := guardStall(readAllTransport{}, time.Second)
	// 30 bytes 50 ms apart: 1.5 s in all, longer than the stall window,
	// yet never near 1 s without progress, even on a loaded host.
	if _, err := tr.Put(context.Background(), "", "k", &slowReader{n: 30, every: 50 * time.Millisecond}); err != nil {
		t.Fatalf("a slow send that keeps moving was cancelled: %v", err)
	}
}

// stuckTransport reads a little of a put's body and then stops, as a
// receiver behind a link that no longer moves; it returns when the put's
// context ends.
type stuckTransport struct{ kvshare.Transport }

func (stuckTransport) Put(ctx context.Context, _, _ string, delta io.Reader) (kvshare.PutResult, error) {
	_, _ = delta.Read(make([]byte, 8))
	<-ctx.Done()
	return kvshare.PutResult{}, context.Cause(ctx)
}

func TestStallGuardCancelsAStalledSend(t *testing.T) {
	tr := guardStall(stuckTransport{}, 50*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := tr.Put(ctx, "", "k", &slowReader{n: 1000, every: 0})
	if !errors.Is(err, errStalled) {
		t.Fatalf("err %v, want the stall", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("a stalled send took %v to end", took)
	}
}
