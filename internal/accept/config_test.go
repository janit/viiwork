package accept

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var testSecret = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

func envWith(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := vars[k]
		return v, ok
	}
}

type cfgFixture struct {
	open         bool
	apiPort      int
	aPath        string // default /models/a.gguf
	bTimeout     string // default 45m; "-" = unset
	aTimeout     string // "" = unset
	modelsRoot   string
	aSize, bSize int64
}

// write creates the config file and the weights under the models root and
// returns the config path.
func (f *cfgFixture) write(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	f.modelsRoot = filepath.Join(dir, "models")
	if err := os.MkdirAll(f.modelsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int64{"a.gguf": max(f.aSize, 1024), "b.gguf": max(f.bSize, 1024)} {
		w, err := os.Create(filepath.Join(f.modelsRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Truncate(size); err != nil {
			t.Fatal(err)
		}
		w.Close()
	}

	var b strings.Builder
	b.WriteString("node:\n  name: node-a\n  state_dir: " + filepath.Join(dir, "state") + "\n")
	if f.apiPort != 0 {
		b.WriteString("api:\n  port: " + strconv.Itoa(f.apiPort) + "\n")
	}
	b.WriteString("mesh:\n  network: tailnet\n")
	if f.open {
		b.WriteString("  open: true\n")
	}
	aPath := f.aPath
	if aPath == "" {
		aPath = "/models/a.gguf"
	}
	b.WriteString("models:\n")
	b.WriteString("  - name: a\n    engine: llamacpp\n    path: " + aPath + "\n    gpus: [0]\n    parallel: 2\n    context: 16384\n")
	if f.aTimeout != "" {
		b.WriteString("    startup_timeout: " + f.aTimeout + "\n")
	}
	b.WriteString("  - name: b\n    engine: llamacpp\n    path: /models/b.gguf\n    gpus: [1, 2]\n    gpus_per_backend: 2\n    parallel: 2\n    context: 8192\n")
	switch f.bTimeout {
	case "":
		b.WriteString("    startup_timeout: 45m\n")
	case "-":
	default:
		b.WriteString("    startup_timeout: " + f.bTimeout + "\n")
	}
	path := filepath.Join(dir, "viiwork.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

var securedEnv = envWith(map[string]string{"VIIWORK_MESH_SECRET": testSecret})

func checkNames(r Report) []string {
	var names []string
	for _, c := range r.Checks {
		names = append(names, c.Name)
	}
	return names
}

func TestConfigSummaryValid(t *testing.T) { // CF1
	f := &cfgFixture{}
	path := f.write(t)
	sum, r := SummarizeConfig(path, securedEnv, f.modelsRoot, "")
	if !r.Pass() {
		t.Fatalf("checks: %+v", r.Checks)
	}
	want := []string{"config valid", "mesh secured", "api port 8086", "gossip port 7946",
		"weights a", "startup_timeout a", "weights b", "startup_timeout b"}
	if got := checkNames(r); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("checks %q, want %q", got, want)
	}
	if r.Command != "config" || r.Target != path {
		t.Errorf("command %q target %q", r.Command, r.Target)
	}
	if sum.Node != "node-a" || sum.Mesh != "secured" || sum.Network != "tailnet" || sum.APIPort != 8086 || sum.GossipPort != 7946 {
		t.Errorf("summary: %+v", sum)
	}
	if len(sum.Models) != 2 {
		t.Fatalf("models: %+v", sum.Models)
	}
	a, b := sum.Models[0], sum.Models[1]
	if a.Name != "a" || a.Engine != "llamacpp" || a.Backends != 1 || a.Slots != 2 || a.CtxPerSlot != 16384 || a.StartupTimeout != 0 || a.WeightsBytes != 1024 {
		t.Errorf("a: %+v", a)
	}
	if b.Name != "b" || b.Backends != 1 || b.Slots != 2 || b.StartupTimeout != 45*time.Minute || len(b.GPUs) != 2 {
		t.Errorf("b: %+v", b)
	}
}

func TestConfigSplitModelNeedsLongStartup(t *testing.T) { // CF2
	f := &cfgFixture{bTimeout: "-"}
	_, r := SummarizeConfig(f.write(t), securedEnv, f.modelsRoot, "")
	if c := checkByName(t, r, "startup_timeout b"); c.Pass {
		t.Errorf("startup_timeout b passed: %+v", c)
	}
	if c := checkByName(t, r, "startup_timeout a"); !c.Pass {
		t.Errorf("startup_timeout a failed: %+v", c)
	}
	f = &cfgFixture{bTimeout: "30m"}
	_, r = SummarizeConfig(f.write(t), securedEnv, f.modelsRoot, "")
	if c := checkByName(t, r, "startup_timeout b"); c.Pass || !strings.Contains(c.Detail, "30m0s") {
		t.Errorf("startup_timeout b passed with 30m: %+v", c)
	}
}

func TestConfigLargeWeightsNeedLongStartup(t *testing.T) { // CF3
	f := &cfgFixture{aSize: 21 << 30}
	_, r := SummarizeConfig(f.write(t), securedEnv, f.modelsRoot, "")
	if c := checkByName(t, r, "startup_timeout a"); c.Pass {
		t.Errorf("startup_timeout a passed with 21 GiB weights: %+v", c)
	}
	f = &cfgFixture{aSize: 21 << 30, aTimeout: "45m"}
	_, r = SummarizeConfig(f.write(t), securedEnv, f.modelsRoot, "")
	if c := checkByName(t, r, "startup_timeout a"); !c.Pass {
		t.Errorf("startup_timeout a failed with 45m: %+v", c)
	}
}

func TestConfigOpenMesh(t *testing.T) { // CF4
	f := &cfgFixture{open: true}
	_, r := SummarizeConfig(f.write(t), envWith(nil), f.modelsRoot, "")
	for _, name := range []string{"config valid", "mesh open"} {
		if c := checkByName(t, r, name); !c.Pass {
			t.Errorf("%s: %+v", name, c)
		}
	}
}

func TestConfigRequireMesh(t *testing.T) { // CF8
	f := &cfgFixture{open: true}
	_, r := SummarizeConfig(f.write(t), envWith(nil), f.modelsRoot, "secured")
	c := checkByName(t, r, "mesh open")
	if c.Pass || c.Detail != "required: secured" {
		t.Errorf("mesh open: %+v", c)
	}
	_, r = SummarizeConfig(f.write(t), envWith(nil), f.modelsRoot, "open")
	if c := checkByName(t, r, "mesh open"); !c.Pass {
		t.Errorf("mesh open with required open: %+v", c)
	}
}

func TestConfigMissingWeights(t *testing.T) { // CF5
	f := &cfgFixture{}
	path := f.write(t)
	if err := os.Remove(filepath.Join(f.modelsRoot, "a.gguf")); err != nil {
		t.Fatal(err)
	}
	_, r := SummarizeConfig(path, securedEnv, f.modelsRoot, "")
	c := checkByName(t, r, "weights a")
	if c.Pass || !strings.Contains(c.Detail, filepath.Join(f.modelsRoot, "a.gguf")) {
		t.Errorf("weights a: %+v", c)
	}
	// Judged on gpus_per_backend alone: a runs one GPU per backend.
	if c := checkByName(t, r, "startup_timeout a"); !c.Pass {
		t.Errorf("startup_timeout a: %+v", c)
	}
}

func TestConfigV1File(t *testing.T) { // CF6
	path := filepath.Join(t.TempDir(), "viiwork.yaml")
	v1 := "server:\n  port: 8080\nmodel:\n  path: /models/x.gguf\ngpus:\n  count: 2\n"
	if err := os.WriteFile(path, []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, r := SummarizeConfig(path, securedEnv, "", "")
	if len(r.Checks) != 1 || r.Checks[0].Name != "config valid" || r.Checks[0].Pass ||
		!strings.Contains(r.Checks[0].Detail, "docs/migrating-to-v2.md") {
		t.Errorf("checks: %+v", r.Checks)
	}
	if sum.Node != "" || len(sum.Models) != 0 {
		t.Errorf("summary not zero: %+v", sum)
	}
}

func TestConfigAPIPort(t *testing.T) { // CF7
	f := &cfgFixture{apiPort: 9304}
	_, r := SummarizeConfig(f.write(t), securedEnv, f.modelsRoot, "")
	if c := checkByName(t, r, "api port 8086"); c.Pass || !strings.Contains(c.Detail, "9304") {
		t.Errorf("api port 8086: %+v", c)
	}
}

func TestConfigMissingSecretNamesVariable(t *testing.T) {
	f := &cfgFixture{}
	_, r := SummarizeConfig(f.write(t), envWith(nil), f.modelsRoot, "")
	if len(r.Checks) != 1 || r.Checks[0].Pass || !strings.Contains(r.Checks[0].Detail, "VIIWORK_MESH_SECRET") {
		t.Errorf("checks: %+v", r.Checks)
	}
}

// A split GGUF loads every shard, so the weights are the sum of the shards the
// first one names.
func TestConfigSplitGGUFWeights(t *testing.T) {
	f := &cfgFixture{aPath: "/models/big/a-00001-of-00003.gguf"}
	path := f.write(t)
	shards := filepath.Join(f.modelsRoot, "big")
	if err := os.MkdirAll(shards, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		w, err := os.Create(filepath.Join(shards, fmt.Sprintf("a-%05d-of-00003.gguf", i)))
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Truncate(8 << 30); err != nil {
			t.Fatal(err)
		}
		w.Close()
	}
	sum, r := SummarizeConfig(path, securedEnv, f.modelsRoot, "")
	if sum.Models[0].WeightsBytes != 24<<30 {
		t.Errorf("weights bytes %d, want %d", sum.Models[0].WeightsBytes, int64(24<<30))
	}
	if c := checkByName(t, r, "startup_timeout a"); c.Pass {
		t.Errorf("24 GiB over three shards passed with the default timeout: %+v", c)
	}
}

// A model directory (vLLM, FreeToken) is the size of the files in it.
func TestConfigDirectoryWeights(t *testing.T) {
	f := &cfgFixture{aPath: "/models/a-dir"}
	path := f.write(t)
	d := filepath.Join(f.modelsRoot, "a-dir", "sub")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{filepath.Join(f.modelsRoot, "a-dir", "one.safetensors"), filepath.Join(d, "two.safetensors")} {
		if err := os.WriteFile(name, make([]byte, 3000), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sum, r := SummarizeConfig(path, securedEnv, f.modelsRoot, "")
	if c := checkByName(t, r, "weights a"); !c.Pass {
		t.Errorf("weights a: %+v", c)
	}
	if sum.Models[0].WeightsBytes != 6000 {
		t.Errorf("weights bytes %d, want 6000", sum.Models[0].WeightsBytes)
	}
}
