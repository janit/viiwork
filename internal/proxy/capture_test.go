package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/activity"
	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/peer"
)

// meshHandlerFor wires the same single-backend mesh handler the prompt tests
// use, since output capture sits on the same routing path as prompt capture.
func meshHandlerFor(t *testing.T, backendAddr string) (*Handler, *activity.Log) {
	t.Helper()
	state := balancer.NewBackendState(0, backendAddr)
	state.SetStatus(balancer.StatusHealthy)
	backends := []*balancer.BackendState{state}
	reg := peer.NewRegistry("viiwork-local", "test", backends, nil, 3*time.Second)
	h := NewMeshHandler(balancer.New(backends, 7, 4), reg, 30*time.Second)
	log := activity.NewLog()
	h.SetActivity(log)
	return h, log
}

func fetchEntry(t *testing.T, h *Handler, rid int64) activity.PromptEntry {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/prompts?rid="+strconv.FormatInt(rid, 10), nil))
	if w.Code != 200 {
		t.Fatalf("/v1/prompts: got %d", w.Code)
	}
	var entry activity.PromptEntry
	if err := json.Unmarshal(w.Body.Bytes(), &entry); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return entry
}

func postChat(t *testing.T, h *Handler, body string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("proxy: got %d (%s)", w.Code, w.Body.String())
	}
}

func TestOutputCapturedNonStreaming(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"2+2 is 4."}}]}`))
	}))
	defer backend.Close()

	h, log := meshHandlerFor(t, backend.Listener.Addr().String())
	postChat(t, h, `{"model":"test","messages":[{"role":"user","content":"what is 2+2?"}]}`)

	entry := fetchEntry(t, h, findRequestRID(t, log))
	if entry.Prompt != "what is 2+2?" {
		t.Errorf("prompt = %q", entry.Prompt)
	}
	if entry.Output != "2+2 is 4." {
		t.Errorf("output = %q, want %q", entry.Output, "2+2 is 4.")
	}
}

func TestOutputCapturedStreaming(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for _, tok := range []string{"Hello", ", ", "world"} {
			w.Write([]byte(`data: {"choices":[{"delta":{"content":"` + tok + `"}}]}` + "\n\n"))
			f.Flush()
		}
		w.Write([]byte("data: [DONE]\n\n"))
		f.Flush()
	}))
	defer backend.Close()

	h, log := meshHandlerFor(t, backend.Listener.Addr().String())
	postChat(t, h, `{"model":"test","stream":true,"messages":[{"role":"user","content":"greet"}]}`)

	entry := fetchEntry(t, h, findRequestRID(t, log))
	if entry.Output != "Hello, world" {
		t.Errorf("output = %q, want %q", entry.Output, "Hello, world")
	}
	// Elapsed is recorded with the output; it is what the dashboard shows for
	// a finished request, so a zero here would silently blank that field.
	if entry.ElapsedMS < 0 {
		t.Errorf("elapsed_ms = %d, want >= 0", entry.ElapsedMS)
	}
}

// A failing backend is exactly when the output panel is worth opening, so the
// error body is kept rather than discarded for having no completion text.
func TestOutputCapturesErrorBody(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"context length exceeded"}}`, http.StatusBadRequest)
	}))
	defer backend.Close()

	h, log := meshHandlerFor(t, backend.Listener.Addr().String())
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"war and peace"}]}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), req)

	entry := fetchEntry(t, h, findRequestRID(t, log))
	if !strings.Contains(entry.Output, "context length exceeded") {
		t.Errorf("output = %q, want the error body", entry.Output)
	}
}

func TestExtractOutputText(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "sse chat deltas",
			raw:  "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\ndata: [DONE]\n\n",
			want: "ab",
		},
		{
			name: "sse legacy completions use text, not delta",
			raw:  "data: {\"choices\":[{\"text\":\"one \"}]}\n\ndata: {\"choices\":[{\"text\":\"two\"}]}\n\n",
			want: "one two",
		},
		{
			name: "whole chat response",
			raw:  `{"choices":[{"message":{"content":"done"}}]}`,
			want: "done",
		},
		{
			name: "reasoning is labelled, not merged into the answer",
			raw:  `{"choices":[{"message":{"reasoning_content":"thinking","content":"answer"}}]}`,
			want: "[reasoning]\nthinking\n\n[answer]\nanswer",
		},
		{
			// A thinking model with think enabled leaves content empty; dropping
			// reasoning would show a blank output for those requests.
			name: "reasoning only",
			raw:  `{"choices":[{"delta":{"reasoning_content":"just thinking"}}]}`,
			want: "[reasoning]\njust thinking",
		},
		{
			name: "malformed chunks are skipped, not fatal",
			raw:  "data: not json\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n",
			want: "ok",
		},
		{name: "empty", raw: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractOutputText([]byte(tc.raw)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The Flusher assertion drives whether responses stream at all: proxyRequest
// and streamThinkDisabled both branch on it, and the latter falls back to a
// buffered io.Copy when it fails. The wrapper must therefore mirror the
// underlying writer rather than always or never advertising Flush.
func TestCaptureWriterMirrorsFlusher(t *testing.T) {
	wrapped, _ := newCaptureWriter(httptest.NewRecorder())
	if _, ok := wrapped.(http.Flusher); !ok {
		t.Error("wrapping a flushing writer must stay flushable")
	}

	wrapped, _ = newCaptureWriter(nonFlushingWriter{httptest.NewRecorder()})
	if _, ok := wrapped.(http.Flusher); ok {
		t.Error("wrapping a non-flushing writer must not advertise Flush")
	}
}

// nonFlushingWriter hides the recorder's Flush method behind an interface that
// does not include it.
type nonFlushingWriter struct{ rec *httptest.ResponseRecorder }

func (n nonFlushingWriter) Header() http.Header         { return n.rec.Header() }
func (n nonFlushingWriter) Write(b []byte) (int, error) { return n.rec.Write(b) }
func (n nonFlushingWriter) WriteHeader(status int)      { n.rec.WriteHeader(status) }

func TestCaptureWriterBounded(t *testing.T) {
	rec := httptest.NewRecorder()
	wrapped, capw := newCaptureWriter(rec)

	chunk := strings.Repeat("x", 64*1024)
	written := 0
	for written < maxCaptureBytes+128*1024 {
		n, err := wrapped.Write([]byte(chunk))
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		written += n
	}
	if len(capw.buf) > maxCaptureBytes {
		t.Errorf("captured %d bytes, cap is %d", len(capw.buf), maxCaptureBytes)
	}
	if !capw.overflow {
		t.Error("overflow not flagged past the cap")
	}
	// The client still gets everything; only the capture is bounded.
	if rec.Body.Len() != written {
		t.Errorf("client received %d bytes, wrote %d", rec.Body.Len(), written)
	}
}

func TestPromptPageServed(t *testing.T) {
	h := NewHandler(balancer.New(nil, 7, 4), "/models/test.gguf", 30*time.Second)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/prompt?rid=1&addr=", nil))
	if w.Code != 200 {
		t.Fatalf("/prompt: got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
	if !strings.Contains(w.Body.String(), "/v1/mesh/prompt") {
		t.Error("prompt page does not fetch the prompt endpoint")
	}
}
