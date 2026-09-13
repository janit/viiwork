package vllm

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/engine/enginetest"
	"github.com/janit/viiwork/v2/internal/gpu"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------- conformance

func TestConformance(t *testing.T) {
	enginetest.Run(t, New(),
		enginetest.Case{
			Name: "single card",
			Spec: engine.Spec{
				Name: "granite-4.2-8b", Path: "/models/granite-4.2-8b-FP8",
				GPUs: []int{0}, Port: 18081, Context: 16384, Parallel: 8,
				Vendor: gpu.VendorNVIDIA,
			},
			Options: "binary: vllm\ngpu_memory_utilization: 0.85\n",
		},
		enginetest.Case{
			Name: "two card tensor parallel",
			Spec: engine.Spec{
				Name: "qwen3-32b", Path: "/models/Qwen3-32B",
				GPUs: []int{2, 3}, Port: 18082, Context: 8192, Parallel: 4,
				Args: []string{"--enforce-eager"}, Vendor: gpu.VendorNVIDIA,
			},
		},
	)
}

// ------------------------------------------------------------- command golden

func TestCommandSingleCard(t *testing.T) {
	s := spec(t, engine.Spec{
		Name: "granite-4.2-8b", Path: "/models/granite-4.2-8b-FP8",
		GPUs: []int{0}, Port: 18081, Context: 16384, Parallel: 8,
		Args: []string{"--enforce-eager"},
	}, "gpu_memory_utilization: 0.85\n")

	cmd, err := New().Command(s)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if cmd.Path != "vllm" {
		t.Errorf("Path = %q, want the default binary %q", cmd.Path, "vllm")
	}
	want := []string{
		"serve", "/models/granite-4.2-8b-FP8",
		"--host", "127.0.0.1",
		"--port", "18081",
		"--served-model-name", "granite-4.2-8b",
		"--tensor-parallel-size", "1",
		"--gpu-memory-utilization", "0.85",
		"--max-model-len", "16384",
		"--max-num-seqs", "8",
		"--enforce-eager",
	}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("args mismatch\n got: %v\nwant: %v", cmd.Args, want)
	}
	// Decision 11: no device variable. gpu.PinningEnv owns that, node-wide.
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "CUDA_VISIBLE_DEVICES") || strings.HasPrefix(kv, "ROCR_VISIBLE_DEVICES") {
			t.Errorf("engine set a device variable (%q); pinning belongs to the node", kv)
		}
	}
}

func TestCommandTensorParallelUsesDefaults(t *testing.T) {
	s := spec(t, engine.Spec{
		Name: "qwen3-32b", Path: "/models/Qwen3-32B",
		GPUs: []int{2, 3}, Port: 18082, Context: 8192, Parallel: 4,
	}, "")

	cmd, err := New().Command(s)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	// Two cards in the backend means --tensor-parallel-size 2. The cards are
	// renumbered from zero by the node's pinning, so the indices 2 and 3 never
	// reach the command line.
	if got := argOf(cmd.Args, "--tensor-parallel-size"); got != "2" {
		t.Errorf("--tensor-parallel-size = %q, want 2", got)
	}
	if got := argOf(cmd.Args, "--gpu-memory-utilization"); got != "0.9" {
		t.Errorf("--gpu-memory-utilization = %q, want the default 0.9", got)
	}
	if got := argOf(cmd.Args, "--port"); got != "18082" {
		t.Errorf("--port = %q, want the assigned port 18082", got)
	}
}

// Decision 1: --max-model-len is ALWAYS passed, from Spec.Context, and
// Spec.Context is per slot. A backend serving more than the node advertises
// makes ctx a lie on every dashboard.
func TestCommandAlwaysPassesContextAndParallel(t *testing.T) {
	s := spec(t, engine.Spec{
		Name: "m", Path: "/models/m", GPUs: []int{0}, Port: 1, Context: 4096, Parallel: 2,
	}, "")
	cmd, err := New().Command(s)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if got := argOf(cmd.Args, "--max-model-len"); got != "4096" {
		t.Errorf("--max-model-len = %q, want 4096 (Spec.Context, per slot, never omitted)", got)
	}
	if got := argOf(cmd.Args, "--max-num-seqs"); got != "2" {
		t.Errorf("--max-num-seqs = %q, want 2 (Spec.Parallel)", got)
	}
}

func TestCommandRejectsUnknownOptionKeyNamingIt(t *testing.T) {
	s := spec(t, engine.Spec{
		Name: "m", Path: "/models/m", GPUs: []int{0}, Port: 1, Context: 8, Parallel: 1,
	}, "gpu_memory_utilisation: 0.85\n") // British spelling: a real typo
	_, err := New().Command(s)
	if err == nil {
		t.Fatal("an unknown key in the vllm block must be an error")
	}
	if !strings.Contains(err.Error(), "gpu_memory_utilisation") {
		t.Errorf("the error must name the offending key, got: %v", err)
	}
}

func TestCommandRejectsCPUBackend(t *testing.T) {
	s := spec(t, engine.Spec{Name: "m", Path: "/models/m", Port: 1, Context: 8, Parallel: 1}, "")
	if _, err := New().Command(s); err == nil {
		t.Error("vllm does not implement CPURunner; a GPU-less Spec must be an error, not --tensor-parallel-size 0")
	}
}

func TestEngineDoesNotClaimCapabilitiesItCannotHonour(t *testing.T) {
	e := New()
	// Decision 9: vLLM publishes cumulative token counters, not per-request
	// progress. A cumulative total rendered as "tokens left in this request"
	// is worse than a blank.
	if _, ok := any(e).(engine.TokenProgressReader); ok {
		t.Error("vllm must not implement TokenProgressReader (Decision 9)")
	}
	if _, ok := any(e).(engine.CPURunner); ok {
		t.Error("vllm must not implement CPURunner: it cannot serve without a GPU")
	}
}

// ------------------------------------------------------------------ validate

func TestValidateOptions(t *testing.T) {
	for _, tc := range []struct {
		name, block, wantIn string
		ok                  bool
	}{
		{name: "defaults", block: "", ok: true},
		{name: "in range", block: "gpu_memory_utilization: 0.85\n", ok: true},
		{name: "above one", block: "gpu_memory_utilization: 1.5\n", wantIn: "models[0].vllm.gpu_memory_utilization"},
		{name: "zero", block: "gpu_memory_utilization: 0\n", wantIn: "models[0].vllm.gpu_memory_utilization"},
		{name: "empty binary", block: "binary: \"\"\n", wantIn: "models[0].vllm.binary"},
		{name: "unknown key", block: "gpu_memory_utilisation: 0.85\n", wantIn: "models[0].vllm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := spec(t, engine.Spec{Name: "m", Path: "/m", GPUs: []int{0}, Port: 1, Context: 8, Parallel: 1}, tc.block)
			err := New().ValidateOptions("models[0]", s)
			if tc.ok {
				if err != nil {
					t.Fatalf("want no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want an error")
			}
			// C1's rule, which did not stop being true because it moved into
			// the engine package: every error names the field that is wrong.
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error must name the field %q, got: %v", tc.wantIn, err)
			}
		})
	}
}

// --------------------------------------------------------------------- probe

func TestProbe(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		wantReady bool
	}{
		{"ready", http.StatusOK, true},
		{"still loading", http.StatusServiceUnavailable, false},
		{"gateway error", http.StatusBadGateway, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/health" {
					t.Errorf("probe asked for %q, want /health", r.URL.Path)
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			p, err := New().Probe(t.Context(), hostPort(srv))
			if err != nil {
				t.Fatalf("an answered probe must not error: %v", err)
			}
			if p.Ready != tc.wantReady {
				t.Errorf("Ready = %v, want %v", p.Ready, tc.wantReady)
			}
			if p.Progress != -1 {
				t.Errorf("Progress = %v, want -1: vLLM reports none, and 0 would render as 0%%", p.Progress)
			}
			if p.Phase != "" {
				t.Errorf("Phase = %q, want empty: vLLM reports no load phase", p.Phase)
			}
			if !tc.wantReady && p.Reason == "" {
				t.Error("a not-ready probe must carry a Reason for the activity log")
			}
		})
	}
}

func TestProbeRefusedIsATransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := hostPort(srv)
	srv.Close()

	if _, err := New().Probe(t.Context(), addr); err == nil {
		t.Error("a refused connection must be an error: the node cannot otherwise tell 'loading' from 'gone'")
	}
}

// ---------------------------------------------------------------------- load

func TestLoadFromCapturedMetrics(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		fixture               string
		wantBusy, wantWaiting int
		wantErr               bool
	}{
		{name: "v1 engine", fixture: "metrics-v1.prom", wantBusy: 7, wantWaiting: 2},
		{name: "v0 legacy cache name", fixture: "metrics-v0-legacy-cache-name.prom", wantBusy: 3, wantWaiting: 0},
		// Counts sum across data-parallel engines: the backend's load is the total.
		{name: "data parallel sums counts", fixture: "metrics-data-parallel.prom", wantBusy: 7, wantWaiting: 3},
		// The per-reason breakdown must not be summed into the waiting total.
		{name: "nightly waiting by reason", fixture: "metrics-nightly-waiting-by-reason.prom", wantBusy: 4, wantWaiting: 1},
		// Decision 3.
		{name: "running gauge absent", fixture: "metrics-no-running-gauge.prom", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := read(t, tc.fixture)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/metrics" {
					t.Errorf("load asked for %q, want /metrics", r.URL.Path)
				}
				_, _ = w.Write(body)
			}))
			defer srv.Close()

			e, s := engineServing(t, srv, 8, 16384)
			load, err := e.Load(t.Context(), s, hostPort(srv))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %+v: a zero would freeze 'idle' into the mesh for a busy backend", load)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if load.Busy != tc.wantBusy {
				t.Errorf("Busy = %d, want %d", load.Busy, tc.wantBusy)
			}
			if load.Waiting != tc.wantWaiting {
				t.Errorf("Waiting = %d, want %d", load.Waiting, tc.wantWaiting)
			}
			// Decisions 2 and 6: both restate what the node asked for.
			if load.Slots != s.Parallel {
				t.Errorf("Slots = %d, want Spec.Parallel %d", load.Slots, s.Parallel)
			}
			if load.CtxPerSlot != int64(s.Context) {
				t.Errorf("CtxPerSlot = %d, want Spec.Context %d", load.CtxPerSlot, s.Context)
			}
		})
	}
}

// KV cache occupancy is parsed and kept accurate even though meshapi has no
// field for it. Under data parallelism it takes the maximum: it is a pressure
// signal, and the engine closest to preemption makes the backend slow.
func TestKVCacheOccupancyTakesTheWorstEngine(t *testing.T) {
	st := parseStats(read(t, "metrics-data-parallel.prom"))
	if st.kvCachePct != 0.80 {
		t.Errorf("kvCachePct = %v, want 0.80 (the worst engine, not the mean)", st.kvCachePct)
	}
	if !st.found["kv_cache"] {
		t.Error("kv_cache not marked found")
	}
}

// An explicit zero and a build that does not publish the gauge are different
// facts. Absent is not zero — on the wire and here.
func TestZeroIsNotAbsent(t *testing.T) {
	zero := parseStats([]byte("vllm:num_requests_running{model_name=\"m\"} 0.0\n"))
	if zero.numRunning != 0 || !zero.found["running"] {
		t.Errorf("an explicit zero must be found: %+v", zero)
	}
	absent := parseStats([]byte("# nothing here\n"))
	if absent.found["running"] {
		t.Error("an absent gauge must not be marked found")
	}
}

func TestHistogramSeriesDoesNotLeakIntoTheGauge(t *testing.T) {
	st := parseStats([]byte("vllm:num_requests_running_bucket{le=\"1\"} 99.0\n"))
	if st.found["running"] || st.numRunning != 0 {
		t.Errorf("a histogram bucket leaked into the gauge: %+v", st)
	}
}

func TestParseSampleForms(t *testing.T) {
	for _, tc := range []struct {
		line, metric string
		val          float64
		ok           bool
	}{
		{`vllm:x{a="1"} 5.0`, "vllm:x", 5, true},
		{`vllm:x 5.0`, "vllm:x", 5, true},
		{`vllm:x{a="1"} 5.0 1700000000000`, "vllm:x", 5, true}, // trailing timestamp
		{`vllm:x{a="}"} 2.0`, "vllm:x", 2, true},               // brace inside a label value
		{`malformed`, "", 0, false},
		{`vllm:x notanumber`, "", 0, false},
	} {
		metric, val, ok := parseSample(tc.line)
		if ok != tc.ok || metric != tc.metric || val != tc.val {
			t.Errorf("parseSample(%q) = (%q, %v, %v), want (%q, %v, %v)",
				tc.line, metric, val, ok, tc.metric, tc.val, tc.ok)
		}
	}
}

func TestDefaultStartupTimeoutIsGenerous(t *testing.T) {
	// Decision 10. vLLM profiles the model, allocates the KV cache and
	// captures CUDA graphs before answering anything.
	if got := New().DefaultStartupTimeout(); got != 20*time.Minute {
		t.Errorf("DefaultStartupTimeout = %v, want 20m", got)
	}
}

// ------------------------------------------------------------------- helpers

// engineServing returns an Engine and the Spec for a backend on srv's port.
// Load is handed that Spec directly, so the engine needs no prior Command call
// and keeps no state between the two.
func engineServing(t *testing.T, srv *httptest.Server, parallel, ctxPerSlot int) (*Engine, engine.Spec) {
	t.Helper()
	_, portStr, _ := strings.Cut(hostPort(srv), ":")
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port from %q: %v", srv.URL, err)
	}
	s := spec(t, engine.Spec{
		Name: "m", Path: "/models/m", GPUs: []int{0},
		Port: port, Context: ctxPerSlot, Parallel: parallel,
	}, "")
	return New(), s
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
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		s.Options = *node.Content[0]
	} else {
		s.Options = node
	}
	return s
}

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func hostPort(srv *httptest.Server) string { return strings.TrimPrefix(srv.URL, "http://") }

// argOf returns the value following flag on a command line, or "" if absent.
func argOf(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}
