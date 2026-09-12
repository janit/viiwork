package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

func TestExtractCompletionTokens(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		want  int64
		found bool
	}{
		{"non-streaming", `{"id":"x","usage":{"prompt_tokens":10,"completion_tokens":42}}`, 42, true},
		{"no usage", `{"id":"x"}`, 0, false},
		{"null usage", `{"usage":null}`, 0, false},
		{"sse with usage", buildSSEStream(20) + "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":20}}\n\ndata: [DONE]\n\n", 20, true},
		{"sse without usage", buildSSEStream(20), 0, false},
		{"last usage wins", "data: {\"usage\":{\"completion_tokens\":7}}\n\ndata: {\"usage\":{\"completion_tokens\":9}}\n\ndata: [DONE]\n\n", 9, true},
	}
	for _, tc := range cases {
		if got, found := extractCompletionTokens([]byte(tc.body)); got != tc.want || found != tc.found {
			t.Errorf("%s: = (%d, %v), want (%d, %v)", tc.name, got, found, tc.want, tc.found)
		}
	}
}

func TestCaptureWriterTailKeepsUsagePastTheCap(t *testing.T) {
	rec := httptest.NewRecorder()
	wrapped, capw := newCaptureWriter(rec)
	chunk := []byte("data: " + sseChunk("token ") + "\n\n")
	written := 0
	for written < 3<<20 {
		n, _ := wrapped.Write(chunk)
		written += n
	}
	for _, tail := range []string{"data: {\"choices\":[],\"usage\":{\"completion_tokens\":20}}\n\n", "data: [DONE]\n\n"} {
		n, _ := wrapped.Write([]byte(tail))
		written += n
	}
	if got, found := capw.CompletionTokens(); got != 20 || !found {
		t.Errorf("CompletionTokens = (%d, %v), want (20, true) from the tail", got, found)
	}
	if len(capw.buf) > maxCaptureBytes || len(capw.Output()) > maxCaptureBytes {
		t.Errorf("capture no longer bounded: buf %d, output %d", len(capw.buf), len(capw.Output()))
	}
	if rec.Body.Len() != written {
		t.Errorf("client received %d bytes, wrote %d", rec.Body.Len(), written)
	}
}
