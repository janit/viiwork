package mesh

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultRouteIPv4 is the address a LAN-mode node advertises: the first
// usable IPv4 of the interface that holds the default route, read from
// /proc/net/route. Among several default routes (a wired and a wireless
// link, say) the lowest metric wins, as it does for the kernel.
func DefaultRouteIPv4(routeTable []byte, ifaceAddrs func(name string) ([]netip.Addr, error)) (netip.Addr, error) {
	iface, found := "", false
	best := int64(-1)
	sc := bufio.NewScanner(bytes.NewReader(routeTable))
	for header := true; sc.Scan(); header = false {
		if header {
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 8 {
			continue
		}
		// Iface Destination Gateway Flags RefCnt Use Metric Mask ...
		if f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(f[3], 16, 32)
		if err != nil || flags&0x1 == 0 { // RTF_UP
			continue
		}
		metric, err := strconv.ParseInt(f[6], 10, 64)
		if err != nil {
			continue
		}
		if !found || metric < best {
			iface, best, found = f[0], metric, true
		}
	}
	if !found {
		return netip.Addr{}, errors.New("no IPv4 default route in /proc/net/route")
	}
	addrs, err := ifaceAddrs(iface)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("default route interface %s: %w", iface, err)
	}
	for _, a := range addrs {
		a = a.Unmap()
		if a.Is4() && !a.IsLoopback() && !a.IsLinkLocalUnicast() {
			return a, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("default route interface %s has no usable IPv4 address", iface)
}

// advertiseDeps are resolveAdvertise's inputs, injectable for tests.
type advertiseDeps struct {
	readRoutes func() ([]byte, error)
	ifaceAddrs func(name string) ([]netip.Addr, error)
	tailnet    func(ctx context.Context) (TailnetStatus, error)
	poll       time.Duration
	logEvery   time.Duration
	wait       time.Duration
	logf       func(format string, args ...any)
}

// defaultAdvertiseDeps are the production inputs: the kernel route table, the
// host's interfaces, the LocalAPI on socket, and the tailnet wait of Decision 9
// (poll every 2 s, log at most every 10 s, give up after 5 minutes).
func defaultAdvertiseDeps(socket string, logf func(format string, args ...any)) advertiseDeps {
	return advertiseDeps{
		readRoutes: func() ([]byte, error) { return os.ReadFile("/proc/net/route") },
		ifaceAddrs: interfaceAddrs,
		tailnet: func(ctx context.Context) (TailnetStatus, error) {
			return ReadTailnetStatus(ctx, socket)
		},
		poll:     2 * time.Second,
		logEvery: 10 * time.Second,
		wait:     5 * time.Minute,
		logf:     logf,
	}
}

func interfaceAddrs(name string) ([]netip.Addr, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(ipnet.IP); ok {
				out = append(out, ip)
			}
		}
	}
	return out, nil
}

// resolveAdvertise picks the address gossip binds to and advertises. An
// override (mesh.advertise) wins without consulting anything. A LAN mesh uses
// the default route's interface. A tailnet mesh waits for tailscaled to report
// this machine's tailnet IPv4: binding a Tailscale IP races tailscaled at
// boot, and binding nothing would leave a node that never joins.
func resolveAdvertise(ctx context.Context, network string, override netip.Addr, d advertiseDeps) (netip.Addr, error) {
	if override.IsValid() {
		return override, nil
	}
	switch network {
	case NetworkLAN:
		routes, err := d.readRoutes()
		if err != nil {
			return netip.Addr{}, fmt.Errorf("reading the route table: %w", err)
		}
		return DefaultRouteIPv4(routes, d.ifaceAddrs)
	case NetworkTailnet:
		return waitTailnetIPv4(ctx, d)
	}
	return netip.Addr{}, fmt.Errorf("unknown mesh network %q", network)
}

func waitTailnetIPv4(ctx context.Context, d advertiseDeps) (netip.Addr, error) {
	deadline := time.Now().Add(d.wait)
	var lastLog time.Time
	var reason string
	for {
		if err := ctx.Err(); err != nil {
			return netip.Addr{}, err
		}
		st, err := d.tailnet(ctx)
		switch {
		case err != nil:
			reason = err.Error()
		default:
			if ip, ok := st.SelfIPv4(); ok {
				return ip, nil
			}
			reason = "tailscaled is " + st.BackendState
			if st.BackendState == "Running" {
				reason += " without a tailnet IPv4 address"
			}
		}
		if !time.Now().Before(deadline) {
			return netip.Addr{}, fmt.Errorf("no tailnet IPv4 address after %v: %s", d.wait, reason)
		}
		if lastLog.IsZero() || time.Since(lastLog) >= d.logEvery {
			d.logf("waiting for the tailnet IPv4 address (%s)", reason)
			lastLog = time.Now()
		}
		timer := time.NewTimer(d.poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return netip.Addr{}, ctx.Err()
		case <-timer.C:
		}
	}
}
