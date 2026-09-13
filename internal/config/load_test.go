package config

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func parseFile(t *testing.T, path string) *Config {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(%s): %v", path, err)
	}
	return cfg
}

func TestParseFullExample(t *testing.T) {
	cfg := parseFile(t, "testdata/full.yaml")

	if cfg.Node.Name != "node-a" || cfg.Node.StateDir != "/var/lib/viiwork" {
		t.Errorf("node = %+v", cfg.Node)
	}
	if len(cfg.API.CORS.AllowOrigins) != 4 || cfg.API.CORS.AllowTailnetIPs == nil || !*cfg.API.CORS.AllowTailnetIPs {
		t.Errorf("api.cors = %+v", cfg.API.CORS)
	}
	if cfg.Mesh.Network != NetworkTailnet || cfg.Mesh.BindPort != 7946 || cfg.Mesh.Open {
		t.Errorf("mesh = %+v", cfg.Mesh)
	}
	if cfg.Mesh.Tailnet.Enabled != ToggleAuto || cfg.Mesh.LAN.MDNS != ToggleOff {
		t.Errorf("mesh toggles: tailnet=%q mdns=%q", cfg.Mesh.Tailnet.Enabled, cfg.Mesh.LAN.MDNS)
	}
	if len(cfg.Mesh.Seeds) != 1 || cfg.Mesh.Seeds[0] != "100.64.0.20:7946" {
		t.Errorf("mesh.seeds = %v", cfg.Mesh.Seeds)
	}
	if cfg.Routing.QueueMax != 32 || cfg.Routing.QueueTimeout.Duration != 15*time.Second {
		t.Errorf("routing = %+v", cfg.Routing)
	}
	if cfg.GPU.Vendor != VendorNVIDIA {
		t.Errorf("gpu.vendor = %q", cfg.GPU.Vendor)
	}
	if len(cfg.Models) != 3 {
		t.Fatalf("models = %d, want 3", len(cfg.Models))
	}

	q := cfg.Models[0]
	if q.Name != "Qwen3.8-27B" || q.Engine != testEngineCPU || q.GPUsPerBackend != 2 || q.Context != 49152 || q.Parallel != 2 {
		t.Errorf("models[0] = %+v", q)
	}
	var lc struct {
		SplitMode    string    `yaml:"split_mode"`
		SplitWeights []float64 `yaml:"split_weights"`
	}
	if b := q.EngineBlock(); b.IsZero() {
		t.Error("models[0].llamacpp block was not captured")
	} else if err := b.Decode(&lc); err != nil {
		t.Errorf("decoding models[0].llamacpp: %v", err)
	} else if lc.SplitMode != "layer" || len(lc.SplitWeights) != 2 {
		t.Errorf("models[0].llamacpp = %+v", lc)
	}
	if q.Backends() != 1 || !equalInts(q.BackendGPUs(0), []int{0, 1}) {
		t.Errorf("models[0] backends=%d gpus=%v", q.Backends(), q.BackendGPUs(0))
	}

	g := cfg.Models[1]
	var vl struct {
		Util float64 `yaml:"gpu_memory_utilization"`
	}
	if g.Engine != testEngineGPU || len(g.Options) != 1 {
		t.Errorf("models[1] = %+v options=%v", g, g.Options)
	}
	gb := g.EngineBlock()
	if err := gb.Decode(&vl); err != nil || vl.Util != 0.85 {
		t.Errorf("models[1].vllm = %+v (%v)", vl, err)
	}

	d := cfg.Models[2]
	var ft struct {
		MemoryRatio float64 `yaml:"memory_ratio"`
		KVReserve   int     `yaml:"kv_reserve_tokens"`
	}
	db := d.EngineBlock()
	if err := db.Decode(&ft); err != nil || ft.MemoryRatio != 0.9 || ft.KVReserve != 32768 {
		t.Errorf("models[2].freetoken = %+v (%v)", ft, err)
	}
	if d.StartupTimeout.Duration != 30*time.Minute || d.Env["HOME"] != "/home/janit" || d.Parallel != 4 {
		t.Errorf("models[2] = %+v", d)
	}
}

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte("models:\n  - name: m\n    engine: llamacpp\n    path: /m.gguf\n    gpus: [0]\n    context: 4096\n"))
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"node.state_dir", cfg.Node.StateDir, "/var/lib/viiwork"},
		{"api.port", cfg.API.Port, 8086},
		{"mesh.network", cfg.Mesh.Network, NetworkTailnet},
		{"mesh.bind_port", cfg.Mesh.BindPort, 7946},
		{"mesh.secret_env", cfg.Mesh.SecretEnv, "VIIWORK_MESH_SECRET"},
		{"mesh.secret_enforce", cfg.Mesh.SecretEnforce, EnforceFull},
		{"mesh.tailnet.enabled", cfg.Mesh.Tailnet.Enabled, ToggleAuto},
		{"mesh.tailnet.socket", cfg.Mesh.Tailnet.Socket, "/var/run/tailscale/tailscaled.sock"},
		{"mesh.rejoin_interval", cfg.Mesh.RejoinInterval.Duration, 60 * time.Second},
		{"mesh.capacity_poll", cfg.Mesh.CapacityPoll.Duration, time.Second},
		{"routing.queue_max", cfg.Routing.QueueMax, 64},
		{"routing.queue_timeout", cfg.Routing.QueueTimeout.Duration, 20 * time.Second},
		{"routing.forward_retry", cfg.Routing.ForwardRetry, 1},
		{"routing.stale_after", cfg.Routing.StaleAfter.Duration, 3 * time.Second},
		{"gpu.vendor", cfg.GPU.Vendor, VendorAuto},
		{"health.timeout", cfg.Health.Timeout.Duration, 10 * time.Second},
		{"health.max_respawns", cfg.Health.MaxRespawns, 3},
		{"activity.event_history", cfg.Activity.EventHistory, 2000},
		{"models[0].gpus_per_backend", cfg.Models[0].GPUsPerBackend, 1},
		{"models[0].parallel", cfg.Models[0].Parallel, 1},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestParseEmptyDocumentGivesDefaults(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API.Port != 8086 || len(cfg.Models) != 0 {
		t.Errorf("empty document: port=%d models=%d", cfg.API.Port, len(cfg.Models))
	}
}

func TestParseRejectsUnknownKey(t *testing.T) {
	_, err := Parse([]byte("mesh:\n  netwrok: lan\n"))
	if err == nil || !strings.Contains(err.Error(), "netwrok") {
		t.Fatalf("err = %v, want a mention of the unknown key", err)
	}
}

func TestParseDetectsV1(t *testing.T) {
	data, err := os.ReadFile("testdata/v1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Parse(data)
	if !errors.Is(err, ErrV1Config) {
		t.Fatalf("err = %v, want ErrV1Config", err)
	}
	if !strings.Contains(err.Error(), `"server"`) || !strings.Contains(err.Error(), "docs/migrating-to-v2.md") {
		t.Errorf("err = %q, want the key and the migration doc named", err)
	}
}

func TestToggleUnmarshal(t *testing.T) {
	for in, want := range map[string]Toggle{
		"auto": ToggleAuto, "AUTO": ToggleAuto,
		"true": ToggleOn, "yes": ToggleOn, "on": ToggleOn,
		"false": ToggleOff, "no": ToggleOff, "off": ToggleOff,
	} {
		var got struct {
			T Toggle `yaml:"t"`
		}
		if err := yaml.Unmarshal([]byte("t: "+in), &got); err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got.T != want {
			t.Errorf("%q -> %q, want %q", in, got.T, want)
		}
	}
	var bad struct {
		T Toggle `yaml:"t"`
	}
	if err := yaml.Unmarshal([]byte("t: maybe"), &bad); err == nil {
		t.Error("t: maybe must be rejected")
	}
}

func TestToggleResolve(t *testing.T) {
	if !ToggleAuto.Resolve(true) || ToggleAuto.Resolve(false) || !ToggleOn.Resolve(false) || ToggleOff.Resolve(true) {
		t.Error("Resolve: auto follows the caller's default, true and false override it")
	}
}

func TestModelBackends(t *testing.T) {
	m := Model{GPUs: []int{0, 1, 4, 5, 6, 7}, GPUsPerBackend: 2}
	if m.Backends() != 3 || !equalInts(m.BackendGPUs(2), []int{6, 7}) {
		t.Errorf("backends=%d gpus(2)=%v", m.Backends(), m.BackendGPUs(2))
	}
	if cpu := (Model{GPUsPerBackend: 1}); cpu.Backends() != 1 || cpu.BackendGPUs(0) != nil {
		t.Error("a CPU model has one backend and no GPUs")
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A model may carry one block, named for its own engine. Anything else is an
// unknown key — which is what kept a typo an error when Model stopped having a
// field per engine.
func TestParseRejectsForeignAndUnknownModelKeys(t *testing.T) {
	base := "models:\n  - name: m\n    engine: llamacpp\n    path: /m.gguf\n    gpus: [0]\n    context: 4096\n"
	for _, tc := range []struct{ name, extra, want string }{
		{"another engine's block", "    vllm:\n      gpu_memory_utilization: 0.9\n", `unknown key "vllm"`},
		{"a typo", "    lamacpp:\n      binary: x\n", `unknown key "lamacpp"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(base + tc.extra))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
			if err != nil && !strings.Contains(err.Error(), "models[0]") {
				t.Errorf("err = %q, want the model named", err)
			}
		})
	}
}

func TestParseKeepsOwnEngineBlock(t *testing.T) {
	cfg, err := Parse([]byte("models:\n  - name: m\n    engine: llamacpp\n    path: /m.gguf\n    gpus: [0]\n    context: 4096\n    llamacpp:\n      threads: 6\n"))
	if err != nil {
		t.Fatal(err)
	}
	var o struct {
		Threads int `yaml:"threads"`
	}
	blk := cfg.Models[0].EngineBlock()
	if err := blk.Decode(&o); err != nil || o.Threads != 6 {
		t.Fatalf("llamacpp block = %+v (%v)", o, err)
	}
}
