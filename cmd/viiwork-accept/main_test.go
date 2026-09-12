package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/accept"
	"github.com/janit/viiwork/v2/meshapi"
)

type result struct {
	code           int
	stdout, stderr string
}

func runArgs(ctx context.Context, env map[string]string, args ...string) result {
	var stdout, stderr bytes.Buffer
	code := run(args, runEnv{
		Stdout: &stdout,
		Stderr: &stderr,
		LookupEnv: func(k string) (string, bool) {
			v, ok := env[k]
			return v, ok
		},
		Accept: accept.DefaultEnv(),
		Ctx:    ctx,
	})
	return result{code, stdout.String(), stderr.String()}
}

func runPlain(args ...string) result { return runArgs(context.Background(), nil, args...) }

// clusterServer answers /v1/cluster with node-a in the given state.
func clusterServer(t *testing.T, state string) *httptest.Server {
	t.Helper()
	st := meshapi.NodeStatus{Node: "node-a", Models: []meshapi.ModelStatus{{Name: "a", Slots: 1,
		Backends: []meshapi.BackendStatus{{ID: "a/0", Status: meshapi.StatusHealthy, Slots: 1}}}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case meshapi.PathCluster:
			json.NewEncoder(w).Encode(meshapi.ClusterResponse{View: "node-b", Members: []meshapi.Member{
				{Node: "node-a", State: state, Status: &st},
			}})
		case meshapi.PathStatus:
			json.NewEncoder(w).Encode(st)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func hostPort(srv *httptest.Server) string { return strings.TrimPrefix(srv.URL, "http://") }

func TestUsage(t *testing.T) { // E1
	r := runPlain()
	if r.code != 2 {
		t.Errorf("exit %d", r.code)
	}
	for _, c := range []string{"ports", "config", "join", "ready", "gone", "models", "saturate", "alias"} {
		if !strings.Contains(r.stderr, c) {
			t.Errorf("usage lacks %q:\n%s", c, r.stderr)
		}
	}
	if r := runPlain("frobnicate"); r.code != 2 {
		t.Errorf("unknown command: exit %d", r.code)
	}
	if r := runPlain("ports", "listen"); r.code != 2 {
		t.Errorf("unknown ports subcommand: exit %d", r.code)
	}
}

func TestJoin(t *testing.T) { // E2, E3, E4
	alive := clusterServer(t, meshapi.MemberAlive)
	r := runPlain("join", "--observer", hostPort(alive), "--node", "node-a", "--timeout", "1s")
	if r.code != 0 || !strings.Contains(r.stdout, "PASS  joined node-a") {
		t.Errorf("E2: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	dead := clusterServer(t, meshapi.MemberDead)
	if r := runPlain("join", "--observer", hostPort(dead), "--node", "node-a", "--timeout", "1s"); r.code != 1 {
		t.Errorf("E3: exit %d\n%s", r.code, r.stdout)
	}
	if r := runPlain("join", "--node", "node-a"); r.code != 2 || !strings.Contains(r.stderr, "--observer") {
		t.Errorf("E4: exit %d\n%s", r.code, r.stderr)
	}
}

func writeConfig(t *testing.T, open bool) string {
	t.Helper()
	dir := t.TempDir()
	cfg := "node:\n  name: node-a\n  state_dir: " + filepath.Join(dir, "state") + "\nmodels:\n" +
		"  - name: a\n    engine: llamacpp\n    path: " + filepath.Join(dir, "a.gguf") + "\n    gpus: [0]\n    context: 4096\n"
	if open {
		cfg += "mesh:\n  open: true\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "a.gguf"), make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "viiwork.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// base64Key matches a standard-base64 encoding of 32 bytes.
var base64Key = regexp.MustCompile(`[A-Za-z0-9+/]{43}=`)

func TestConfigDummySecret(t *testing.T) { // E5, E6
	path := writeConfig(t, false)
	r := runPlain("config", "--file", path, "--dummy-secret")
	if r.code != 0 {
		t.Errorf("E5: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if k := base64Key.FindString(r.stdout + r.stderr); k != "" {
		t.Errorf("E5: output carries a key-shaped string %q", k)
	}
	if !strings.Contains(r.stdout, "llamacpp") {
		t.Errorf("E5: no summary table:\n%s", r.stdout)
	}

	r = runPlain("config", "--file", path)
	if r.code != 1 || !strings.Contains(r.stdout, "FAIL  config valid") || !strings.Contains(r.stdout, "VIIWORK_MESH_SECRET") {
		t.Errorf("E6: exit %d\n%s", r.code, r.stdout)
	}

	// An open mesh must not be handed a secret: that would fail validation.
	if r := runPlain("config", "--file", writeConfig(t, true), "--dummy-secret"); r.code != 0 {
		t.Errorf("open mesh with --dummy-secret: exit %d\n%s", r.code, r.stdout)
	}
	// A real secret in the environment is used as is.
	real := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	if r := runArgs(context.Background(), map[string]string{"VIIWORK_MESH_SECRET": real}, "config", "--file", path, "--json"); r.code != 0 || strings.Contains(r.stdout, real) {
		t.Errorf("real secret: exit %d\n%s", r.code, r.stdout)
	}
	if r := runPlain("config", "--file", path, "--require-mesh", "maybe"); r.code != 2 {
		t.Errorf("--require-mesh maybe: exit %d", r.code)
	}
}

func TestAliasBearer(t *testing.T) { // E7, E8
	var mu sync.Mutex
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set(meshapi.HeaderAlias, "s")
		w.Header().Set(meshapi.HeaderModel, "m")
		json.NewEncoder(w).Encode(map[string]any{"model": "m", "choices": []any{map[string]any{
			"message": map[string]any{"content": "ready"}, "finish_reason": "stop"}}})
	}))
	defer srv.Close()

	env := map[string]string{"GW_KEY": "test-key-7f3a9c"}
	r := runArgs(context.Background(), env, "alias", "--entry", srv.URL, "--alias", "s", "--expect-model", "m", "--bearer-env", "GW_KEY")
	if r.code != 0 {
		t.Errorf("E7: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	mu.Lock()
	if len(auth) != 1 || auth[0] != "Bearer test-key-7f3a9c" {
		t.Errorf("E7: authorization %q", auth)
	}
	mu.Unlock()
	if strings.Contains(r.stdout+r.stderr, "test-key-7f3a9c") {
		t.Errorf("E7: the key was printed")
	}

	r = runPlain("alias", "--entry", srv.URL, "--alias", "s", "--expect-model", "m", "--bearer-env", "GW_KEY")
	if r.code != 2 || !strings.Contains(r.stderr, "GW_KEY") {
		t.Errorf("E8: exit %d\n%s", r.code, r.stderr)
	}
}

func TestReadyJSON(t *testing.T) { // E9
	srv := clusterServer(t, meshapi.MemberAlive)
	r := runPlain("ready", "--node", hostPort(srv), "--json")
	var rep accept.Report
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil || rep.Command != "ready" || r.code != 0 {
		t.Errorf("E9: exit %d, %v\n%s", r.code, err, r.stdout)
	}
}

func TestGoneInterrupted(t *testing.T) { // E10
	srv := clusterServer(t, meshapi.MemberAlive)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r := runArgs(ctx, nil, "gone", "--observer", hostPort(srv), "--node", "node-a", "--json")
	var rep accept.Report
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil {
		t.Fatalf("E10: %v\n%s", err, r.stdout)
	}
	if r.code != 1 || len(rep.Checks) == 0 || rep.Checks[0].Detail != "interrupted" {
		t.Errorf("E10: exit %d, %+v", r.code, rep.Checks)
	}
}

func TestCommandHelp(t *testing.T) { // E11
	r := runPlain("models", "--help")
	if r.code != 0 {
		t.Errorf("exit %d", r.code)
	}
	for _, want := range []string{"--node", "--model", "--via", "--timeout", "--json", "10m0s"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("help lacks %q:\n%s", want, r.stdout)
		}
	}
	for _, c := range [][]string{{"ports", "serve"}, {"ports", "probe"}, {"config"}, {"join"}, {"ready"}, {"gone"}, {"saturate"}, {"alias"}} {
		if r := runPlain(append(c, "--help")...); r.code != 0 || !strings.Contains(r.stdout, "--json") {
			t.Errorf("%v --help: exit %d\n%s", c, r.code, r.stdout)
		}
	}
}

func TestPortsServeStopsOnTime(t *testing.T) {
	r := runPlain("ports", "serve", "--addr", "127.0.0.1", "--port", "0", "--for", "200ms")
	if r.code != 0 {
		t.Errorf("exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	if r := runPlain("ports", "serve", "--addr", "not-an-address"); r.code != 2 {
		t.Errorf("bad --addr: exit %d", r.code)
	}
}
