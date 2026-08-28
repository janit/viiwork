package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// maxCaptureBytes bounds the raw response bytes retained per request for the
// dashboard's output panel.
//
// The number looks generous because SSE is verbose: llama-server wraps every
// few characters of text in a ~200-byte JSON envelope, so the ratio of stream
// bytes to answer text runs around 50:1. 2 MB of raw stream is therefore only
// tens of thousands of characters of answer — roughly where activity's own
// maxPromptChars truncates anyway. Live memory is this times the number of
// requests in flight, which backpressure already bounds.
const maxCaptureBytes = 2 << 20

// captureWriter tees a proxied response into a bounded buffer on its way to
// the client, so the finished text can be recorded for the prompt history.
//
// It captures raw bytes and parses them once, after the response completes,
// rather than decoding each SSE chunk as it passes. That ordering is
// deliberate: the streaming loop is the per-token hot path, and the think
// rewriter goes out of its way to avoid decoding chunks it does not have to
// (see streamThinkDisabled's fast path). Adding a JSON decode per token here
// would give that back. An append into a byte slice is a memcpy, and once the
// cap is reached even that stops.
//
// Wrapping the outer ResponseWriter also means what gets captured is what the
// client actually received — after think-block rewriting, not before.
type captureWriter struct {
	http.ResponseWriter
	buf      []byte
	status   int
	overflow bool
}

func (c *captureWriter) WriteHeader(status int) {
	c.status = status
	c.ResponseWriter.WriteHeader(status)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if !c.overflow {
		if room := maxCaptureBytes - len(c.buf); room > 0 {
			if len(b) <= room {
				c.buf = append(c.buf, b...)
			} else {
				c.buf = append(c.buf, b[:room]...)
				c.overflow = true
			}
		} else {
			c.overflow = true
		}
	}
	return c.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer, should anything
// downstream ever need it.
func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// flushCaptureWriter is captureWriter for an underlying writer that flushes.
// The split is not cosmetic: both proxyRequest and streamThinkDisabled branch
// on w.(http.Flusher), and streamThinkDisabled degrades to a plain io.Copy
// when the assertion fails. A wrapper that always advertised Flush would make
// a non-flushing writer look flushable; one that never did would silently turn
// streaming responses into buffered ones.
type flushCaptureWriter struct {
	*captureWriter
	f http.Flusher
}

func (c *flushCaptureWriter) Flush() { c.f.Flush() }

// newCaptureWriter wraps w, preserving whether it can flush.
func newCaptureWriter(w http.ResponseWriter) (http.ResponseWriter, *captureWriter) {
	c := &captureWriter{ResponseWriter: w, status: http.StatusOK}
	if f, ok := w.(http.Flusher); ok {
		return &flushCaptureWriter{captureWriter: c, f: f}, c
	}
	return c, c
}

// Output returns the assistant text the response carried, or — for a response
// that failed — the error body itself, which is the more useful thing to see
// in the dashboard when a request went wrong.
func (c *captureWriter) Output() string {
	out := extractOutputText(c.buf)
	if out == "" && c.status >= 400 {
		return strings.TrimSpace(string(c.buf))
	}
	if c.overflow && out != "" {
		return out + "\n\n... [output truncated: response exceeded the capture limit]"
	}
	return out
}

// completionShape covers every response body this proxy forwards, in both
// their streaming and non-streaming spellings: chat completions carry the text
// under delta (streaming) or message (whole), plain completions carry it under
// text. Unused fields simply stay empty, so one struct decodes all of them.
type completionShape struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
		Text string `json:"text"`
	} `json:"choices"`
}

var sseDataPrefix = []byte("data:")

// extractOutputText reassembles the answer from a captured response body,
// which is either an SSE stream of deltas or a single JSON object.
//
// Reasoning is kept, labelled and separated from the answer rather than
// concatenated into it. A thinking model with think enabled puts everything in
// reasoning_content and leaves content empty, so dropping reasoning would show
// a blank output for exactly the requests most worth inspecting; merging the
// two silently would misrepresent what the client received.
func extractOutputText(raw []byte) string {
	var content, reasoning strings.Builder

	if bytes.HasPrefix(bytes.TrimLeft(raw, " \r\n"), sseDataPrefix) {
		for _, line := range bytes.Split(raw, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, sseDataPrefix) {
				continue
			}
			payload := bytes.TrimSpace(line[len(sseDataPrefix):])
			if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
				continue
			}
			appendChoiceText(payload, &content, &reasoning)
		}
	} else {
		appendChoiceText(raw, &content, &reasoning)
	}

	answer := strings.TrimSpace(content.String())
	think := strings.TrimSpace(reasoning.String())
	switch {
	case think == "":
		return answer
	case answer == "":
		return "[reasoning]\n" + think
	default:
		return "[reasoning]\n" + think + "\n\n[answer]\n" + answer
	}
}

// appendChoiceText decodes one JSON body or SSE payload and appends whatever
// text it carries. A payload that does not decode is skipped rather than
// reported: this runs off the request path on best-effort telemetry, and a
// backend emitting something unexpected should not cost anything visible.
func appendChoiceText(payload []byte, content, reasoning *strings.Builder) {
	var c completionShape
	if json.Unmarshal(payload, &c) != nil {
		return
	}
	for _, ch := range c.Choices {
		content.WriteString(ch.Delta.Content)
		content.WriteString(ch.Message.Content)
		content.WriteString(ch.Text)
		reasoning.WriteString(ch.Delta.ReasoningContent)
		reasoning.WriteString(ch.Message.ReasoningContent)
	}
}
