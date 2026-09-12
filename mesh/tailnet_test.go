package mesh

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeLocalAPI serves the tailscaled LocalAPI on a unix socket and records the
// Host header of every request.
type fakeLocalAPI struct {
	socket string
	mu     sync.Mutex
	hosts  []string
}

func startLocalAPI(t *testing.T, status int, body string) *fakeLocalAPI {
	t.Helper()
	dir, err := os.MkdirTemp("", "ts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fakeLocalAPI{socket: filepath.Join(dir, "tailscaled.sock")}
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hosts = append(f.hosts, r.Host)
		f.mu.Unlock()
		if r.URL.Path != "/localapi/v0/status" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	return f
}

func statusFixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/tailnet-status.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestReadTailnetStatus(t *testing.T) {
	api := startLocalAPI(t, 200, statusFixture(t))
	st, err := ReadTailnetStatus(context.Background(), api.socket)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range st.Peers {
		names = append(names, p.HostName)
	}
	if !slices.Equal(names, []string{"broken", "gb2", "node-a", "odd", "phone"}) {
		t.Errorf("peer order = %v", names)
	}
	if ip, ok := st.SelfIPv4(); !ok || ip != netip.MustParseAddr("100.64.0.105") {
		t.Errorf("SelfIPv4 = %v, %v", ip, ok)
	}
	if len(api.hosts) != 1 || api.hosts[0] != "local-tailscaled.sock" {
		t.Errorf("Host headers = %v", api.hosts)
	}
	if !slices.Equal(st.Peers[0].IPs, []netip.Addr{netip.MustParseAddr("100.64.0.6")}) {
		t.Errorf("broken peer IPs = %v, want only 100.64.0.6", st.Peers[0].IPs)
	}
}

func TestTailnetFeeder(t *testing.T) {
	api := startLocalAPI(t, 200, statusFixture(t))
	f := TailnetFeeder(api.socket, 7946)
	if f.Name() != "tailnet" {
		t.Errorf("Name = %q", f.Name())
	}
	got, err := f.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"100.64.0.2:7946", "100.64.0.6:7946"}) {
		t.Errorf("candidates = %v", got)
	}
}

func TestTailnetNotRunning(t *testing.T) {
	api := startLocalAPI(t, 200, strings.Replace(statusFixture(t), `"Running"`, `"NeedsLogin"`, 1))
	if _, err := TailnetFeeder(api.socket, 7946).Candidates(context.Background()); err == nil || !strings.Contains(err.Error(), "NeedsLogin") {
		t.Errorf("err = %v, want it to name NeedsLogin", err)
	}
	st, err := ReadTailnetStatus(context.Background(), api.socket)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.SelfIPv4(); ok {
		t.Error("SelfIPv4 must be false unless tailscaled is Running")
	}
}

func TestReadTailnetStatusErrors(t *testing.T) {
	dir, err := os.MkdirTemp("", "ts")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if _, err := ReadTailnetStatus(context.Background(), filepath.Join(dir, "nothing.sock")); err == nil {
		t.Error("a socket nobody listens on must be an error")
	}
	api := startLocalAPI(t, 500, "boom")
	if _, err := ReadTailnetStatus(context.Background(), api.socket); err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("500: err = %v", err)
	}
}
