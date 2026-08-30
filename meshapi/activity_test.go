package meshapi

import (
	"testing"
	"time"
)

func TestBackendLabel(t *testing.T) {
	if got := BackendLabel(3, nil); got != "gpu-3" {
		t.Errorf("single GPU: got %q, want gpu-3", got)
	}
	// A tensor-split group has no single GPU id. Labelling it by GPUID alone
	// rendered every group in a multi-group fleet as "gpu--1".
	if got := BackendLabel(-1, []int{4, 5}); got != "ts-4,5" {
		t.Errorf("tensor split: got %q, want ts-4,5", got)
	}
	if got := BackendLabel(-1, []int{7, 8, 9}); got != "ts-7,8,9" {
		t.Errorf("three-way split: got %q, want ts-7,8,9", got)
	}
}

func TestPeerLabel(t *testing.T) {
	if got := PeerLabel("gb2:9302"); got != "peer gb2:9302" {
		t.Errorf("got %q, want peer gb2:9302", got)
	}
}

// These strings are exactly what viiwork v1.5.2 emits. The dashboard splits on
// the arrow and matches the terminal word, so a change here silently strands
// in-flight rows on every node in the fleet.
func TestRequestMessageGrammar(t *testing.T) {
	elapsed := 1234 * time.Millisecond
	tests := []struct{ got, want string }{
		{RequestStarted("qwen", BackendLabel(3, nil)), "qwen → gpu-3"},
		{RequestStarted("qwen", PeerLabel("gb2:9302")), "qwen → peer gb2:9302"},
		{RequestDone("qwen", BackendLabel(3, nil), elapsed), "qwen → gpu-3 done (1.234s)"},
		{RequestDone("qwen", PeerLabel("gb2:9302"), elapsed), "qwen → peer gb2:9302 done (1.234s)"},
		{RequestAborted("qwen", BackendLabel(-1, []int{4, 5}), elapsed), "qwen → ts-4,5 aborted by client (1.234s)"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("got %q, want %q", tt.got, tt.want)
		}
	}
}

func TestIsRequestTerminal(t *testing.T) {
	started := RequestStarted("qwen", "gpu-3")
	if IsRequestTerminal(started) {
		t.Errorf("a start event must not clear the in-flight row: %q", started)
	}
	for _, msg := range []string{
		RequestDone("qwen", "gpu-3", time.Second),
		RequestAborted("qwen", "gpu-3", time.Second),
		"qwen → gpu-3 finished",
		"qwen → gpu-3 DONE (1s)",
	} {
		if !IsRequestTerminal(msg) {
			t.Errorf("terminal message not recognised, row would age forever: %q", msg)
		}
	}
}

// A model whose name contains a terminal word must not be mistaken for a
// finished request at the start event. This is the one case where the loose
// match in IsRequestTerminal could bite, so it is pinned rather than assumed.
func TestModelNameDoesNotFakeCompletion(t *testing.T) {
	if IsRequestTerminal(RequestStarted("qwen-3", "gpu-1")) {
		t.Error("plain start event reported terminal")
	}
}

func TestSplitRequestMessage(t *testing.T) {
	tests := []struct {
		msg, model, dest string
		ok               bool
	}{
		{"qwen → gpu-3", "qwen", "gpu-3", true},
		{"qwen → gpu-3 done (1.234s)", "qwen", "gpu-3", true},
		{"qwen → ts-4,5 aborted by client (2s)", "qwen", "ts-4,5", true},
		{"qwen → peer gb2:9302 done (1s)", "qwen", "peer gb2:9302", true},
		{"backend gpu-3 spawned", "", "", false},
	}
	for _, tt := range tests {
		model, dest, ok := SplitRequestMessage(tt.msg)
		if ok != tt.ok || model != tt.model || dest != tt.dest {
			t.Errorf("SplitRequestMessage(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.msg, model, dest, ok, tt.model, tt.dest, tt.ok)
		}
	}
}

// Round-tripping is what a second implementation actually relies on: it builds
// a message with the helpers, and the reference consumer takes it apart again.
func TestRequestMessageRoundTrip(t *testing.T) {
	for _, dest := range []string{BackendLabel(3, nil), BackendLabel(-1, []int{4, 5}), PeerLabel("gb2:9302")} {
		start := RequestStarted("my-model", dest)
		gotModel, gotDest, ok := SplitRequestMessage(start)
		if !ok || gotModel != "my-model" || gotDest != dest {
			t.Errorf("start round-trip failed for %q: (%q, %q, %v)", dest, gotModel, gotDest, ok)
		}
		done := RequestDone("my-model", dest, 500*time.Millisecond)
		gotModel, gotDest, ok = SplitRequestMessage(done)
		if !ok || gotModel != "my-model" || gotDest != dest {
			t.Errorf("done round-trip failed for %q: (%q, %q, %v)", dest, gotModel, gotDest, ok)
		}
		if !IsRequestTerminal(done) {
			t.Errorf("done not terminal for %q", dest)
		}
	}
}
