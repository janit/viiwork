package accept

// Config validation for the vLLM and FreeToken engines, through the same path
// a node takes. engines_test.go registers all three.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openEnv is an environment with no mesh secret, to match the open mesh these
// fixtures declare: C1 refuses a config that is both secured and open.
var openEnv = envWith(nil)

// writeEngineConfig writes a one-model config for engine eng with the given
// models[] lines appended, and returns its path.
func writeEngineConfig(t *testing.T, eng, modelLines string) string {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("node:\n  name: node-a\n  state_dir: " + filepath.Join(dir, "state") + "\n")
	b.WriteString("mesh:\n  network: tailnet\n  open: true\n")
	b.WriteString("models:\n  - name: m\n    engine: " + eng + "\n    path: /models/m\n")
	b.WriteString(modelLines)
	path := filepath.Join(dir, "viiwork.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// check returns the named check, so a test can assert on the one rule it is
// about rather than on every check in the report. These fixtures point at
// weights that do not exist, so the whole report never passes.
func check(r Report, name string) Check {
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	return Check{Name: name, Detail: "no such check in the report"}
}

// SPIKE FINDING F6, resolved here rather than in prose.
//
// FreeToken's ValidateOptions rejects a backend with more than one card,
// because the engine runs one card per process. The finding was that the rule
// is unstatable without knowing what Spec.GPUs holds at VALIDATION time — one
// backend's cards, or the model's whole gpus: list. If it were the whole list,
// this ordinary three-card config would be rejected on day one.
//
// config.ModelSpec passes BackendGPUs(0), so it is one backend's cards, and
// this test is what keeps that true.
func TestFreeTokenAcceptsOneCardPerBackendAcrossManyCards(t *testing.T) {
	path := writeEngineConfig(t, "freetoken",
		"    gpus: [0, 1, 2]\n    gpus_per_backend: 1\n    parallel: 4\n    context: 32768\n")

	_, r := SummarizeConfig(path, openEnv, "", "")

	if c := check(r, "config valid"); !c.Pass {
		t.Fatalf("three single-card FreeToken backends must validate: %s", c.Detail)
	}
}

// The rule still has to bite: two cards in one backend is what the engine
// cannot do, and the error names the field the operator has to change.
func TestFreeTokenRejectsTwoCardsInOneBackend(t *testing.T) {
	path := writeEngineConfig(t, "freetoken",
		"    gpus: [0, 1]\n    gpus_per_backend: 2\n    parallel: 4\n    context: 32768\n")

	_, r := SummarizeConfig(path, openEnv, "", "")

	c := check(r, "config valid")
	if c.Pass {
		t.Fatal("a two-card FreeToken backend must be refused: the engine rejects more than one card per process")
	}
	if !strings.Contains(c.Detail, "gpus_per_backend") {
		t.Errorf("the error must name gpus_per_backend, got: %s", c.Detail)
	}
}

// vLLM is the opposite case: it splits one model across its backend's cards,
// so many cards in one backend is the normal configuration.
func TestVLLMAcceptsManyCardsInOneBackend(t *testing.T) {
	path := writeEngineConfig(t, "vllm",
		"    gpus: [0, 1]\n    gpus_per_backend: 2\n    parallel: 8\n    context: 16384\n")

	_, r := SummarizeConfig(path, openEnv, "", "")

	if c := check(r, "config valid"); !c.Pass {
		t.Fatalf("a two-card vLLM backend must validate: %s", c.Detail)
	}
}

// Neither engine runs on CPU, so omitting gpus: is refused rather than started
// as a CPU backend that would never load.
func TestNewEnginesRequireGPUs(t *testing.T) {
	for _, eng := range []string{"vllm", "freetoken"} {
		t.Run(eng, func(t *testing.T) {
			path := writeEngineConfig(t, eng, "    parallel: 2\n    context: 8192\n")

			_, r := SummarizeConfig(path, openEnv, "", "")

			c := check(r, "config valid")
			if c.Pass {
				t.Fatalf("%s has no gpus: and does not run on CPU; it must be refused", eng)
			}
			if !strings.Contains(c.Detail, "gpus") {
				t.Errorf("the error must name gpus, got: %s", c.Detail)
			}
		})
	}
}

// Acceptance wants a multi-card backend to allow at least 45m to load, and
// vLLM's engine default is 20m (v2.2 Decision 10). So a tensor-parallel vLLM
// model that says nothing about startup_timeout is flagged — correctly: 20m is
// the single-card figure, and a 2-card backend also has to shard the
// checkpoint before it profiles and captures CUDA graphs. The operator sets
// startup_timeout, and this test pins that they are told to.
func TestVLLMMultiCardIsToldToSetAStartupTimeout(t *testing.T) {
	path := writeEngineConfig(t, "vllm",
		"    gpus: [0, 1]\n    gpus_per_backend: 2\n    parallel: 8\n    context: 16384\n")

	_, r := SummarizeConfig(path, openEnv, "", "")

	if c := check(r, "startup_timeout m"); c.Pass {
		t.Fatal("a 2-card vLLM backend on the engine default must be flagged: 20m is the single-card figure")
	}

	// And saying so silences it.
	path = writeEngineConfig(t, "vllm",
		"    gpus: [0, 1]\n    gpus_per_backend: 2\n    parallel: 8\n    context: 16384\n    startup_timeout: 45m\n")

	_, r = SummarizeConfig(path, openEnv, "", "")

	if c := check(r, "startup_timeout m"); !c.Pass {
		t.Errorf("an explicit 45m must satisfy the rule, got: %s", c.Detail)
	}
}

// replaceInFile rewrites one exact substring in a file the test wrote.
func replaceInFile(t *testing.T, path, old, new string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, old) {
		t.Fatalf("%q not found in %s", old, path)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(s, old, new, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}
