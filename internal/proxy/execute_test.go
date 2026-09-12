package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/route"
	"github.com/janit/viiwork/v2/internal/supervisor"
	"github.com/janit/viiwork/v2/meshapi"
)

// fakeLocalBackend is a route.LocalBackend in front of an httptest engine.
type fakeLocalBackend struct {
	id       string
	addr     string
	mu       sync.Mutex
	state    supervisor.State
	slots    int
	inFlight int
	releases int
	// onRelease runs under the lock after the nth release has taken its slot
	// back, so a test can occupy the slot again before anyone else sees it.
	onRelease func(n int)
	hard      atomic.Int64
}

func (b *fakeLocalBackend) ID() string   { return b.id }
func (b *fakeLocalBackend) Addr() string { return b.addr }
func (b *fakeLocalBackend) State() supervisor.State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
func (b *fakeLocalBackend) Slots() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.slots
}
func (b *fakeLocalBackend) InFlight() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inFlight
}
func (b *fakeLocalBackend) Acquire() {
	b.mu.Lock()
	b.inFlight++
	b.mu.Unlock()
}
func (b *fakeLocalBackend) Release() {
	b.mu.Lock()
	b.inFlight--
	b.releases++
	if b.onRelease != nil {
		b.onRelease(b.releases)
	}
	b.mu.Unlock()
}
func (b *fakeLocalBackend) NoteHardFailure() { b.hard.Add(1) }

var _ route.LocalBackend = (*fakeLocalBackend)(nil)

func engineServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

func backendFor(id string, s *httptest.Server) *fakeLocalBackend {
	return &fakeLocalBackend{id: id, addr: s.Listener.Addr().String(), state: supervisor.StateHealthy, slots: 1}
}

// trackingWriter records whether anything was written at all.
type trackingWriter struct {
	*httptest.ResponseRecorder
	wroteHeader bool
}

func (w *trackingWriter) WriteHeader(code int) {
	w.wroteHeader = true
	w.ResponseRecorder.WriteHeader(code)
}

func (w *trackingWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseRecorder.Write(b)
}

func chatRequest(target string, body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
}

const reasoningJSON = `{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"<think>\nhmm\n</think>\n\nHello"}}]}`

func TestServeLocalX1ThinkDisabledJSON(t *testing.T) {
	eng := engineServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reasoningJSON)
	})
	rec := httptest.NewRecorder()
	res := serveLocal(rec, chatRequest("/v1/chat/completions", `{"model":"m"}`), []byte(`{"model":"m"}`), backendFor("m/0", eng), "m", "self", true)
	want := rewriteThinkResponse([]byte(reasoningJSON))
	if res.Outcome != outcomeServed || res.Status != 200 || rec.Body.String() != string(want) {
		t.Fatalf("res=%+v body=%q", res, rec.Body.String())
	}
	if rec.Header().Get("Content-Length") != fmt.Sprint(len(want)) {
		t.Errorf("Content-Length = %q, want %d", rec.Header().Get("Content-Length"), len(want))
	}
	if rec.Header().Get(meshapi.HeaderNode) != "self" || rec.Header().Get(meshapi.HeaderGPUBackend) != "m/0" || rec.Header().Get(meshapi.HeaderModel) != "m" {
		t.Errorf("headers = %v", rec.Header())
	}
}

// streamingClient serves a handler and reads its response one SSE event at a time.
func streamingClient(t *testing.T, h http.HandlerFunc) *bufio.Reader {
	t.Helper()
	front := httptest.NewServer(h)
	t.Cleanup(front.Close)
	resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return bufio.NewReader(resp.Body)
}

func readEvent(t *testing.T, r *bufio.Reader, within time.Duration) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		line, _ := r.ReadString('\n')
		sep, _ := r.ReadString('\n') // blank separator
		got <- line + sep
	}()
	select {
	case l := <-got:
		return l
	case <-time.After(within):
		t.Fatal("no event arrived")
		return ""
	}
}

const (
	chunkOne     = "data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n"
	chunkTwo     = "data: {\"choices\":[{\"delta\":{\"content\":\"two\"}}]}\n\n"
	twoChunkBody = chunkOne + chunkTwo
)

func twoChunkEngine(release <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chunkOne)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, chunkTwo)
	}
}

func TestServeLocalX2Unbuffered(t *testing.T) {
	release := make(chan struct{})
	eng := engineServer(t, twoChunkEngine(release))
	b := backendFor("m/0", eng)
	r := streamingClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		serveLocal(w, r, body, b, "m", "self", false)
	})
	first := readEvent(t, r, 2*time.Second)
	if !strings.Contains(first, "one") {
		t.Fatalf("first event = %q", first)
	}
	close(release)
	second := readEvent(t, r, 2*time.Second)
	rest, _ := io.ReadAll(r)
	if got := first + second + string(rest); got != twoChunkBody {
		t.Errorf("client bytes = %q, want the engine's %q", got, twoChunkBody)
	}
}

func TestServeLocalX3EngineGone(t *testing.T) {
	eng := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	b := backendFor("m/0", eng)
	eng.Close()
	w := &trackingWriter{ResponseRecorder: httptest.NewRecorder()}
	res := serveLocal(w, chatRequest("/v1/chat/completions", "{}"), []byte("{}"), b, "m", "self", false)
	if res.Outcome != outcomeRetryable || b.hard.Load() != 1 || w.wroteHeader || w.Body.Len() != 0 {
		t.Errorf("res=%+v hard=%d wrote=%v", res, b.hard.Load(), w.wroteHeader)
	}
}

func TestServeLocalX4X5Statuses(t *testing.T) {
	busy := engineServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
	b := backendFor("m/0", busy)
	w := &trackingWriter{ResponseRecorder: httptest.NewRecorder()}
	if res := serveLocal(w, chatRequest("/v1/chat/completions", "{}"), []byte("{}"), b, "m", "self", false); res.Outcome != outcomeRetryable || b.hard.Load() != 0 || w.wroteHeader {
		t.Errorf("X4: res=%+v hard=%d wrote=%v", res, b.hard.Load(), w.wroteHeader)
	}

	boom := engineServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	})
	rec := httptest.NewRecorder()
	if res := serveLocal(rec, chatRequest("/v1/chat/completions", "{}"), []byte("{}"), backendFor("m/0", boom), "m", "self", false); res.Outcome != outcomeServed || rec.Code != 500 || rec.Body.String() != `{"error":"boom"}` {
		t.Errorf("X5: res=%+v code=%d body=%q", res, rec.Code, rec.Body.String())
	}
}

// X14: a think-disabled JSON response cut off mid-body is retryable and
// leaves no headers behind for the next attempt's response.
func TestServeLocalX14CutJSONLeavesNoHeaders(t *testing.T) {
	eng := engineServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"choices":[`)
		w.(http.Flusher).Flush()
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
	})
	w := &trackingWriter{ResponseRecorder: httptest.NewRecorder()}
	res := serveLocal(w, chatRequest("/v1/chat/completions", "{}"), []byte("{}"), backendFor("m/0", eng), "m", "self", true)
	if res.Outcome != outcomeRetryable || w.wroteHeader || len(w.Header()) != 0 {
		t.Errorf("res=%+v wrote=%v headers=%v", res, w.wroteHeader, w.Header())
	}
}

func TestServeLocalX6ClientGone(t *testing.T) {
	engineDone := make(chan struct{})
	eng := engineServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(engineDone)
	})
	b := backendFor("m/0", eng)
	ctx, cancel := context.WithCancel(context.Background())
	req := chatRequest("/v1/chat/completions", "{}").WithContext(ctx)
	w := &firstWriteCanceller{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	res := serveLocal(w, req, []byte("{}"), b, "m", "self", false)
	if !res.Aborted {
		t.Errorf("res = %+v, want aborted", res)
	}
	select {
	case <-engineDone:
	case <-time.After(time.Second):
		t.Error("the engine's request was not cancelled")
	}
}

// firstWriteCanceller cancels the client's context after the first body write.
type firstWriteCanceller struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
	once   sync.Once
}

func (w *firstWriteCanceller) Write(b []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(b)
	w.once.Do(w.cancel)
	return n, err
}

func TestForwardToPeerX7X8(t *testing.T) {
	var gotOrigin string
	var gotOK bool
	var gotQuery string
	verifier := mustAuth(t, "P", k1, nil, fakeMembers{})
	peer := engineServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotOrigin, gotOK = verifier.Verify(r, body)
		gotQuery = r.URL.RawQuery
		w.Header().Set(meshapi.HeaderNode, "P")
		w.Header().Set(meshapi.HeaderGPUBackend, "m/1")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reasoningJSON)
	})
	signer := mustAuth(t, "self", k1, nil, fakeMembers{})
	rec := httptest.NewRecorder()
	target := route.Target{Node: "P", Addr: peer.Listener.Addr().String()}
	res := forwardToPeer(rec, chatRequest("/v1/chat/completions?host=P", `{"model":"m"}`), []byte(`{"model":"m"}`), target, signer, "self")
	if !gotOK || gotOrigin != "self" || gotQuery != "host=P" {
		t.Errorf("X7: verify=%q,%v query=%q", gotOrigin, gotOK, gotQuery)
	}
	if res.Outcome != outcomeServed || rec.Body.String() != reasoningJSON {
		t.Errorf("X8: body rewritten or not served: res=%+v body=%q", res, rec.Body.String())
	}
	h := rec.Header()
	if h.Get(meshapi.HeaderNode) != "P" || h.Get(meshapi.HeaderGPUBackend) != "m/1" || h.Get(meshapi.HeaderOrigin) != "self" || h.Get("Content-Length") != fmt.Sprint(len(reasoningJSON)) {
		t.Errorf("X8: headers = %v", h)
	}
}

func TestForwardToPeerX9X10(t *testing.T) {
	auth := mustAuth(t, "self", nil, nil, fakeMembers{})
	full := engineServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(429) })
	w := &trackingWriter{ResponseRecorder: httptest.NewRecorder()}
	if res := forwardToPeer(w, chatRequest("/v1/chat/completions", "{}"), []byte("{}"), route.Target{Node: "P", Addr: full.Listener.Addr().String()}, auth, "self"); res.Outcome != outcomeRetryable || w.wroteHeader {
		t.Errorf("X9: res=%+v wrote=%v", res, w.wroteHeader)
	}
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := gone.Listener.Addr().String()
	gone.Close()
	if res := forwardToPeer(httptest.NewRecorder(), chatRequest("/v1/chat/completions", "{}"), []byte("{}"), route.Target{Node: "P", Addr: addr}, auth, "self"); res.Outcome != outcomeRetryable || !strings.Contains(res.Reason, "peer P") {
		t.Errorf("X10: res=%+v", res)
	}
}

func TestForwardToPeerX11Unbuffered(t *testing.T) {
	release := make(chan struct{})
	peer := engineServer(t, twoChunkEngine(release))
	auth := mustAuth(t, "self", nil, nil, fakeMembers{})
	r := streamingClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		forwardToPeer(w, r, body, route.Target{Node: "P", Addr: peer.Listener.Addr().String()}, auth, "self")
	})
	if first := readEvent(t, r, 2*time.Second); !strings.Contains(first, "one") {
		t.Fatalf("first event = %q", first)
	}
	close(release)
	if second := readEvent(t, r, 2*time.Second); !strings.Contains(second, "two") {
		t.Errorf("second event = %q", second)
	}
}

func TestForwardToPeerX12CutAfterFirstByte(t *testing.T) {
	peer := engineServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: chunk1\n\n")
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	})
	auth := mustAuth(t, "self", nil, nil, fakeMembers{})
	rec := httptest.NewRecorder()
	res := forwardToPeer(rec, chatRequest("/v1/chat/completions", "{}"), []byte("{}"), route.Target{Node: "P", Addr: peer.Listener.Addr().String()}, auth, "self")
	if res.Outcome != outcomeServed || !strings.Contains(rec.Body.String(), "chunk1") {
		t.Errorf("res=%+v body=%q", res, rec.Body.String())
	}
}

func TestIsHardSocketFailure(t *testing.T) {
	hard := []error{io.EOF, io.ErrUnexpectedEOF, syscall.ECONNREFUSED, syscall.ECONNRESET, &net.OpError{Op: "dial", Err: errors.New("x")}, fmt.Errorf("wrapped: %w", io.EOF)}
	for _, err := range hard {
		if !isHardSocketFailure(err) {
			t.Errorf("%v must be hard", err)
		}
	}
	for _, err := range []error{nil, context.Canceled, context.DeadlineExceeded, errors.New("other"), &net.OpError{Op: "read", Err: errors.New("x")}} {
		if isHardSocketFailure(err) {
			t.Errorf("%v must not be hard", err)
		}
	}
}
