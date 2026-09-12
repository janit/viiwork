package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/mesh/meshtest"
	"github.com/janit/viiwork/v2/meshapi"
)

// meshPorts hands out distinct gossip ports for loopback members on a
// meshtest network; no real socket is opened on them.
var meshPorts atomic.Int32

func init() { meshPorts.Store(21000) }

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// meshTune puts a node on a meshtest network at a loopback address of its own,
// so members' API addresses (loopback plus the real listener port) are
// dialable, and seeds it with the given addresses.
func meshTune(net *meshtest.Network, name string, addr *netip.AddrPort, seeds ...func() netip.AddrPort) func(*mesh.Options) {
	return func(o *mesh.Options) {
		*addr = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(meshPorts.Add(1)))
		tr := net.TransportAt(name, *addr)
		o.Advertise = addr.Addr()
		o.BindPort = int(addr.Port())
		o.TailnetSocket, o.LocalAPISocket = "", ""
		o.AddrCheck = func(netip.Addr) error { return nil }
		o.RejoinInterval = 300 * time.Millisecond
		for _, s := range seeds {
			o.Seeds = append(o.Seeds, s().String())
		}
		o.Tune = func(c *memberlist.Config) {
			c.Transport = tr
			c.ProbeInterval = 200 * time.Millisecond
			c.ProbeTimeout = 100 * time.Millisecond
			c.SuspicionMult = 2
			c.GossipInterval = 50 * time.Millisecond
			c.PushPullInterval = time.Second
			c.TCPTimeout = 500 * time.Millisecond
			c.DeadNodeReclaimTime = time.Second
		}
	}
}

func loopbackListen(network, _ string) (net.Listener, error) {
	return net.Listen(network, "127.0.0.1:0")
}

type testNode struct {
	*Node
	cfgPath  string
	stateDir string
	log      *syncBuffer
	gossip   netip.AddrPort
	cancel   context.CancelFunc
	done     chan error
}

// startNode loads cfgYAML, builds the node and runs it until the test ends.
func startNode(t *testing.T, net *meshtest.Network, name string, models []fakeModel, extra string, tune func(*testNode) func(*mesh.Options)) *testNode {
	t.Helper()
	dir := t.TempDir()
	tn := &testNode{cfgPath: filepath.Join(dir, "viiwork.yaml"), stateDir: filepath.Join(dir, "state"), log: &syncBuffer{}}
	if err := os.MkdirAll(tn.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, tn.cfgPath, nodeConfig(name, tn.stateDir, models, extra))
	cfg, err := config.Load(tn.cfgPath, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	o := Options{ConfigPath: tn.cfgPath, Version: "v2-test", Log: tn.log, LookupEnv: noEnv, Listen: loopbackListen}
	if tune != nil {
		o.MeshTune = tune(tn)
	} else {
		o.MeshTune = meshTune(net, name, &tn.gossip)
	}
	n, err := New(cfg, o)
	if err != nil {
		t.Fatal(err)
	}
	tn.Node = n
	ctx, cancel := context.WithCancel(context.Background())
	tn.cancel = cancel
	tn.done = make(chan error, 1)
	go func() { tn.done <- n.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-tn.done:
		case <-time.After(30 * time.Second):
			t.Error("node did not stop within 30s")
		}
	})
	select {
	case <-n.Ready():
	case err := <-tn.done:
		t.Fatalf("Run returned before Ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("node not ready within 10s")
	}
	return tn
}

func noEnv(string) (string, bool) { return "", false }

func (tn *testNode) url(path string) string { return "http://" + tn.APIAddr() + path }

func getJSON(t *testing.T, url string, v any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if v != nil {
		_ = json.NewDecoder(resp.Body).Decode(v)
	}
	return resp.StatusCode
}

func (tn *testNode) status(t *testing.T) meshapi.NodeStatus {
	t.Helper()
	var st meshapi.NodeStatus
	getJSON(t, tn.url(meshapi.PathStatus), &st)
	return st
}

func modelOf(st meshapi.NodeStatus, name string) (meshapi.ModelStatus, bool) {
	for _, m := range st.Models {
		if m.Name == name {
			return m, true
		}
	}
	return meshapi.ModelStatus{}, false
}

func healthyModel(st meshapi.NodeStatus, name string) bool {
	m, ok := modelOf(st, name)
	if !ok || len(m.Backends) == 0 {
		return false
	}
	for _, b := range m.Backends {
		if b.Status != meshapi.StatusHealthy {
			return false
		}
	}
	return true
}

func pidsOf(st meshapi.NodeStatus, name string) []int {
	m, _ := modelOf(st, name)
	var pids []int
	for _, b := range m.Backends {
		pids = append(pids, b.PID)
	}
	return pids
}

func until(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %v", what, within)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestNodeNewErrors(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(state, 0o700) })
	path := filepath.Join(dir, "viiwork.yaml")
	writeFile(t, path, nodeConfig("n1", state, nil, ""))
	cfg, err := config.Load(path, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg, Options{ConfigPath: path, Log: io.Discard, LookupEnv: noEnv}); err == nil || !strings.Contains(err.Error(), state) {
		t.Errorf("L1: err = %v, want one naming %s", err, state)
	}

	state2 := filepath.Join(dir, "state2")
	if err := os.MkdirAll(state2, 0o755); err != nil {
		t.Fatal(err)
	}
	aliases := filepath.Join(state2, "aliases.json")
	writeFile(t, aliases, "garbage{")
	cfg.Node.StateDir = state2
	if _, err := New(cfg, Options{ConfigPath: path, Log: io.Discard, LookupEnv: noEnv}); err == nil || !strings.Contains(err.Error(), aliases) {
		t.Errorf("L2: err = %v, want one naming %s", err, aliases)
	}
}

func TestNodeServes(t *testing.T) {
	tn := startNode(t, meshtest.NewNetwork(), "n1", []fakeModel{{name: "m", readyAfter: "100ms"}}, "", nil)
	until(t, 10*time.Second, "m healthy with /health 200", func() bool {
		return getJSON(t, tn.url(meshapi.PathHealth), nil) == 200 && healthyModel(tn.status(t), "m")
	})

	resp, err := http.Post(tn.url(meshapi.PathChatCompletions), "application/json", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get(meshapi.HeaderNode) != "n1" {
		t.Fatalf("L4: %d %v %s", resp.StatusCode, resp.Header, body)
	}
	until(t, 3*time.Second, "counters on /v1/status", func() bool {
		m, _ := modelOf(tn.status(t), "m")
		return m.RequestsTotal == 1 && m.TokensTotal == 3
	})

	pids := pidsOf(tn.status(t), "m")
	addr := tn.APIAddr()
	start := time.Now()
	tn.cancel()
	select {
	case err := <-tn.done:
		tn.done <- err // for the cleanup
		if err != nil {
			t.Errorf("L5: Run returned %v", err)
		}
	case <-time.After(2*time.Second + 11*time.Second):
		t.Fatal("L5: Run did not return within respawn_grace + 11s")
	}
	t.Logf("shutdown took %s", time.Since(start))
	for _, pid := range pids {
		if pid == 0 || !processGone(pid) {
			t.Errorf("L5: engine process %d is still running", pid)
		}
	}
	if conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		conn.Close()
		t.Error("L5: the API listener is still open")
	}
}

func TestNodeShutdownLetsStreamFinish(t *testing.T) {
	tn := startNode(t, meshtest.NewNetwork(), "n1", []fakeModel{{name: "m", streamHold: "1s"}}, "", nil)
	until(t, 10*time.Second, "m healthy", func() bool { return healthyModel(tn.status(t), "m") })

	resp, err := http.Post(tn.url(meshapi.PathChatCompletions), "application/json", strings.NewReader(`{"model":"m","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	if line, _ := reader.ReadString('\n'); !strings.Contains(line, "hi") {
		t.Fatalf("L6: first line %q", line)
	}
	tn.cancel()
	rest, _ := io.ReadAll(reader)
	if !strings.Contains(string(rest), "[DONE]") {
		t.Errorf("L6: the in-flight stream was cut at shutdown: %q", rest)
	}
	select {
	case err := <-tn.done:
		tn.done <- err
		if err != nil {
			t.Errorf("L6: Run returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("L6: Run did not return")
	}
}

func TestNodeDuplicateNameIsFatal(t *testing.T) {
	net := meshtest.NewNetwork()
	first := startNode(t, net, "dup", nil, "", nil)
	var secondAddr netip.AddrPort
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	_ = os.MkdirAll(state, 0o755)
	path := filepath.Join(dir, "viiwork.yaml")
	writeFile(t, path, nodeConfig("dup", state, nil, ""))
	cfg, err := config.Load(path, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	n, err := New(cfg, Options{ConfigPath: path, Log: io.Discard, LookupEnv: noEnv, Listen: loopbackListen,
		MeshTune: meshTune(net, "dup-second", &secondAddr, func() netip.AddrPort { return first.gossip })})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), `duplicate node name "dup"`) {
			t.Errorf("L7: Run returned %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("L7: Run did not return the duplicate-name error")
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		t.Error("L7: the context was cancelled before Run returned")
	}
}

// A node answers /health before it has applied its models: Run starts the API
// listener, then blocks in mesh.Start, and only reaches Supervisor.Apply after
// that. A node that cannot reach tailscaled sits in that gap indefinitely. The
// model count therefore comes from the running configuration rather than from
// the supervisor, so Decision 5's 503 fires for a node that is serving nothing
// (found by the gb1 trial, 2026-09-12).
func TestNodeHealthBeforeModelsApplied(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "viiwork.yaml")
	writeFile(t, path, nodeConfig("n1", state, []fakeModel{{name: "m"}}, ""))
	cfg, err := config.Load(path, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	n, err := New(cfg, Options{ConfigPath: path, Version: "v2-test", Log: io.Discard, LookupEnv: noEnv})
	if err != nil {
		t.Fatal(err)
	}

	healthy, total, models := n.health()
	if healthy != 0 || total != 0 || models != 1 {
		t.Errorf("health() = (%d, %d, %d), want (0, 0, 1): a configured model must count before Supervisor.Apply", healthy, total, models)
	}

	rec := get(NewServer(ServerDeps{Health: n.health, Version: "v2-test", Self: "n1", Started: time.Now()}), meshapi.PathHealth)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusServiceUnavailable || body["status"] != "unhealthy" {
		t.Errorf("/health before Apply = %d %s, want 503 unhealthy", rec.Code, rec.Body.String())
	}

	// A pure router, with no models configured at all, stays healthy (R3).
	writeFile(t, path, nodeConfig("n2", state, nil, ""))
	cfg2, err := config.Load(path, noEnv)
	if err != nil {
		t.Fatal(err)
	}
	n2, err := New(cfg2, Options{ConfigPath: path, Version: "v2-test", Log: io.Discard, LookupEnv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, models := n2.health(); models != 0 {
		t.Errorf("a node with no models configured reports %d models, want 0", models)
	}
}
