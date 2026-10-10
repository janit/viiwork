package strata

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/engine/enginetest"
	"github.com/janit/viiwork/v2/internal/gpu"
	"gopkg.in/yaml.v3"
)

// goodConfig is gb3's working config reduced to the keys this engine reads,
// plus two it must ignore (port, gpu).
const goodConfig = `{
 "exe": "/s/Strata/build-906/strata",
 "args": ["--pack", "/s/data/packs/coder-iq1_m", "--max-context", "131072", "--kv", "int8"],
 "cwd": "/s/Strata",
 "model_name": "qwen3.8-flash-next-coder",
 "port": 8080,
 "host": "0.0.0.0",
 "gpu": [0, 1, 2, 3, 4]
}`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "strata.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func spec(t *testing.T, s engine.Spec, block string) engine.Spec {
	t.Helper()
	if block == "" {
		return s
	}
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(block), &node); err != nil {
		t.Fatalf("parse options block: %v", err)
	}
	s.Options = *node.Content[0]
	return s
}

func coderSpec(path string) engine.Spec {
	return engine.Spec{
		Name: "qwen3.8-flash-next-coder", Path: path, GPUs: []int{0, 1, 2, 3, 4},
		Port: 18080, Context: 131072, Parallel: 1, Backends: 2,
	}
}

func TestConfigCheck(t *testing.T) {
	for _, tc := range []struct {
		name    string
		json    string
		mutate  func(*engine.Spec)
		wantErr string // "" = must pass
	}{
		{name: "gb3 config passes", json: goodConfig},
		{name: "name by alias list",
			json:   strings.Replace(goodConfig, `"port": 8080`, `"aliases": ["coder", "fleet-coder"], "port": 8080`, 1),
			mutate: func(s *engine.Spec) { s.Name = "fleet-coder" }},
		{name: "name by alias string",
			json:   strings.Replace(goodConfig, `"port": 8080`, `"aliases": "coder, fleet-coder", "port": 8080`, 1),
			mutate: func(s *engine.Spec) { s.Name = "fleet-coder" }},
		{name: "absent model_name is upstream's default",
			json:   `{"args": ["--max-context", "131072"]}`,
			mutate: func(s *engine.Spec) { s.Name = "qwen3.8-flash-next" }},
		{name: "byte order mark", json: "\xef\xbb\xbf" + goodConfig},
		{name: "wrong name", json: goodConfig,
			mutate:  func(s *engine.Spec) { s.Name = "other" },
			wantErr: `"other"`},
		{name: "aliases of the wrong type",
			json:    strings.Replace(goodConfig, `"port": 8080`, `"aliases": 7, "port": 8080`, 1),
			wantErr: `"aliases"`},
		{name: "context differs", json: goodConfig,
			mutate:  func(s *engine.Spec) { s.Context = 65536 },
			wantErr: "--max-context"},
		{name: "no --max-context",
			json:    `{"model_name": "qwen3.8-flash-next-coder", "args": ["--kv", "int8"]}`,
			wantErr: "--max-context"},
		{name: "--max-context without a value",
			json:    `{"model_name": "qwen3.8-flash-next-coder", "args": ["--max-context"]}`,
			wantErr: "--max-context"},
		{name: "--max-context not a number",
			json:    `{"model_name": "qwen3.8-flash-next-coder", "args": ["--max-context", "big"]}`,
			wantErr: "--max-context"},
		// Upstream's server reads the first and the engine may read the last:
		// two of them is a file the node cannot interpret.
		{name: "--max-context twice",
			json:    `{"model_name": "qwen3.8-flash-next-coder", "args": ["--max-context", "4096", "--max-context", "131072"]}`,
			wantErr: "more than once"},
		// Strata v0.1.39 reads only the two-argument form: its engine exits on
		// --max-context=N as an unknown argument.
		{name: "--max-context=N in one arg",
			json:    `{"model_name": "qwen3.8-flash-next-coder", "args": ["--max-context=131072"]}`,
			wantErr: "one argument"},
		{name: "--max-context in both forms",
			json:    `{"model_name": "qwen3.8-flash-next-coder", "args": ["--max-context=4096", "--max-context", "131072"]}`,
			wantErr: "one argument"},
		{name: "model_name null",
			json:    `{"model_name": null, "args": ["--max-context", "131072"]}`,
			mutate:  func(s *engine.Spec) { s.Name = "qwen3.8-flash-next" },
			wantErr: `"model_name"`},
		// Upstream looks keys up exactly; Go's decoder would not.
		{name: "keys are matched exactly",
			json:    `{"Model_Name": "qwen3.8-flash-next-coder", "args": ["--max-context", "131072"]}`,
			wantErr: `"qwen3.8-flash-next"`},
		// With a key set, /health stays an open 200 while /slots and every
		// request answer 401: healthy to the node, useless to the mesh.
		{name: "api_key",
			json:    strings.Replace(goodConfig, `"port": 8080`, `"api_key": "s3cret", "port": 8080`, 1),
			wantErr: `"api_key"`},
		{name: "empty api_key is fine",
			json: strings.Replace(goodConfig, `"port": 8080`, `"api_key": "", "port": 8080`, 1)},
		{name: "idle_unload_s true is one second upstream",
			json:    strings.Replace(goodConfig, `"port": 8080`, `"idle_unload_s": true, "port": 8080`, 1),
			wantErr: `"idle_unload_s"`},
		{name: "idle_unload_s negative is on upstream",
			json:    strings.Replace(goodConfig, `"port": 8080`, `"idle_unload_s": -5, "port": 8080`, 1),
			wantErr: `"idle_unload_s"`},
		{name: "idle_unload_s null is fine",
			json: strings.Replace(goodConfig, `"port": 8080`, `"idle_unload_s": null, "port": 8080`, 1)},
		// Upstream ignores "parallel" when the args set the batch themselves,
		// and the engine does not guess what a raw engine flag means: /slots
		// reports the real count once the backend is up.
		{name: "--batch in args: parallel is not checked",
			json:   `{"model_name": "qwen3.8-flash-next-coder", "parallel": 4, "args": ["--max-context", "131072", "--batch", "2"]}`,
			mutate: func(s *engine.Spec) { s.Parallel = 2 }},
		{name: "--slots in args: parallel is not checked",
			json: `{"model_name": "qwen3.8-flash-next-coder", "parallel": 4, "args": ["--max-context", "131072", "--slots", "2"]}`},
		{name: "each name is quoted on its own",
			json:    strings.Replace(goodConfig, `"port": 8080`, `"aliases": ["a, m"], "port": 8080`, 1),
			mutate:  func(s *engine.Spec) { s.Name = "m" },
			wantErr: `"qwen3.8-flash-next-coder", "a, m"`},
		{name: "parallel 4 matches",
			json:   strings.Replace(goodConfig, `"port": 8080`, `"parallel": 4, "port": 8080`, 1),
			mutate: func(s *engine.Spec) { s.Parallel = 4 }},
		{name: "parallel differs",
			json:    strings.Replace(goodConfig, `"port": 8080`, `"parallel": 4, "port": 8080`, 1),
			wantErr: `"parallel"`},
		{name: "node wants 2, file says nothing", json: goodConfig,
			mutate:  func(s *engine.Spec) { s.Parallel = 2 },
			wantErr: `"parallel"`},
		{name: "parallel true is one slot upstream",
			json: strings.Replace(goodConfig, `"port": 8080`, `"parallel": true, "port": 8080`, 1)},
		{name: "parallel 2.5 is one slot upstream",
			json: strings.Replace(goodConfig, `"port": 8080`, `"parallel": 2.5, "port": 8080`, 1)},
		{name: "parallel 1",
			json: strings.Replace(goodConfig, `"port": 8080`, `"parallel": 1, "port": 8080`, 1)},
		{name: "parallel above upstream's maximum is 8",
			json:   strings.Replace(goodConfig, `"port": 8080`, `"parallel": 12, "port": 8080`, 1),
			mutate: func(s *engine.Spec) { s.Parallel = 8 }},
		{name: "lazy_load",
			json:    strings.Replace(goodConfig, `"port": 8080`, `"lazy_load": true, "port": 8080`, 1),
			wantErr: `"lazy_load"`},
		{name: "lazy_load false is fine",
			json: strings.Replace(goodConfig, `"port": 8080`, `"lazy_load": false, "port": 8080`, 1)},
		{name: "idle_unload_s",
			json:    strings.Replace(goodConfig, `"port": 8080`, `"idle_unload_s": 600, "port": 8080`, 1),
			wantErr: `"idle_unload_s"`},
		{name: "idle_unload_s as a string",
			json:    strings.Replace(goodConfig, `"port": 8080`, `"idle_unload_s": "600", "port": 8080`, 1),
			wantErr: `"idle_unload_s"`},
		{name: "idle_unload_s zero is fine",
			json: strings.Replace(goodConfig, `"port": 8080`, `"idle_unload_s": 0, "port": 8080`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := coderSpec(writeConfig(t, tc.json))
			if tc.mutate != nil {
				tc.mutate(&s)
			}
			c, err := readConfig(s.Path)
			if err == nil {
				err = c.check(s)
			}
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("no error, want one naming %s", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not name %s", err, tc.wantErr)
			}
		})
	}
}

func TestReadConfigErrors(t *testing.T) {
	if _, err := readConfig(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("a missing file must be an error")
	}
	for name, body := range map[string]string{"not json": "exe = strata", "a list": `["a"]`, "empty": ""} {
		p := writeConfig(t, body)
		if _, err := readConfig(p); err == nil || !strings.Contains(err.Error(), p) {
			t.Errorf("%s: error %v must name the file", name, err)
		}
	}
	// Every other engine's path is the model. Reading one of those into the
	// node to find out it is not JSON would cost the host its memory.
	big := filepath.Join(t.TempDir(), "model.gguf")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(3 << 30); err != nil { // sparse: no disk is used
		t.Fatal(err)
	}
	f.Close()
	if _, err := readConfig(big); err == nil || !strings.Contains(err.Error(), "not the model") {
		t.Errorf("a 3 GiB file: error %v, want one saying the path is the JSON config, not the model", err)
	}
	if _, err := readConfig(t.TempDir()); err == nil {
		t.Error("a directory must be an error")
	}
}

func TestValidateOptions(t *testing.T) {
	e := New()
	base := engine.Spec{Name: "m", Path: "/x.json", GPUs: []int{0, 1}, Context: 4096, Parallel: 1}
	for _, tc := range []struct{ name, block, wantErr string }{
		{"dir alone", "dir: /opt/strata\n", ""},
		{"both keys", "python: /opt/strata/.venv/bin/python\ndir: /opt/strata\n", ""},
		{"no block", "", "models[3].strata.dir"},
		{"empty dir", "dir: \"\"\n", "models[3].strata.dir"},
		{"empty python", "python: \"\"\ndir: /opt/strata\n", "models[3].strata.python"},
		{"unknown key", "dir: /opt/strata\nbinary: strata\n", "models[3].strata: unknown key binary"},
		// Python resolves a relative directory against wherever the node
		// happened to be started.
		{"relative dir", "dir: strata\n", "models[3].strata.dir"},
		{"warmup", "dir: /opt/strata\nwarmup: 45s\n", ""},
		{"warmup off", "dir: /opt/strata\nwarmup: 0s\n", ""},
		{"negative warmup", "dir: /opt/strata\nwarmup: -1s\n", "models[3].strata.warmup"},
		{"warmup too long", "dir: /opt/strata\nwarmup: 30m\n", "models[3].strata.warmup"},
		{"warmup without a unit", "dir: /opt/strata\nwarmup: soon\n", "models[3].strata: warmup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := e.ValidateOptions("models[3]", spec(t, base, tc.block))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

const block = "dir: /opt/strata\n"

func TestConformance(t *testing.T) {
	conf := `{"model_name": "conformance-model", "args": ["--max-context", "4096"]}`
	enginetest.Run(t, New(),
		enginetest.Case{
			Name:         "radeon, five cards",
			Spec:         engine.Spec{Path: writeConfig(t, conf), GPUs: []int{5, 6, 7, 8, 9}, Vendor: gpu.VendorAMD},
			Options:      block,
			NameInConfig: true,
		},
		enginetest.Case{
			Name:         "nvidia, two cards, operator args",
			Spec:         engine.Spec{Path: writeConfig(t, conf), GPUs: []int{1, 2}, Vendor: gpu.VendorNVIDIA, Args: []string{"--api-monitor"}},
			Options:      "python: /opt/strata/.venv/bin/python\ndir: /opt/strata\n",
			NameInConfig: true,
		},
	)
}

func TestCommandGoldenAMD(t *testing.T) {
	s := coderSpec(writeConfig(t, goodConfig))
	s.GPUs, s.Vendor, s.Args = []int{5, 6, 7, 8, 9}, gpu.VendorAMD, []string{"--api-monitor"}
	cmd, err := New().Command(spec(t, s, block))
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if cmd.Path != "python3" {
		t.Errorf("Path = %q, want the default python3", cmd.Path)
	}
	want := []string{
		"-m", "serve.server", "--engine", "strata",
		"--config", s.Path,
		"--host", "127.0.0.1", "--port", "18080",
		// Radeon: counted INSIDE the node's ROCR_VISIBLE_DEVICES pin, so the
		// second backend's cards 5..9 are 0..4 to the process.
		"--gpu", "0,1,2,3,4",
		"--api-monitor",
	}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("args\n got: %v\nwant: %v", cmd.Args, want)
	}
	// PYTHONSAFEPATH keeps the node's working directory off sys.path: without
	// it `-m serve.server` would run a serve/ found there before the checkout.
	wantEnv := []string{"PYTHONPATH=/opt/strata", "PYTHONSAFEPATH=1", "PYTHONUNBUFFERED=1"}
	if !slices.Equal(cmd.Env, wantEnv) {
		t.Errorf("env\n got: %v\nwant: %v", cmd.Env, wantEnv)
	}
}

// On NVIDIA the server overwrites CUDA_VISIBLE_DEVICES with its "gpu" list.
// 0..n-1 here would put every backend on the node's first cards.
func TestCommandPassesRealCardsOffAMD(t *testing.T) {
	for _, v := range []gpu.Vendor{gpu.VendorNVIDIA, gpu.VendorNone, ""} {
		s := coderSpec(writeConfig(t, goodConfig))
		s.GPUs, s.Vendor = []int{5, 6}, v
		cmd, err := New().Command(spec(t, s, "python: /opt/strata/.venv/bin/python\ndir: /opt/strata\n"))
		if err != nil {
			t.Fatalf("vendor %q: %v", v, err)
		}
		if i := slices.Index(cmd.Args, "--gpu"); i < 0 || cmd.Args[i+1] != "5,6" {
			t.Errorf("vendor %q: args %v, want --gpu 5,6", v, cmd.Args)
		}
		if cmd.Path != "/opt/strata/.venv/bin/python" {
			t.Errorf("Path = %q, want the python the block names", cmd.Path)
		}
	}
}

func TestCommandRefuses(t *testing.T) {
	good := writeConfig(t, goodConfig)
	for _, tc := range []struct {
		name    string
		spec    engine.Spec
		block   string
		wantErr string
	}{
		{"no cards", func() engine.Spec { s := coderSpec(good); s.GPUs = nil; return s }(), block, "GPU"},
		{"no dir", coderSpec(good), "python: python3\n", "strata.dir"},
		{"missing file", coderSpec(filepath.Join(t.TempDir(), "absent.json")), block, "absent.json"},
		// The rule Case.NameInConfig moved here from the kit.
		{"file names another model", func() engine.Spec { s := coderSpec(good); s.Name = "other"; return s }(), block, good},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New().Command(spec(t, tc.spec, tc.block))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestDeclarations(t *testing.T) {
	e := New()
	if e.Name() != "strata" {
		t.Errorf("Name() = %q", e.Name())
	}
	if e.DefaultStartupTimeout() != 30*time.Minute {
		t.Errorf("DefaultStartupTimeout() = %v, want 30m", e.DefaultStartupTimeout())
	}
	if got := engine.UsageOf(e); got != (engine.UsageReporting{Unasked: true, CachedTokens: true}) {
		t.Errorf("UsageOf = %+v", got)
	}
	if !engine.SeparatesReasoning(e) {
		t.Error("strata streams reasoning as reasoning_content and the answer as content")
	}
	if _, ok := any(e).(engine.CPURunner); ok {
		t.Error("strata must not claim to run on CPU")
	}
	if _, ok := any(e).(engine.Versioner); ok {
		t.Error("strata has no version to read from its binary")
	}
}

// Rules about the model entry itself, checked at config validation and again
// at launch.
func TestSpecRules(t *testing.T) {
	good := writeConfig(t, goodConfig)
	for _, tc := range []struct {
		name    string
		mutate  func(*engine.Spec)
		wantErr string
	}{
		// The engine's batch window holds eight rows; more is not a count
		// the server will run, and less than one is no backend at all.
		{"parallel above the batch window", func(s *engine.Spec) { s.Parallel = 9 }, "models[3].parallel"},
		{"parallel 0", func(s *engine.Spec) { s.Parallel = 0 }, "models[3].parallel"},
		// source: hands the path to the fetch step, and Strata's JSON names
		// the program to run: it must be a file the operator wrote.
		{"no path", func(s *engine.Spec) { s.Path = "" }, "models[3].path"},
		{"--config in args", func(s *engine.Spec) { s.Args = []string{"--config", "/other.json"} }, "models[3].args"},
		{"--config= in args", func(s *engine.Spec) { s.Args = []string{"--config=/other.json"} }, "models[3].args"},
		{"--api-key in args", func(s *engine.Spec) { s.Args = []string{"--api-key", "x"} }, "models[3].args"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := spec(t, coderSpec(good), block)
			tc.mutate(&s)
			if err := New().ValidateOptions("models[3]", s); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ValidateOptions: error %v, want one naming %s", err, tc.wantErr)
			}
			if _, err := New().Command(s); err == nil {
				t.Error("Command built a command line for it")
			}
		})
	}
}

func TestPerfKeyIsTheConfigFilesContent(t *testing.T) {
	e := New()
	a := e.PerfKey(engine.Spec{Path: writeConfig(t, goodConfig)})
	if a == "" || a != e.PerfKey(engine.Spec{Path: writeConfig(t, goodConfig)}) {
		t.Errorf("the same content must give the same, non-empty key; got %q", a)
	}
	if a == e.PerfKey(engine.Spec{Path: writeConfig(t, strings.Replace(goodConfig, "int8", "q4_0", 1))}) {
		t.Error("different content must give a different key")
	}
	// A file that cannot be read adds nothing; Command is what refuses it.
	if k := e.PerfKey(engine.Spec{Path: filepath.Join(t.TempDir(), "absent.json")}); k != "" {
		t.Errorf("an unreadable file gave key %q", k)
	}
}

func TestWarmUp(t *testing.T) {
	e := New()
	base := engine.Spec{Name: "m", Path: "/x.json", GPUs: []int{0, 1}, Context: 4096, Parallel: 1}
	for _, tc := range []struct {
		name, block string
		want        time.Duration
	}{
		{"default", "dir: /opt/strata\n", 30 * time.Second},
		{"set", "dir: /opt/strata\nwarmup: 45s\n", 45 * time.Second},
		{"off", "dir: /opt/strata\nwarmup: 0s\n", 0},
		// Command refuses this block; a warm-up read from it would be a guess.
		{"invalid block", "warmup: 45s\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.WarmUp(spec(t, base, tc.block)); got != tc.want {
				t.Errorf("WarmUp = %s, want %s", got, tc.want)
			}
			if got := engine.WarmUpOf(e, spec(t, base, tc.block)); got != tc.want {
				t.Errorf("WarmUpOf = %s, want %s", got, tc.want)
			}
		})
	}
}

// Since v0.1.41 /slots lists the batch slots, so the node can publish them:
// a model entry and a file that agree on more than one slot give a command.
func TestParallelAboveOne(t *testing.T) {
	path := writeConfig(t, strings.Replace(goodConfig, `"port": 8080`, `"parallel": 2, "port": 8080`, 1))
	s := spec(t, coderSpec(path), block)
	s.Parallel = 2
	if err := New().ValidateOptions("models[0]", s); err != nil {
		t.Fatalf("ValidateOptions: %v", err)
	}
	if _, err := New().Command(s); err != nil {
		t.Fatalf("Command: %v", err)
	}
}
