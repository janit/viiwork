package freetoken

import (
	"encoding/json"
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
			Name: "one card",
			Spec: engine.Spec{
				Name: "DeepSeek-V4-Flash", Path: "/models/DSV4-Flash-NVFP4",
				GPUs: []int{0}, Port: 18091, Context: 65536, Parallel: 4,
				Vendor: gpu.VendorNVIDIA,
			},
			Options: "binary: ft\nmemory_ratio: 0.92\nmoe_backend: offload\n",
		},
		enginetest.Case{
			Name: "operator args",
			Spec: engine.Spec{
				Name: "qwen3-next", Path: "/models/qwen3-next",
				GPUs: []int{1}, Port: 18092, Context: 32768, Parallel: 2,
				Args: []string{"--page-size", "64"}, Vendor: gpu.VendorNVIDIA,
			},
		},
	)
}

// ------------------------------------------------------------- command golden

func TestCommandOmitsKVReserveWhenZero(t *testing.T) {
	s := spec(t, engine.Spec{
		Name: "DeepSeek-V4-Flash", Path: "/models/DSV4-Flash-NVFP4",
		GPUs: []int{0}, Port: 18091, Context: 65536, Parallel: 4,
		Args: []string{"--page-size", "128"},
	}, "memory_ratio: 0.92\nmoe_backend: offload\n")

	cmd, err := New().Command(s)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if cmd.Path != "ft" {
		t.Errorf("Path = %q, want the default binary %q", cmd.Path, "ft")
	}
	want := []string{
		"serve",
		"--model", "/models/DSV4-Flash-NVFP4",
		"--host", "127.0.0.1",
		"--port", "18091",
		"--served-model-name", "DeepSeek-V4-Flash",
		"--memory-ratio", "0.92",
		"--max-running-requests", "4",
		"--max-seq-len-override", "65536",
		"--moe-backend", "offload",
		"--page-size", "128",
	}
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("args mismatch\n got: %v\nwant: %v", cmd.Args, want)
	}
	if slices.Contains(cmd.Args, "--kv-reserve-tokens") {
		t.Error("--kv-reserve-tokens must be omitted when zero")
	}
}

func TestCommandIncludesKVReserveWhenSet(t *testing.T) {
	s := spec(t, engine.Spec{
		Name: "m", Path: "/models/m", GPUs: []int{0}, Port: 1, Context: 8192, Parallel: 1,
	}, "kv_reserve_tokens: 4096\n")

	cmd, err := New().Command(s)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if got := argOf(cmd.Args, "--kv-reserve-tokens"); got != "4096" {
		t.Errorf("--kv-reserve-tokens = %q, want 4096", got)
	}
	// The default moe_backend must still be passed.
	if got := argOf(cmd.Args, "--moe-backend"); got != "auto" {
		t.Errorf("--moe-backend = %q, want the default auto", got)
	}
}

// Decision 11: no device variable, and no --gpu. The node's pinning is correct
// on both engine generations; --gpu is correct on only the newer one.
func TestCommandLeavesCardSelectionToTheNode(t *testing.T) {
	s := spec(t, engine.Spec{
		Name: "m", Path: "/models/m", GPUs: []int{3}, Port: 1, Context: 8192, Parallel: 1,
	}, "")
	cmd, err := New().Command(s)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if slices.Contains(cmd.Args, "--gpu") {
		t.Error("--gpu must not be generated: FreeToken 0.1.2 has no such flag")
	}
	for _, kv := range cmd.Env {
		if strings.Contains(kv, "VISIBLE_DEVICES") || strings.Contains(kv, "CUDA_DEVICE_ORDER") {
			t.Errorf("engine set a pinning variable (%q); that belongs to the node", kv)
		}
	}
}

func TestCommandRejectsMultiCardBackend(t *testing.T) {
	s := spec(t, engine.Spec{
		Name: "m", Path: "/models/m", GPUs: []int{0, 1}, Port: 1, Context: 8192, Parallel: 1,
	}, "")
	if _, err := New().Command(s); err == nil {
		t.Error("the engine takes exactly one card per process; two must be an error")
	}
}

func TestEngineImplementsExactlyTheCapabilitiesItCanHonour(t *testing.T) {
	e := New()
	// Decision 9: cumulative token counters are not per-request progress.
	if _, ok := any(e).(engine.TokenProgressReader); ok {
		t.Error("freetoken must not implement TokenProgressReader (Decision 9)")
	}
	if _, ok := any(e).(engine.CPURunner); ok {
		t.Error("freetoken must not implement CPURunner")
	}
	// Decision 8: it CAN answer which card it bound, so it must.
	if _, ok := any(e).(engine.GPUBindingReader); !ok {
		t.Error("freetoken must implement GPUBindingReader")
	}
}

// ------------------------------------------------------------------ validate

func TestValidateOptions(t *testing.T) {
	oneGPU := engine.Spec{Name: "m", Path: "/m", GPUs: []int{0}, Port: 1, Context: 8, Parallel: 1}
	twoGPU := engine.Spec{Name: "m", Path: "/m", GPUs: []int{0, 1}, Port: 1, Context: 8, Parallel: 1}

	for _, tc := range []struct {
		name, block, wantIn string
		base                engine.Spec
		ok                  bool
	}{
		{name: "defaults", base: oneGPU, ok: true},
		{name: "in range", base: oneGPU, block: "memory_ratio: 0.8\n", ok: true},
		{name: "ratio above one", base: oneGPU, block: "memory_ratio: 1.2\n", wantIn: "models[0].freetoken.memory_ratio"},
		{name: "empty moe backend", base: oneGPU, block: "moe_backend: \"\"\n", wantIn: "models[0].freetoken.moe_backend"},
		{name: "negative reserve", base: oneGPU, block: "kv_reserve_tokens: -1\n", wantIn: "models[0].freetoken.kv_reserve_tokens"},
		{name: "unknown key", base: oneGPU, block: "memory_ration: 0.8\n", wantIn: "models[0].freetoken"},
		// Decision 7, as close as the Spec allows it to be expressed. See F6.
		{name: "two cards per backend", base: twoGPU, wantIn: "models[0].gpus_per_backend"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := spec(t, tc.base, tc.block)
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
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error must name the field %q, got: %v", tc.wantIn, err)
			}
		})
	}
}

// --------------------------------------------------------------------- probe

// The readiness rule, over every lifecycle shape the engine produces. A 200 is
// never on its own enough.
func TestProbeLifecycleShapes(t *testing.T) {
	for _, tc := range []struct {
		name, body       string
		wantReady        bool
		wantPhase        string
		wantProgress     float64
		wantReasonSubstr string
	}{
		{
			name: "serving", body: `{"status":"ok","model":"m","maintenance":"serving","version":"0.4.1"}`,
			wantReady: true, wantProgress: -1,
		},
		{
			// An older build omits maintenance from the ready shape.
			name: "ok without maintenance", body: `{"status":"ok","model":"m"}`,
			wantReady: true, wantProgress: -1,
		},
		{
			name:      "loading with progress",
			body:      `{"status":"loading","phase":"weights","progress":{"done_bytes":25,"total_bytes":100},"model":"m"}`,
			wantReady: false, wantPhase: "weights", wantProgress: 0.25, wantReasonSubstr: "loading",
		},
		{
			name: "loading without byte counts", body: `{"status":"loading","phase":"cuda_graphs"}`,
			wantReady: false, wantPhase: "cuda_graphs", wantProgress: -1, wantReasonSubstr: "cuda_graphs",
		},
		{
			// A live cache rebuild takes the engine out of service WITHOUT
			// changing status. Also 200, also 503s every request.
			name: "rebuilding", body: `{"status":"ok","model":"m","maintenance":"rebuilding"}`,
			wantReady: false, wantProgress: -1, wantReasonSubstr: "rebuilding",
		},
		{
			name: "fatal error", body: `{"status":"error","message":"worker died"}`,
			wantReady: false, wantProgress: -1, wantReasonSubstr: "worker died",
		},
		{
			// Decision 4: not the document => not ready, NOT a transport error.
			name: "not the health document", body: `<html>nginx</html>`,
			wantReady: false, wantProgress: -1, wantReasonSubstr: "not a FreeToken",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := serving(t, map[string]string{"/health": tc.body})
			defer srv.Close()

			p, err := New().Probe(t.Context(), hostPort(srv))
			if err != nil {
				t.Fatalf("a 200 must never be a transport error: %v", err)
			}
			if p.Ready != tc.wantReady {
				t.Fatalf("Ready = %v, want %v (body %s)", p.Ready, tc.wantReady, tc.body)
			}
			if p.Phase != tc.wantPhase {
				t.Errorf("Phase = %q, want %q", p.Phase, tc.wantPhase)
			}
			if p.Progress != tc.wantProgress {
				t.Errorf("Progress = %v, want %v", p.Progress, tc.wantProgress)
			}
			if tc.wantReasonSubstr != "" && !strings.Contains(p.Reason, tc.wantReasonSubstr) {
				t.Errorf("Reason = %q, want it to mention %q", p.Reason, tc.wantReasonSubstr)
			}
		})
	}
}

// The live capture from a real DSV4-Flash backend, 7.5 hours into serving.
func TestProbeAgainstLiveCapture(t *testing.T) {
	srv := serving(t, map[string]string{"/health": string(read(t, "health-live-dsv4.json"))})
	defer srv.Close()

	p, err := New().Probe(t.Context(), hostPort(srv))
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !p.Ready {
		t.Errorf("the live serving capture must read as ready, got %+v", p)
	}
}

func TestProbeRefusedIsATransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := hostPort(srv)
	srv.Close()
	if _, err := New().Probe(t.Context(), addr); err == nil {
		t.Error("a refused connection must be an error")
	}
}

// ---------------------------------------------------------------------- load

func TestLoadAndCacheOccupancyAcrossPoolShapes(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantCache  float64
		wantFound  []string
		wantAbsent []string
	}{
		{
			name:      "dense attention reports kv",
			body:      `{"requests":{"active":3},"kv":{"used_pages":310,"total_pages":500},"mamba":null,"swa":null}`,
			wantCache: 0.62, wantFound: []string{"kv", "cache"}, wantAbsent: []string{"swa", "mamba"},
		},
		{
			// Reading only kv would report a permanent zero for this model.
			name:      "sliding window reports swa with a null kv",
			body:      `{"requests":{"active":1},"kv":null,"swa":{"used_pages":9,"total_pages":10}}`,
			wantCache: 0.9, wantFound: []string{"swa", "cache"}, wantAbsent: []string{"kv"},
		},
		{
			name:      "linear attention reports mamba slots",
			body:      `{"requests":{"active":0},"kv":null,"mamba":{"used_slots":1,"total_slots":4}}`,
			wantCache: 0.25, wantFound: []string{"mamba", "cache"}, wantAbsent: []string{"kv", "swa"},
		},
		{
			// The maximum across pools, not the first one found.
			name:      "hybrid takes the fullest pool",
			body:      `{"requests":{"active":2},"kv":{"used_pages":1,"total_pages":10},"mamba":{"used_slots":7,"total_slots":8}}`,
			wantCache: 0.875, wantFound: []string{"kv", "mamba", "cache"},
		},
		{
			// An unallocated pool is absent, not a zero occupancy.
			name:      "all pools null",
			body:      `{"requests":{"active":0},"kv":null,"swa":null,"mamba":null}`,
			wantCache: 0, wantAbsent: []string{"cache", "kv", "swa", "mamba"},
		},
		{
			name:      "zero-sized pool is absent",
			body:      `{"requests":{"active":0},"kv":{"used_pages":0,"total_pages":0}}`,
			wantCache: 0, wantAbsent: []string{"cache", "kv"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, ok := parseStats([]byte(tc.body))
			if !ok {
				t.Fatal("parseStats rejected a valid document")
			}
			if st.cachePct != tc.wantCache {
				t.Errorf("cachePct = %v, want %v", st.cachePct, tc.wantCache)
			}
			for _, k := range tc.wantFound {
				if !st.found[k] {
					t.Errorf("%q should be found: %v", k, st.found)
				}
			}
			for _, k := range tc.wantAbsent {
				if st.found[k] {
					t.Errorf("%q should be absent, not zero: %v", k, st.found)
				}
			}
		})
	}
}

func TestLoadRestatesWhatTheNodeAsked(t *testing.T) {
	srv := serving(t, map[string]string{"/v1/stats": `{"requests":{"active":3},"kv":{"used_pages":1,"total_pages":2}}`})
	defer srv.Close()

	e, s := engineServing(t, srv, 6, 32768)
	load, err := e.Load(t.Context(), s, hostPort(srv))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if load.Busy != 3 {
		t.Errorf("Busy = %d, want 3", load.Busy)
	}
	// Decision 5: the engine publishes no queue-depth gauge.
	if load.Waiting != 0 {
		t.Errorf("Waiting = %d, want 0: FreeToken reports no queue depth", load.Waiting)
	}
	if load.Slots != s.Parallel {
		t.Errorf("Slots = %d, want Spec.Parallel %d", load.Slots, s.Parallel)
	}
	if load.CtxPerSlot != int64(s.Context) {
		t.Errorf("CtxPerSlot = %d, want Spec.Context %d", load.CtxPerSlot, s.Context)
	}
}

func TestLoadRejectsARubbishStatsDocument(t *testing.T) {
	srv := serving(t, map[string]string{"/v1/stats": `<html>nginx</html>`})
	defer srv.Close()

	e, s := engineServing(t, srv, 4, 8192)
	if _, err := e.Load(t.Context(), s, hostPort(srv)); err == nil {
		t.Error("a body that is not the stats document must be an error, not a zero load")
	}
}

// The live DSV4 capture reports BOTH kv and swa pools at once — an easy
// assumption to get wrong, since a reader that looked only at kv would show a
// permanent zero for the one model this engine exists to run.
func TestLiveCaptureReportsBothKVAndSWA(t *testing.T) {
	st, ok := parseStats(read(t, "stats-live-dsv4.json"))
	if !ok {
		t.Fatal("parseStats rejected the live capture")
	}
	if !st.found["kv"] || !st.found["swa"] {
		t.Errorf("DSV4 reports both kv and swa: %v", st.found)
	}
	if st.found["mamba"] {
		t.Error("mamba is null here and must not be marked found")
	}
	if st.numRunning != 0 {
		t.Errorf("numRunning = %d, want 0", st.numRunning)
	}
}

// ----------------------------------------------------------------- boundGPU

func TestBoundGPU(t *testing.T) {
	const uuid = "GPU-9e8d7c6b-5a49-4f13-8207-c1b0a4e6d3f5"
	t.Run("engine reports a card", func(t *testing.T) {
		body := `{"requests":{"active":0},"gpus":[{"index":0,"name":"NVIDIA GeForce RTX 5090","uuid":"` + uuid + `"}]}`
		srv := serving(t, map[string]string{"/v1/stats": body})
		defer srv.Close()
		got, ok, err := New().BoundGPU(t.Context(), hostPort(srv))
		if err != nil || !ok || got != uuid {
			t.Errorf("BoundGPU = (%q, %v, %v), want (%q, true, nil)", got, ok, err, uuid)
		}
	})
	t.Run("engine at 0.1.2 reports none", func(t *testing.T) {
		// The live capture predates the gpus field. ok=false is "unknown",
		// which is NOT a mismatch — Decision 8.
		srv := serving(t, map[string]string{"/v1/stats": string(read(t, "stats-live-dsv4.json"))})
		defer srv.Close()
		_, ok, err := New().BoundGPU(t.Context(), hostPort(srv))
		if err != nil {
			t.Fatalf("BoundGPU: %v", err)
		}
		if ok {
			t.Error("an engine that reports no UUID must read as unknown, not as a mismatch")
		}
	})
}

// ------------------------------------------------------- the context ceiling

// FINDING F8, in executable form.
//
// The node publishes Spec.Context per slot to the mesh (Decision 6: "no
// /v1/models round trip"). On this live DSV4-Flash backend the checkpoint
// advertises 1,048,576 tokens while the KV pool the engine actually built
// holds 501 * 128 = 64,128 — the number a request is measured against. An
// operator writing `context: 131072` gets a mesh advertising 131072 and a
// backend answering 400 "prompt is too long".
//
// v1 read /v1/cache/status to catch exactly this. Nothing in v2.2 does: the
// disposition table carries ParseHealth and ParseStats and does not mention
// ParseKVCapacity or ParseContextLen.
func TestLiveCaptureShowsTheAdvertisedContextIsNotTheServableOne(t *testing.T) {
	var stats struct {
		Model struct {
			Ctx int64 `json:"ctx"`
		} `json:"model"`
	}
	if err := json.Unmarshal(read(t, "stats-live-dsv4.json"), &stats); err != nil {
		t.Fatalf("stats: %v", err)
	}
	var cache struct {
		Geometry struct {
			NumPages int64 `json:"num_pages"`
			PageSize int64 `json:"page_size"`
		} `json:"geometry"`
	}
	if err := json.Unmarshal(read(t, "cache-status-live-dsv4.json"), &cache); err != nil {
		t.Fatalf("cache status: %v", err)
	}

	advertised := stats.Model.Ctx
	servable := cache.Geometry.NumPages * cache.Geometry.PageSize
	if advertised != 1048576 {
		t.Fatalf("fixture drift: advertised ctx = %d", advertised)
	}
	if servable != 64128 {
		t.Fatalf("fixture drift: servable = %d", servable)
	}
	if servable >= advertised {
		t.Fatal("this test exists because the servable ceiling is far below the advertised one")
	}
	t.Logf("advertised %d tokens, servable %d: a factor of %.1f",
		advertised, servable, float64(advertised)/float64(servable))
}

func TestDefaultStartupTimeoutIsThirtyMinutes(t *testing.T) {
	if got := New().DefaultStartupTimeout(); got != 30*time.Minute {
		t.Errorf("DefaultStartupTimeout = %v, want 30m", got)
	}
}

// ------------------------------------------------------------------- helpers

// serving returns a server answering the given paths with 200, and 200 with an
// empty body for anything else — because FreeToken answers 200 everywhere and
// a test server that 404s would not reproduce the trap.
func serving(t *testing.T, bodies map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			body = "{}"
		}
		_, _ = w.Write([]byte(body))
	}))
}

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

func argOf(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}
