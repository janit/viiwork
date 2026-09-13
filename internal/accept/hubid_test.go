package accept

// models[].path is not always a filesystem path. vLLM and FreeToken both load
// a HuggingFace repo id directly, and the v1 viiwork-nvidia example shipped
// one — `path: Qwen/Qwen3-32B-AWQ`. Acceptance runs BEFORE v1 is stopped, so a
// false "weights missing" here is the worst possible moment for one.

import (
	"strings"
	"testing"
)

// A hub id is not checked locally, and that is not a failure. The operator is
// converting a working v1 node; telling them their weights are gone is a lie
// that stops a migration.
func TestWeightsCheckAcceptsAHubID(t *testing.T) {
	path := writeEngineConfig(t, "vllm",
		"    gpus: [0, 1]\n    gpus_per_backend: 2\n    parallel: 8\n    context: 16384\n    startup_timeout: 45m\n")
	// The fixture writes path: /models/m; make it a hub id instead.
	replaceInFile(t, path, "path: /models/m", "path: Qwen/Qwen3-32B-AWQ")

	_, r := SummarizeConfig(path, openEnv, "", "")

	c := check(r, "weights m")
	if !c.Pass {
		t.Fatalf("a hub id must not fail the weights check: %s", c.Detail)
	}
	if !strings.Contains(c.Detail, "hub id") {
		t.Errorf("the detail should say it was not checked locally, got %q", c.Detail)
	}
}

// A local path that really is missing must still fail. The fix must not turn
// the check off.
func TestWeightsCheckStillCatchesAMissingLocalPath(t *testing.T) {
	path := writeEngineConfig(t, "vllm",
		"    gpus: [0]\n    parallel: 8\n    context: 16384\n")

	_, r := SummarizeConfig(path, openEnv, "", "")

	if c := check(r, "weights m"); c.Pass {
		t.Fatal("/models/m does not exist; the weights check must still fail on a real path")
	}
}

// The shapes that separate the two. A hub id is org/name — no leading slash,
// no ./, no file extension, exactly one slash. Anything else is a path.
func TestHubIDRecognition(t *testing.T) {
	for _, tc := range []struct {
		path string
		hub  bool
	}{
		{"Qwen/Qwen3-32B-AWQ", true},
		{"Qwen/Qwen2.5-0.5B-Instruct", true},
		{"deepseek-ai/DeepSeek-V4-Flash", true},
		// Paths, every one of which must stay a path.
		{"/models/m.gguf", false},
		{"/models/DSV4-Flash-NVFP4", false},
		{"./models/m", false},
		{"models/m.gguf", false}, // has an extension
		{"m.gguf", false},        // no slash at all
		{"/models/a/b", false},   // absolute, two slashes
		{"a/b/c", false},         // three segments is not org/name
		{"", false},
	} {
		if got := isHubID(tc.path); got != tc.hub {
			t.Errorf("isHubID(%q) = %v, want %v", tc.path, got, tc.hub)
		}
	}
}
