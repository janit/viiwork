package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sort"
)

// TailnetPeer is one device from tailscaled's status.
type TailnetPeer struct {
	HostName string
	IPs      []netip.Addr
	Online   bool
}

// TailnetStatus is the part of tailscaled's status discovery needs.
type TailnetStatus struct {
	BackendState string
	Self         TailnetPeer
	Peers        []TailnetPeer // sorted by host name, then first IP
}

// SelfIPv4 is this machine's tailnet IPv4, available only once tailscaled is
// Running.
func (s TailnetStatus) SelfIPv4() (netip.Addr, bool) {
	if s.BackendState != "Running" {
		return netip.Addr{}, false
	}
	return firstTailnetIPv4(s.Self.IPs)
}

func firstTailnetIPv4(ips []netip.Addr) (netip.Addr, bool) {
	for _, ip := range ips {
		if ip.Is4() && tailnetRange.Contains(ip) {
			return ip, true
		}
	}
	return netip.Addr{}, false
}

// localAPIStatusURL is what Tailscale's own client requests. The host name is
// not resolved (the transport dials the socket); plain HTTP works on Linux.
const localAPIStatusURL = "http://local-tailscaled.sock/localapi/v0/status"

// maxStatusBody bounds the LocalAPI response; a large tailnet is well under it.
const maxStatusBody = 16 << 20

// ReadTailnetStatus asks tailscaled's LocalAPI over its unix socket. This keeps
// Tailscale's Go module out of the build. The request is bounded by ctx and
// its connection closed after use.
//
// ipnstate.Status and PeerStatus carry no json tags, so the keys are Go field
// names: BackendState, Self, Peer (a map keyed by node key), HostName,
// TailscaleIPs, Online.
func ReadTailnetStatus(ctx context.Context, socket string) (TailnetStatus, error) {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: true,
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, localAPIStatusURL, nil)
	if err != nil {
		return TailnetStatus{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return TailnetStatus{}, fmt.Errorf("tailscaled LocalAPI at %s: %w", socket, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxStatusBody))
	if err != nil {
		return TailnetStatus{}, fmt.Errorf("tailscaled LocalAPI: reading status: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return TailnetStatus{}, fmt.Errorf("tailscaled LocalAPI answered HTTP %d: %s", resp.StatusCode, truncate(body, 200))
	}

	type rawPeer struct {
		HostName     string
		TailscaleIPs []string
		Online       bool
	}
	var raw struct {
		BackendState string
		Self         rawPeer
		Peer         map[string]rawPeer
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return TailnetStatus{}, fmt.Errorf("tailscaled LocalAPI: decoding status: %w", err)
	}
	convert := func(p rawPeer) TailnetPeer {
		out := TailnetPeer{HostName: p.HostName, Online: p.Online}
		for _, s := range p.TailscaleIPs {
			if ip, err := netip.ParseAddr(s); err == nil {
				out.IPs = append(out.IPs, ip)
			}
		}
		return out
	}
	st := TailnetStatus{BackendState: raw.BackendState, Self: convert(raw.Self)}
	for _, p := range raw.Peer {
		st.Peers = append(st.Peers, convert(p))
	}
	sort.Slice(st.Peers, func(i, j int) bool {
		a, b := st.Peers[i], st.Peers[j]
		if a.HostName != b.HostName {
			return a.HostName < b.HostName
		}
		return firstIP(a).Less(firstIP(b))
	})
	return st, nil
}

func firstIP(p TailnetPeer) netip.Addr {
	if len(p.IPs) == 0 {
		return netip.Addr{}
	}
	return p.IPs[0]
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}

type tailnetFeeder struct {
	socket string
	port   int
}

// TailnetFeeder offers every online tailnet device's IPv4 on the gossip port.
// Offline devices are never offered, and devices not running viiwork refuse
// the join and are skipped by the rejoin round.
func TailnetFeeder(socket string, port int) Feeder { return tailnetFeeder{socket: socket, port: port} }

func (tailnetFeeder) Name() string { return "tailnet" }

func (f tailnetFeeder) Candidates(ctx context.Context) ([]string, error) {
	st, err := ReadTailnetStatus(ctx, f.socket)
	if err != nil {
		return nil, err
	}
	if st.BackendState != "Running" {
		return nil, fmt.Errorf("tailscaled is %s, not Running", st.BackendState)
	}
	var out []string
	for _, p := range st.Peers {
		if !p.Online {
			continue
		}
		if ip, ok := firstTailnetIPv4(p.IPs); ok {
			out = append(out, netip.AddrPortFrom(ip, uint16(f.port)).String())
		}
	}
	sort.Strings(out)
	return out, nil
}
