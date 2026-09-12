package mesh

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func routeFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/proc-net-route.txt")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

const routeHeader = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"

func addrsOf(name string, addrs ...string) func(string) ([]netip.Addr, error) {
	return func(iface string) ([]netip.Addr, error) {
		if iface != name {
			return nil, fmt.Errorf("unexpected interface %s", iface)
		}
		var out []netip.Addr
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
}

func TestDefaultRouteIPv4Teddy(t *testing.T) {
	var asked []string
	ip, err := DefaultRouteIPv4(routeFixture(t), func(name string) ([]netip.Addr, error) {
		asked = append(asked, name)
		return addrsOf("eno1np0", "fe80::1", "192.168.42.144")(name)
	})
	if err != nil || ip != netip.MustParseAddr("192.168.42.144") {
		t.Errorf("DefaultRouteIPv4 = %v, %v", ip, err)
	}
	if len(asked) != 1 || asked[0] != "eno1np0" {
		t.Errorf("asked about %v, want only eno1np0", asked)
	}
}

func TestDefaultRouteIPv4LowestMetric(t *testing.T) {
	table := routeHeader +
		"wlan0\t00000000\t0101A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0\n" +
		"eth0\t00000000\t0102A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"
	ip, err := DefaultRouteIPv4([]byte(table), addrsOf("eth0", "192.168.2.10"))
	if err != nil || ip != netip.MustParseAddr("192.168.2.10") {
		t.Errorf("DefaultRouteIPv4 = %v, %v, want eth0's address", ip, err)
	}
}

func TestDefaultRouteIPv4RouteDown(t *testing.T) {
	table := routeHeader + "eth0\t00000000\t0102A8C0\t0000\t0\t0\t100\t00000000\t0\t0\t0\n"
	if _, err := DefaultRouteIPv4([]byte(table), addrsOf("eth0", "192.168.2.10")); err == nil || !strings.Contains(err.Error(), "no IPv4 default route") {
		t.Errorf("err = %v", err)
	}
}

func TestDefaultRouteIPv4BadInput(t *testing.T) {
	if _, err := DefaultRouteIPv4([]byte(routeHeader), addrsOf("eth0")); err == nil || !strings.Contains(err.Error(), "no IPv4 default route") {
		t.Errorf("header only: err = %v", err)
	}
	if _, err := DefaultRouteIPv4(routeFixture(t), addrsOf("eno1np0", "fe80::1")); err == nil || !strings.Contains(err.Error(), "eno1np0") {
		t.Errorf("no usable IPv4: err = %v", err)
	}
}

func failingAdvertiseDeps(t *testing.T) advertiseDeps {
	return advertiseDeps{
		readRoutes: func() ([]byte, error) { t.Error("readRoutes called"); return nil, errors.New("no") },
		ifaceAddrs: func(string) ([]netip.Addr, error) { t.Error("ifaceAddrs called"); return nil, errors.New("no") },
		tailnet: func(context.Context) (TailnetStatus, error) {
			t.Error("tailnet called")
			return TailnetStatus{}, errors.New("no")
		},
		poll: time.Millisecond, logEvery: time.Hour, wait: time.Second,
		logf: func(string, ...any) {},
	}
}

func TestResolveAdvertiseOverride(t *testing.T) {
	want := netip.MustParseAddr("100.64.0.105")
	got, err := resolveAdvertise(context.Background(), NetworkTailnet, want, failingAdvertiseDeps(t))
	if err != nil || got != want {
		t.Errorf("override = %v, %v", got, err)
	}
}

func TestResolveAdvertiseLAN(t *testing.T) {
	d := failingAdvertiseDeps(t)
	d.readRoutes = func() ([]byte, error) { return routeFixture(t), nil }
	d.ifaceAddrs = addrsOf("eno1np0", "fe80::1", "192.168.42.144")
	got, err := resolveAdvertise(context.Background(), NetworkLAN, netip.Addr{}, d)
	if err != nil || got != netip.MustParseAddr("192.168.42.144") {
		t.Errorf("lan = %v, %v", got, err)
	}
}

func TestResolveAdvertiseTailnetWait(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	calls := 0
	d := failingAdvertiseDeps(t)
	d.tailnet = func(context.Context) (TailnetStatus, error) {
		calls++
		if calls <= 3 {
			return TailnetStatus{BackendState: "NeedsLogin"}, nil
		}
		return TailnetStatus{BackendState: "Running", Self: TailnetPeer{IPs: []netip.Addr{netip.MustParseAddr("100.64.0.105")}}}, nil
	}
	d.logf = func(format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	got, err := resolveAdvertise(context.Background(), NetworkTailnet, netip.Addr{}, d)
	if err != nil || got != netip.MustParseAddr("100.64.0.105") {
		t.Fatalf("tailnet = %v, %v", got, err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "tailscaled is NeedsLogin") {
		t.Errorf("wait lines = %q, want exactly one naming NeedsLogin", lines)
	}
}

func TestResolveAdvertiseTailnetTimeout(t *testing.T) {
	d := failingAdvertiseDeps(t)
	d.wait = 20 * time.Millisecond
	d.tailnet = func(context.Context) (TailnetStatus, error) {
		return TailnetStatus{}, errors.New("dial unix /var/run/tailscale/tailscaled.sock: connect: no such file or directory")
	}
	_, err := resolveAdvertise(context.Background(), NetworkTailnet, netip.Addr{}, d)
	if err == nil || !strings.Contains(err.Error(), "no tailnet IPv4 address after") || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("err = %v", err)
	}
}

func TestResolveAdvertiseCancelled(t *testing.T) {
	d := failingAdvertiseDeps(t)
	d.wait = time.Hour
	d.tailnet = func(context.Context) (TailnetStatus, error) { return TailnetStatus{BackendState: "Starting"}, nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := resolveAdvertise(ctx, NetworkTailnet, netip.Addr{}, d)
	if !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
		t.Errorf("err = %v after %v, want context.Canceled at once", err, time.Since(start))
	}
}
