package llamacpp

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/gpu"
)

const gib = int64(1) << 30

// parseModel builds models[0] through config.Parse, so per-model defaults
// apply exactly as they do in production.
func parseModel(t *testing.T, yaml string) config.Model {
	t.Helper()
	cfg, err := config.Parse([]byte("models:\n" + yaml))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Models) != 1 {
		t.Fatalf("want one model, got %d", len(cfg.Models))
	}
	return cfg.Models[0]
}

// testEngine fixes the host: CPU count, RAM and model size.
func testEngine(nproc int, ramBytes, modelBytes int64, sizeErr error) *Engine {
	return &Engine{
		nproc:    func() int { return nproc },
		totalRAM: func() int64 { return ramBytes },
		modelSize: func(string) (int64, error) {
			return modelBytes, sizeErr
		},
	}
}

func command(t *testing.T, e *Engine, m config.Model, backend int) engine.Command {
	t.Helper()
	cmd, err := e.Command(engine.Spec{Model: m, GPUs: m.BackendGPUs(backend), Port: 40001, Vendor: gpu.VendorAMD})
	if err != nil {
		t.Fatal(err)
	}
	return cmd
}

// flagValue returns the value after the first occurrence of flag, or "".
func flagValue(args []string, flag string) (string, bool) {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return "", i >= 0
	}
	return args[i+1], true
}

func TestNameAndTimeout(t *testing.T) {
	e := New()
	if e.Name() != config.EngineLlamaCpp || e.DefaultStartupTimeout() != 10*time.Minute {
		t.Errorf("Name=%q DefaultStartupTimeout=%v", e.Name(), e.DefaultStartupTimeout())
	}
}

func TestCommandSingleGPUExact(t *testing.T) {
	m := parseModel(t, `  - name: granite-4.2-8b
    engine: llamacpp
    path: /models/granite-4.2-8b-Q8_0.gguf
    gpus: [2]
    context: 16384
    parallel: 2
    args: ["--jinja"]
`)
	cmd := command(t, testEngine(16, 247*gib, 9*gib, nil), m, 0)
	if cmd.Path != "llama-server" {
		t.Errorf("Path = %q", cmd.Path)
	}
	want := strings.Fields("--model /models/granite-4.2-8b-Q8_0.gguf --alias granite-4.2-8b --host 127.0.0.1 --port 40001 --ctx-size 32768 --parallel 2 --n-gpu-layers -1 --slots --log-disable --threads 16 --jinja")
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("Args =\n %v\nwant\n %v", cmd.Args, want)
	}
	wantEnv := []string{"MALLOC_MMAP_THRESHOLD_=65536", "MALLOC_TRIM_THRESHOLD_=65536", "MALLOC_ARENA_MAX=4"}
	if !slices.Equal(cmd.Env, wantEnv) {
		t.Errorf("Env = %v, want %v", cmd.Env, wantEnv)
	}
}

func TestCommandSplitPairWithWeights(t *testing.T) {
	m := parseModel(t, `  - name: Qwen3.8-27B
    engine: llamacpp
    path: /models/Qwen3.8-27B.gguf
    gpus: [0, 1]
    gpus_per_backend: 2
    context: 49152
    parallel: 2
    llamacpp:
      split_weights: [1.16, 1.0]
`)
	cmd := command(t, testEngine(16, 247*gib, 18*gib, nil), m, 0)
	for flag, want := range map[string]string{"--split-mode": "layer", "--tensor-split": "1.16,1", "--ctx-size": "98304"} {
		if got, _ := flagValue(cmd.Args, flag); got != want {
			t.Errorf("%s = %q, want %q (args %v)", flag, got, want, cmd.Args)
		}
	}
	if _, ok := flagValue(cmd.Args, "--main-gpu"); ok {
		t.Errorf("layer mode must not pass --main-gpu: %v", cmd.Args)
	}
}

func TestCommandRowModeEvenSplit(t *testing.T) {
	m := parseModel(t, `  - name: m
    engine: llamacpp
    path: /models/m.gguf
    gpus: [4, 5]
    gpus_per_backend: 2
    context: 4096
    llamacpp:
      split_mode: row
      main_gpu: 1
`)
	cmd := command(t, testEngine(16, 247*gib, gib, nil), m, 0)
	for flag, want := range map[string]string{"--split-mode": "row", "--tensor-split": "1,1", "--main-gpu": "1"} {
		if got, _ := flagValue(cmd.Args, flag); got != want {
			t.Errorf("%s = %q, want %q (args %v)", flag, got, want, cmd.Args)
		}
	}
}

func TestCommandCPUModel(t *testing.T) {
	m := parseModel(t, "  - name: tiny\n    engine: llamacpp\n    path: /models/tiny.gguf\n    context: 2048\n")
	cmd := command(t, testEngine(4, 16*gib, gib, nil), m, 0)
	if got, _ := flagValue(cmd.Args, "--n-gpu-layers"); got != "0" {
		t.Errorf("--n-gpu-layers = %q, want 0", got)
	}
	for _, flag := range []string{"--split-mode", "--tensor-split", "--main-gpu"} {
		if slices.Contains(cmd.Args, flag) {
			t.Errorf("a CPU backend must not pass %s: %v", flag, cmd.Args)
		}
	}
}

func TestCommandThreads(t *testing.T) {
	const base = "  - name: m\n    engine: llamacpp\n    path: /models/m.gguf\n    context: 4096\n"
	cases := []struct {
		name  string
		yaml  string
		nproc int
		want  string // "" = no generated --threads
	}{
		{"explicit llamacpp.threads", base + "    gpus: [0]\n    llamacpp:\n      threads: 4\n", 16, "4"},
		{"operator -t in args", base + "    gpus: [0]\n    args: [\"-t\", \"8\"]\n", 16, ""},
		{"3 backends on 16 CPUs", base + "    gpus: [0, 1, 2]\n", 16, "5"},
		{"3 backends on 2 CPUs", base + "    gpus: [0, 1, 2]\n", 2, "1"},
	}
	for _, tc := range cases {
		m := parseModel(t, tc.yaml)
		cmd := command(t, testEngine(tc.nproc, 64*gib, gib, nil), m, 0)
		n := 0
		for _, a := range cmd.Args {
			if a == "--threads" {
				n++
			}
		}
		got, _ := flagValue(cmd.Args, "--threads")
		switch {
		case tc.want == "" && n != 0:
			t.Errorf("%s: generated --threads %q, want none (args %v)", tc.name, got, cmd.Args)
		case tc.want != "" && (n != 1 || got != tc.want):
			t.Errorf("%s: --threads %q (%d occurrences), want %q", tc.name, got, n, tc.want)
		}
	}
}

func TestCommandNoMmap(t *testing.T) {
	const base = "  - name: m\n    engine: llamacpp\n    path: /models/m.gguf\n    gpus: [0]\n    context: 4096\n"
	cases := []struct {
		name    string
		args    string
		ram     int64
		model   int64
		sizeErr error
		want    bool
	}{
		{"100 GiB on 46 GiB", "", 46 * gib, 100 * gib, nil, true},
		{"exactly 80% of RAM", "", 100 * gib, 80 * gib, nil, true},
		{"79% of RAM", "", 100 * gib, 79 * gib, nil, false},
		{"operator --mmap", "    args: [\"--mmap\"]\n", 46 * gib, 100 * gib, nil, false},
		{"RAM unknown", "", 0, 100 * gib, nil, false},
		{"size lookup fails", "", 46 * gib, 0, errors.New("stat: permission denied"), false},
	}
	for _, tc := range cases {
		m := parseModel(t, base+tc.args)
		cmd := command(t, testEngine(16, tc.ram, tc.model, tc.sizeErr), m, 0)
		if got := slices.Contains(cmd.Args, "--no-mmap"); got != tc.want {
			t.Errorf("%s: --no-mmap present = %v, want %v (args %v)", tc.name, got, tc.want, cmd.Args)
		}
	}
}

func TestCommandOperatorArgsComeLast(t *testing.T) {
	m := parseModel(t, "  - name: m\n    engine: llamacpp\n    path: /models/m.gguf\n    gpus: [0]\n    context: 4096\n    args: [\"--n-gpu-layers\", \"20\"]\n")
	cmd := command(t, testEngine(16, 64*gib, gib, nil), m, 0)
	var at []int
	for i, a := range cmd.Args {
		if a == "--n-gpu-layers" {
			at = append(at, i)
		}
	}
	if len(at) != 2 || cmd.Args[at[0]+1] != "-1" || cmd.Args[at[1]+1] != "20" {
		t.Errorf("the operator's --n-gpu-layers must follow the generated one (llama.cpp takes the last): %v", cmd.Args)
	}
}

func TestCommandMissingOptionsBlock(t *testing.T) {
	m := config.Model{Name: "handmade", Engine: config.EngineLlamaCpp, Path: "/m.gguf", GPUs: []int{0}, GPUsPerBackend: 1, Context: 1, Parallel: 1}
	_, err := testEngine(4, gib, gib, nil).Command(engine.Spec{Model: m, GPUs: []int{0}, Port: 1})
	if err == nil || !strings.Contains(err.Error(), "handmade") {
		t.Errorf("err = %v, want an error naming the model", err)
	}
}

func TestModelTotalSize(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, size int) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	part1 := write("model-00001-of-00003.gguf", 10)
	write("model-00002-of-00003.gguf", 20)
	write("model-00003-of-00003.gguf", 30)
	if n, err := modelTotalSize(part1); err != nil || n != 60 {
		t.Errorf("multi-part = %d, %v; want 60", n, err)
	}
	if n, err := modelTotalSize(write("single.gguf", 7)); err != nil || n != 7 {
		t.Errorf("single file = %d, %v; want 7", n, err)
	}
	if _, err := modelTotalSize(filepath.Join(dir, "missing.gguf")); err == nil {
		t.Error("a missing file must be an error")
	}
}
