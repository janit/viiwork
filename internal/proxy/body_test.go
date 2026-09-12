package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestExtractModelFastEscapedKeys guards a hole the byte-level think/task check
// cannot see on its own: JSON permits unicode escapes in keys, so "think"
// is a legitimate spelling of "think" that bytes.Contains will not match. Taking
// the fast path there would silently discard a think/task the client did send.
func TestExtractModelFastEscapedKeys(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool // should the fast path engage?
	}{
		{"plain model only", `{"model":"m","messages":[]}`, true},
		{"plain think present", `{"model":"m","think":true}`, false},
		{"escaped think key", `{"model":"m","\u0074hink":true}`, false},
		{"escaped task key", `{"model":"m","\u0074ask":"x"}`, false},
		{"duplicate model keys", `{"model":"a","model":"b"}`, false},
		{"nested before model", `{"messages":[{"role":"user"}],"model":"m"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := extractModelFast([]byte(tt.body))
			if ok != tt.want {
				t.Errorf("extractModelFast engaged=%v, want %v", ok, tt.want)
			}
		})
	}
}

// TestExtractModelFastMatchesUnmarshal asserts the fast path never disagrees
// with the full unmarshal it is standing in for.
func TestExtractModelFastMatchesUnmarshal(t *testing.T) {
	bodies := []string{
		`{"model":"Laguna-XS-2.1-Q4_K_M","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","think":true}`,
		`{"model":"m","\u0074hink":true}`,
		`{"messages":[],"model":"m"}`,
		`{"model":"a","model":"b"}`,
		`{"model":"unicode <brackets>","max_tokens":5}`,
	}
	for _, body := range bodies {
		var ref struct {
			Model string `json:"model"`
			Think *bool  `json:"think"`
			Task  string `json:"task"`
		}
		if err := json.Unmarshal([]byte(body), &ref); err != nil {
			t.Fatalf("setup: %v", err)
		}
		got, ok := extractModelFast([]byte(body))
		if !ok {
			continue // fell back to the reference path, which is always correct
		}
		if got != ref.Model {
			t.Errorf("body %s: fast=%q unmarshal=%q", body, got, ref.Model)
		}
		if ref.Think != nil || ref.Task != "" {
			t.Errorf("body %s: fast path engaged despite think/task present", body)
		}
	}
}

// TestReadBodyPresizedLyingContentLength covers a client that advertises far
// more than it sends. The read must still be correct, and must not be sized by
// the advertised number.
func TestReadBodyPresizedLyingContentLength(t *testing.T) {
	body := []byte(`{"model":"m"}`)
	got, err := readBodyPresized(bytes.NewReader(body), 32<<20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("got %q, want %q", got, body)
	}
	if c := cap(got); c > presizeCap+bytes.MinRead {
		t.Errorf("allocated cap %d from a lying Content-Length; cap should be bounded by presizeCap=%d", c, presizeCap)
	}
}

// TestReadBodyPresizedUnderstatedContentLength covers the opposite lie: a body
// longer than advertised must be read in full, not truncated.
func TestReadBodyPresizedUnderstatedContentLength(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 100000)
	got, err := readBodyPresized(bytes.NewReader(body), 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != len(body) {
		t.Errorf("got %d bytes, want %d — body was truncated to Content-Length", len(got), len(body))
	}
}

func TestSanitizeHost(t *testing.T) {
	for _, in := range []string{"", "mesh", "MESH", "  "} {
		if got, ok := sanitizeHost(in); got != "" || !ok {
			t.Errorf("sanitizeHost(%q) = %q, %v; want no pin", in, got, ok)
		}
	}
	for _, in := range []string{"gb2", "100.64.0.1", "[fd7a::1]", "gb2.example.ts.net"} {
		if got, ok := sanitizeHost(in); got != in || !ok {
			t.Errorf("sanitizeHost(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"gb2/x", "a b", strings.Repeat("a", 254)} {
		if got, ok := sanitizeHost(in); got != "" || ok {
			t.Errorf("sanitizeHost(%q) = %q, %v; want rejected", in, got, ok)
		}
	}
}

func TestSanitizeTaskID(t *testing.T) {
	if got := sanitizeTaskID("  ab\x01c  "); got != "abc" {
		t.Errorf("sanitizeTaskID = %q, want abc", got)
	}
	long := strings.Repeat("abcd", 10)
	if got := sanitizeTaskID(long); got != long[:32] {
		t.Errorf("sanitizeTaskID(40 chars) = %q, want the first 32", got)
	}
}

func TestExtractPromptText(t *testing.T) {
	chat := []byte(`{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`)
	if got := extractPromptText(chat); got != "c" {
		t.Errorf("chat prompt = %q, want c", got)
	}
	if got := extractPromptText([]byte(`{"prompt":"p"}`)); got != "p" {
		t.Errorf("completion prompt = %q, want p", got)
	}
}
