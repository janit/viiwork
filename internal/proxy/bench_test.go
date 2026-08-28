package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Benchmarks for the per-request and per-token hot paths.
//
// Context for judging any result here: viiwork proxies LLM inference that takes
// seconds to minutes, so shaving microseconds off request handling does not make
// a user-visible request faster. What it DOES do on gb1 is free CPU. The host has
// 4 cores shared between viiwork and every llama-server prompt-eval thread, and
// the manager already warns when backends oversubscribe them. Go-side CPU spent
// per token is CPU taken from inference, so these paths are worth keeping cheap.

// sseChunk is a representative llama.cpp streaming delta.
func sseChunk(content string) string {
	return fmt.Sprintf(`{"id":"chatcmpl-abc123","object":"chat.completion.chunk","created":1700000000,`+
		`"model":"Laguna-XS-2.1-Q4_K_M","system_fingerprint":"b10437",`+
		`"choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, content)
}

func buildSSEStream(nTokens int) string {
	var b strings.Builder
	b.WriteString("data: " + sseChunk("<think>") + "\n\n")
	for i := 0; i < nTokens/2; i++ {
		b.WriteString("data: " + sseChunk(fmt.Sprintf("reason%d ", i)) + "\n\n")
	}
	b.WriteString("data: " + sseChunk("</think>") + "\n\n")
	for i := 0; i < nTokens/2; i++ {
		b.WriteString("data: " + sseChunk(fmt.Sprintf("answer%d ", i)) + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// flushRecorder is an http.ResponseWriter that implements http.Flusher, which
// streamThinkDisabled requires before it will take the rewriting path at all.
type flushRecorder struct{ n int }

func (f *flushRecorder) Header() http.Header         { return http.Header{} }
func (f *flushRecorder) Write(p []byte) (int, error) { f.n += len(p); return len(p), nil }
func (f *flushRecorder) WriteHeader(int)             {}
func (f *flushRecorder) Flush()                      {}

// BenchmarkStreamThinkDisabled is THE hot loop: think is disabled whenever the
// request omits "think", which is the default, so every streaming response is
// re-parsed and re-encoded one SSE chunk per token.
func BenchmarkStreamThinkDisabled(b *testing.B) {
	for _, n := range []int{64, 512} {
		stream := buildSSEStream(n)
		b.Run(fmt.Sprintf("tokens=%d", n), func(b *testing.B) {
			b.SetBytes(int64(len(stream)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				w := &flushRecorder{}
				streamThinkDisabled(w, strings.NewReader(stream), func() {})
			}
		})
	}
}

// reasoningChunk emits reasoning_content, which forces the SLOW path: the
// chunk may be mutated, so it must be decoded and re-encoded.
func reasoningChunk(reasoning string) string {
	return fmt.Sprintf(`{"id":"chatcmpl-abc123","object":"chat.completion.chunk","created":1700000000,`+
		`"model":"Laguna-XS-2.1-Q4_K_M","system_fingerprint":"b10437",`+
		`"choices":[{"index":0,"delta":{"reasoning_content":%q},"finish_reason":null}]}`, reasoning)
}

func buildReasoningStream(nTokens int) string {
	var b strings.Builder
	b.WriteString("data: " + reasoningChunk("<think>") + "\n\n")
	for i := 0; i < nTokens; i++ {
		b.WriteString("data: " + reasoningChunk(fmt.Sprintf("thought%d ", i)) + "\n\n")
	}
	b.WriteString("data: " + reasoningChunk("</think>the answer") + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// BenchmarkStreamThinkDisabledReasoning measures the slow path — every chunk
// carries reasoning_content, so the pass-through fast path never fires. This is
// the worst case and bounds the other benchmark's optimism.
func BenchmarkStreamThinkDisabledReasoning(b *testing.B) {
	stream := buildReasoningStream(512)
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := &flushRecorder{}
		streamThinkDisabled(w, strings.NewReader(stream), func() {})
	}
}

// BenchmarkRewriteThinkResponse covers the non-streaming equivalent.
func BenchmarkRewriteThinkResponse(b *testing.B) {
	body := []byte(`{"id":"chatcmpl-abc","object":"chat.completion","created":1700000000,` +
		`"model":"Laguna-XS-2.1-Q4_K_M","choices":[{"index":0,"message":{"role":"assistant",` +
		`"content":"the answer","reasoning_content":"` + strings.Repeat("thinking ", 400) + `"},` +
		`"finish_reason":"stop"}],"usage":{"prompt_tokens":52,"completion_tokens":1767,"total_tokens":1819}}`)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rewriteThinkResponse(body)
	}
}

// chatBody builds a request body of roughly the shape and size viiwork sees.
func chatBody(promptTokens int, withTask bool) []byte {
	task := ""
	if withTask {
		task = `"task":"benchmark-task",`
	}
	prompt := strings.Repeat("the quick brown fox jumps over the lazy dog ", promptTokens/9)
	return []byte(fmt.Sprintf(`{"model":"Laguna-XS-2.1-Q4_K_M",%s"messages":[`+
		`{"role":"system","content":"You are a helpful assistant."},`+
		`{"role":"user","content":%q}],"max_tokens":400,"temperature":0.7}`, task, prompt))
}

// BenchmarkExtractRequestFields measures what handleProxy pays just to learn the
// model name: a full json.Unmarshal over the entire body, prompt included.
func BenchmarkExtractRequestFields(b *testing.B) {
	for _, tokens := range []int{512, 16384} {
		body := chatBody(tokens, false)
		b.Run(fmt.Sprintf("prompt=%dtok", tokens), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var reqBody struct {
					Model string `json:"model"`
					Think *bool  `json:"think"`
					Task  string `json:"task"`
				}
				json.Unmarshal(body, &reqBody)
			}
		})
	}
}

// BenchmarkStripTaskField measures the unmarshal-into-map + re-marshal round trip
// that runs whenever a request carries a "task" field.
func BenchmarkStripTaskField(b *testing.B) {
	body := chatBody(16384, true)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var generic map[string]json.RawMessage
		if err := json.Unmarshal(body, &generic); err == nil {
			delete(generic, "task")
			json.Marshal(generic)
		}
	}
}

// BenchmarkReadBody measures io.ReadAll on an un-presized buffer, which is how
// handleProxy currently buffers the request.
func BenchmarkReadBody(b *testing.B) {
	body := chatBody(16384, false)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		readBodyPresized(r.Body, r.ContentLength)
	}
}

// BenchmarkExtractModelFast measures the early-stopping extraction against the
// full unmarshal it replaces, on the same bodies.
func BenchmarkExtractModelFast(b *testing.B) {
	for _, tokens := range []int{512, 16384} {
		body := chatBody(tokens, false)
		b.Run(fmt.Sprintf("prompt=%dtok", tokens), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, ok := extractModelFast(body); !ok {
					b.Fatal("fast path did not engage")
				}
			}
		})
	}
}

// BenchmarkExtractModelFastFallback measures the cost when the fast path bails —
// a body carrying "task" must fall through to the full unmarshal, and the wasted
// scan is what that costs.
func BenchmarkExtractModelFastFallback(b *testing.B) {
	body := chatBody(16384, true)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := extractModelFast(body); ok {
			b.Fatal("fast path should not engage when task is present")
		}
	}
}

// BenchmarkCaptureWriter measures what output capture adds to the streaming
// path. The SSE benchmarks above call streamThinkDisabled directly and so do
// not see the wrapper at all; this exercises it the way handleProxy does, one
// Write per SSE chunk.
func BenchmarkCaptureWriter(b *testing.B) {
	chunk := []byte(`data: {"choices":[{"delta":{"content":" token"}}]}` + "\n\n")
	const chunks = 512

	// Split deliberately: "writes" is the part that sits on the per-token path
	// and is the number to watch, while "writes+extract" adds the one-shot
	// parse that runs once per request after the client is already served.
	b.Run("bare-writes", func(b *testing.B) {
		b.SetBytes(int64(len(chunk) * chunks))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var w http.ResponseWriter = discardWriter{}
			for j := 0; j < chunks; j++ {
				w.Write(chunk)
			}
		}
	})

	b.Run("capture-writes", func(b *testing.B) {
		b.SetBytes(int64(len(chunk) * chunks))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			w, _ := newCaptureWriter(discardWriter{})
			for j := 0; j < chunks; j++ {
				w.Write(chunk)
			}
		}
	})

	b.Run("capture-writes+extract", func(b *testing.B) {
		b.SetBytes(int64(len(chunk) * chunks))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			w, capw := newCaptureWriter(discardWriter{})
			for j := 0; j < chunks; j++ {
				w.Write(chunk)
			}
			_ = capw.Output()
		}
	})
}

// discardWriter is a flushing ResponseWriter that does nothing, so the
// benchmark measures the wrapper rather than the recorder underneath it.
type discardWriter struct{}

func (discardWriter) Header() http.Header         { return http.Header{} }
func (discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (discardWriter) WriteHeader(int)             {}
func (discardWriter) Flush()                      {}
