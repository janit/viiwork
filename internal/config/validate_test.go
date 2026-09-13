package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// testSecret is standard base64 of 32 zero bytes.
const testSecret = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func envOf(kv map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := kv[k]; return v, ok }
}

// validConfig is an open mesh with one llamacpp model. Each test case breaks
// exactly one thing.
func validConfig() *Config {
	c := Defaults()
	c.Mesh.Open = true
	c.Models = []Model{{Name: "granite-4.2-8b", Engine: testEngineCPU, Path: "/models/granite.gguf", GPUs: []int{2}, Context: 16384}}
	applyModelDefaults(&c.Models[0])
	return &c
}

func engineModel(engine, name string, gpus ...int) Model {
	m := Model{Name: name, Engine: engine, Path: "/models/" + name, GPUs: gpus, Context: 8192}
	applyModelDefaults(&m)
	return m
}

// engineBlockModel is engineModel carrying the engine's own configuration
// block, written as an operator would write it.
func engineBlockModel(engine, name, block string, gpus ...int) Model {
	m := engineModel(engine, name, gpus...)
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(block), &n); err != nil {
		panic(err)
	}
	// Unmarshal gives a document node; the mapping is its only child.
	m.Options = map[string]yaml.Node{engine: *n.Content[0]}
	return m
}

func TestValidate(t *testing.T) {
	secret := map[string]string{"VIIWORK_MESH_SECRET": testSecret}
	cases := []struct {
		name    string
		mutate  func(c *Config)
		env     map[string]string
		wantErr string // empty means the config must be valid
	}{
		// mesh mode
		{name: "open mesh", mutate: func(c *Config) {}},
		{name: "secured mesh", mutate: func(c *Config) { c.Mesh.Open = false }, env: secret},
		{name: "secret and open", mutate: func(c *Config) {}, env: secret, wantErr: "choose a secured mesh or an open one"},
		{name: "neither secret nor open", mutate: func(c *Config) { c.Mesh.Open = false }, wantErr: "mesh.open is not true"},
		{name: "secret wrong length", mutate: func(c *Config) { c.Mesh.Open = false }, env: map[string]string{"VIIWORK_MESH_SECRET": "AAAAAAAAAAAAAAAAAAAAAA=="}, wantErr: "decodes to 16 bytes"},
		{name: "secret not base64", mutate: func(c *Config) { c.Mesh.Open = false }, env: map[string]string{"VIIWORK_MESH_SECRET": "not base64!"}, wantErr: "not standard base64"},
		{name: "previous secret without primary", mutate: func(c *Config) {}, env: map[string]string{"VIIWORK_MESH_SECRET_PREV": testSecret}, wantErr: "VIIWORK_MESH_SECRET_PREV is set but VIIWORK_MESH_SECRET is not"},
		{name: "enforce below full without secret", mutate: func(c *Config) { c.Mesh.SecretEnforce = EnforceNone }, wantErr: "requires VIIWORK_MESH_SECRET"},
		{name: "enforce outgoing with secret", mutate: func(c *Config) { c.Mesh.Open = false; c.Mesh.SecretEnforce = EnforceOutgoing }, env: secret},
		{name: "custom secret env", mutate: func(c *Config) { c.Mesh.Open = false; c.Mesh.SecretEnv = "MY_SECRET" }, env: map[string]string{"MY_SECRET": testSecret}},

		// node, api, mesh
		{name: "empty state dir", mutate: func(c *Config) { c.Node.StateDir = "" }, wantErr: "node.state_dir"},
		{name: "api port out of range", mutate: func(c *Config) { c.API.Port = 70000 }, wantErr: "api.port"},
		{name: "bad network", mutate: func(c *Config) { c.Mesh.Network = "wan" }, wantErr: "mesh.network"},
		{name: "bad bind port", mutate: func(c *Config) { c.Mesh.BindPort = 0 }, wantErr: "mesh.bind_port"},
		{name: "bad enforce", mutate: func(c *Config) { c.Mesh.SecretEnforce = "partial" }, wantErr: "mesh.secret_enforce"},
		{name: "advertise not an ip", mutate: func(c *Config) { c.Mesh.Advertise = "gb1" }, wantErr: "mesh.advertise"},
		{name: "advertise ip", mutate: func(c *Config) { c.Mesh.Advertise = "100.64.0.105" }},
		{name: "seed not ip:port", mutate: func(c *Config) { c.Mesh.Seeds = []string{"gb1:7946"} }, wantErr: "mesh.seeds[0]"},
		{name: "seed ip:port", mutate: func(c *Config) { c.Mesh.Seeds = []string{"100.64.0.105:7946"} }},
		{name: "bad tailnet toggle", mutate: func(c *Config) { c.Mesh.Tailnet.Enabled = "sometimes" }, wantErr: "mesh.tailnet.enabled"},
		{name: "bad mdns toggle", mutate: func(c *Config) { c.Mesh.LAN.MDNS = "sometimes" }, wantErr: "mesh.lan.mdns"},
		{name: "tailnet on without socket", mutate: func(c *Config) { c.Mesh.Tailnet.Socket = "" }, wantErr: "mesh.tailnet.socket"},
		{name: "lan mesh needs no socket", mutate: func(c *Config) { c.Mesh.Network = NetworkLAN; c.Mesh.Tailnet.Socket = "" }},
		{name: "zero rejoin interval", mutate: func(c *Config) { c.Mesh.RejoinInterval = Duration{} }, wantErr: "mesh.rejoin_interval"},
		{name: "zero capacity poll", mutate: func(c *Config) { c.Mesh.CapacityPoll = Duration{} }, wantErr: "mesh.capacity_poll"},

		// routing, gpu, health, activity, power, energy
		{name: "negative queue max", mutate: func(c *Config) { c.Routing.QueueMax = -1 }, wantErr: "routing.queue_max"},
		{name: "negative forward retry", mutate: func(c *Config) { c.Routing.ForwardRetry = -1 }, wantErr: "routing.forward_retry"},
		{name: "zero stale after", mutate: func(c *Config) { c.Routing.StaleAfter = Duration{} }, wantErr: "routing.stale_after"},
		{name: "bad vendor", mutate: func(c *Config) { c.GPU.Vendor = "intel" }, wantErr: "gpu.vendor"},
		{name: "negative power limit", mutate: func(c *Config) { c.GPU.PowerLimitWatts = -1 }, wantErr: "gpu.power_limit_watts"},
		{name: "zero health interval", mutate: func(c *Config) { c.Health.Interval = Duration{} }, wantErr: "health.interval"},
		{name: "zero max failures", mutate: func(c *Config) { c.Health.MaxFailures = 0 }, wantErr: "health.max_failures"},
		{name: "negative max respawns", mutate: func(c *Config) { c.Health.MaxRespawns = -1 }, wantErr: "health.max_respawns"},
		{name: "negative event history", mutate: func(c *Config) { c.Activity.EventHistory = -1 }, wantErr: "activity.event_history"},
		{name: "power control without hosts", mutate: func(c *Config) { c.Power.Control.Enabled = true }, wantErr: "power.control.hosts is empty"},
		{name: "bad power source", mutate: func(c *Config) { c.Power.Source = "ipmi" }, wantErr: "power.source"},
		{name: "energy without dir", mutate: func(c *Config) { c.Energy.Enabled = true; c.Energy.Dir = "" }, wantErr: "energy.dir"},

		// models
		{name: "zero-model super peer", mutate: func(c *Config) { c.Models = nil }},
		{name: "duplicate model name", mutate: func(c *Config) { c.Models = append(c.Models, engineModel(testEngineCPU, "granite-4.2-8b", 3)) }, wantErr: "models[1].name \"granite-4.2-8b\" is already used by models[0]"},
		{name: "whitespace in name", mutate: func(c *Config) { c.Models[0].Name = "granite 8b" }, wantErr: "no whitespace"},
		{name: "unknown engine", mutate: func(c *Config) { c.Models[0].Engine = "tgi" }, wantErr: "models[0].engine"},
		{name: "missing path", mutate: func(c *Config) { c.Models[0].Path = "" }, wantErr: "models[0].path"},
		{name: "gpu shared across models", mutate: func(c *Config) { c.Models = append(c.Models, engineModel(testEngineGPU, "other", 2)) }, wantErr: "models[1].gpus: GPU 2 is already used by models[0] (granite-4.2-8b)"},
		{name: "gpu listed twice", mutate: func(c *Config) { c.Models[0].GPUs = []int{2, 2} }, wantErr: "lists GPU 2 twice"},
		{name: "negative gpu", mutate: func(c *Config) { c.Models[0].GPUs = []int{-1} }, wantErr: "is not a GPU index"},
		{name: "gpus not divisible", mutate: func(c *Config) { c.Models[0].GPUs = []int{0, 1, 2}; c.Models[0].GPUsPerBackend = 2 }, wantErr: "not divisible"},
		{name: "llamacpp on cpu", mutate: func(c *Config) { c.Models[0].GPUs = nil }},
		{name: "gpu-only engine without gpus", mutate: func(c *Config) { c.Models = []Model{engineModel(testEngineGPU, "v")} }, wantErr: "it cannot run on CPU"},
		{name: "zero context", mutate: func(c *Config) { c.Models[0].Context = 0 }, wantErr: "models[0].context"},
		{name: "zero parallel", mutate: func(c *Config) { c.Models[0].Parallel = 0 }, wantErr: "models[0].parallel"},
		// An engine's own option rules are the engine's; this package only has
		// to carry the block and let the error through with the field named.
		// The rules themselves are tested where they live, in the engine.
		{name: "engine rejects its own options", mutate: func(c *Config) {
			c.Models = []Model{engineBlockModel(testEnginePick, "p", "knob: 2\n", 0)}
		}, wantErr: "models[0].picky.knob 2 must be <= 1"},
		{name: "engine accepts its own options", mutate: func(c *Config) {
			c.Models = []Model{engineBlockModel(testEnginePick, "p", "knob: 0.5\n", 0)}
		}},
		{name: "engine rejects an unknown key in its block", mutate: func(c *Config) {
			c.Models = []Model{engineBlockModel(testEnginePick, "p", "knobb: 1\n", 0)}
		}, wantErr: "unknown key knobb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(c)
			err := c.Validate(envOf(tc.env))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateFullExample(t *testing.T) {
	cfg := parseFile(t, "testdata/full.yaml")
	if err := cfg.Validate(envOf(map[string]string{"VIIWORK_MESH_SECRET": testSecret})); err != nil {
		t.Fatalf("testdata/full.yaml must validate: %v", err)
	}
}

func TestMeshKeys(t *testing.T) {
	c := validConfig()
	c.Mesh.Open = false
	keys, err := c.MeshKeys(envOf(map[string]string{"VIIWORK_MESH_SECRET": testSecret, "VIIWORK_MESH_SECRET_PREV": testSecret}))
	if err != nil {
		t.Fatal(err)
	}
	if keys.Open() || len(keys.Primary) != 32 || len(keys.Previous) != 32 {
		t.Errorf("secured keys: open=%v primary=%d previous=%d", keys.Open(), len(keys.Primary), len(keys.Previous))
	}

	open := validConfig()
	keys, err = open.MeshKeys(envOf(nil))
	if err != nil || !keys.Open() {
		t.Errorf("open mesh: keys=%+v err=%v", keys, err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if _, err := Load(write("open.yaml", "mesh:\n  open: true\n"), envOf(nil)); err != nil {
		t.Errorf("open mesh file: %v", err)
	}
	if _, err := Load(write("closed.yaml", "mesh:\n  open: false\n"), envOf(nil)); err == nil || !strings.Contains(err.Error(), "mesh.open is not true") {
		t.Errorf("closed mesh without secret: err = %v", err)
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml"), envOf(nil)); err == nil || !strings.Contains(err.Error(), "reading config") {
		t.Errorf("missing file: err = %v", err)
	}
}
