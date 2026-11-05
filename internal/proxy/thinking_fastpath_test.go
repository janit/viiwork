package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// runThinkStream is streamThinkDisabled's output for sse, with the rename
// fast path on or off.
func runThinkStream(t *testing.T, sse string, fast bool) string {
	t.Helper()
	old := renameFastPath
	renameFastPath = fast
	defer func() { renameFastPath = old }()
	w := httptest.NewRecorder()
	if _, err := streamThinkDisabled(w, strings.NewReader(sse), func() {}); err != nil {
		t.Fatal(err)
	}
	return w.Body.String()
}

// sameFrames compares two SSE outputs line by line, data payloads as JSON
// values: the slow path re-encodes with sorted keys, the fast path keeps the
// engine's bytes, and both must say the same thing.
func sameFrames(t *testing.T, fast, slow string) {
	t.Helper()
	fl, sl := strings.Split(fast, "\n"), strings.Split(slow, "\n")
	if len(fl) != len(sl) {
		t.Fatalf("line count differs:\nfast %q\nslow %q", fast, slow)
	}
	for i := range fl {
		f, s := fl[i], sl[i]
		if !strings.HasPrefix(f, "data: {") || !strings.HasPrefix(s, "data: {") {
			if f != s {
				t.Errorf("line %d: fast %q, slow %q", i, f, s)
			}
			continue
		}
		var fv, sv any
		ferr := json.Unmarshal([]byte(f[len("data: "):]), &fv)
		serr := json.Unmarshal([]byte(s[len("data: "):]), &sv)
		if ferr != nil || serr != nil {
			// A chunk that is not JSON is passed through, by both paths.
			if f != s {
				t.Errorf("line %d: fast %q (%v), slow %q (%v)", i, f, ferr, s, serr)
			}
			continue
		}
		if !reflect.DeepEqual(fv, sv) {
			t.Errorf("line %d differs:\nfast %s\nslow %s", i, f, s)
		}
	}
}

// The rename fast path must produce what the slow path produces, for the
// chunks it takes and for the ones it must leave to the slow path.
func TestStreamThinkDisabledRenameFastPathMatchesSlowPath(t *testing.T) {
	chunks := []string{
		`{"id":"a","created":1700000000,"choices":[{"index":0,"delta":{"reasoning_content":"plain token"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"role":"assistant","reasoning_content":"with \"quotes\" and \\ backslash"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"</think> stray close outside a block"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"html <b>&amp;</b> stays"}}],"usage":null}`,
		`{"choices":[{"delta":{"reasoning_content":""}}]}`,
		`{"choices":[{"delta":{"reasoning_content" : "spaced key"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"stop here"},"finish_reason":"stop"}]}`,
		// Left to the slow path:
		`{"choices":[{"delta":{"reasoning_content":"ä escaped"}}]}`,
		`{"choices":[{"delta":{"content":"x","reasoning_content":"y"}}]}`,
		`{"choices":[{"delta":{"content":null,"reasoning_content":"null content"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"a"}},{"delta":{"reasoning_content":"b"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"say \"reasoning_content\" twice"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":7}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"<think>opens"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"inside"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"</think>after"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"tail"}}]}`,
	}
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	sse := b.String()
	fast, slow := runThinkStream(t, sse, true), runThinkStream(t, sse, false)
	sameFrames(t, fast, slow)
	if !strings.Contains(fast, `{"id":"a","created":1700000000,"choices":[{"index":0,"delta":{"content":"plain token"},"finish_reason":null}]}`) {
		t.Errorf("the fast path did not keep the engine's bytes: %q", fast)
	}
}

func TestUntaggedReasoningKeyGuards(t *testing.T) {
	takes := `{"choices":[{"delta":{"reasoning_content":"x"}}]}`
	if at, ok := untaggedReasoningKey(newChunkProbe(), []byte(takes)); !ok || !strings.HasPrefix(takes[at:], `"reasoning_content"`) {
		t.Errorf("plain chunk: at=%d ok=%v", at, ok)
	}
	for _, c := range []string{
		`{"choices":[{"delta":{"reasoning_content":"<think>"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"<think>"}}]}`,
		`{"choices":[{"delta":{"content":"a","reasoning_content":"b"}}]}`,
		`{"choices":[{"delta":{"content":null,"reasoning_content":"b"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":null}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"a"}},{"delta":{"reasoning_content":"b"}}]}`,
		`{"choices":[{"delta":null,"message":{"reasoning_content":"a"}}]}`,
		`not json "reasoning_content"`,
	} {
		if _, ok := untaggedReasoningKey(newChunkProbe(), []byte(c)); ok {
			t.Errorf("fast path taken for %s", c)
		}
	}
}

// runThinkStreamPaths is streamThinkDisabled's output with both fast paths
// on, or both off.
func runThinkStreamPaths(t *testing.T, sse string, fast bool) string {
	t.Helper()
	old := suppressFastPath
	suppressFastPath = fast
	defer func() { suppressFastPath = old }()
	return runThinkStream(t, sse, fast)
}

// Inside a think block the suppress fast path must decide what the slow path
// decides: which chunks are dropped, which close the block, and what a
// truncation salvages.
func TestStreamThinkDisabledSuppressFastPathMatchesSlowPath(t *testing.T) {
	open := `{"id":"s","choices":[{"delta":{"reasoning_content":"<think>"}}]}`
	cases := map[string][]string{
		"plain tokens then close": {
			`{"choices":[{"delta":{"reasoning_content":"one "}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"two"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{"reasoning_content":"</think>answer"}}]}`,
		},
		"escaped close tag": {
			`{"choices":[{"delta":{"reasoning_content":"one"}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"\u003c/think\u003eanswer"}}]}`,
			`{"choices":[{"delta":{"content":"after"}}]}`,
		},
		"reopen resets the buffer": {
			`{"choices":[{"delta":{"reasoning_content":"dropped"}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"\u003cthink\u003eagain"}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"kept"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"length"}]}`,
		},
		"truncated inside the block": {
			`{"choices":[{"delta":{"reasoning_content":"The answer is 42."}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"x"},"finish_reason":"length"}]}`,
			`{"choices":[{"delta":{},"finish_reason":"length"}]}`,
		},
		"shapes left to the slow path": {
			`{"choices":[{"delta":{"content":"c","reasoning_content":"r"}}]}`,
			`{"choices":[{"delta":{"content":null,"reasoning_content":"null content"}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"a"}},{"delta":{"reasoning_content":"b"}}]}`,
			`{"choices":[{"delta":{"reasoning_content":7}}]}`,
			`{"choices":[{"delta":{"reasoning_content":null}}]}`,
			`{"Choices":[{"delta":{"reasoning_content":"case"}}]}`,
			`{"choices":[{"Delta":{"reasoning_content":"case"}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"dup","reasoning_content":"licate"}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"trailing"}}]} x`,
			`{"choices":[{"delta":{"reasoning_content":"keep"},"finish_reason":""}]}`,
			`{"choices":[{"delta":{"reasoning_content":"end"},"finish_reason":"length"}]}`,
		},
	}
	for name, chunks := range cases {
		t.Run(name, func(t *testing.T) {
			var b strings.Builder
			for _, c := range append([]string{open}, chunks...) {
				b.WriteString("data: " + c + "\n\n")
			}
			b.WriteString("data: [DONE]\n\n")
			sse := b.String()
			sameFrames(t, runThinkStreamPaths(t, sse, true), runThinkStreamPaths(t, sse, false))
		})
	}
}
