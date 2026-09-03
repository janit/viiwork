package peer

// The gateway's validPeerAddr (viiwork-gateway/internal/mesh/registry.go) is
// the reference for these rules — it faced the same self-reported addresses
// and had to make them safe to dial. This is a reimplementation against that
// function's pinned test table rather than a verbatim copy; the two should
// stay in step, and a rule changed in one and not the other is a bug in one
// of them.
//
// The threat: a gossiped address is dialled before anything about it is
// proved, so whatever this function admits gets probed by every adopting
// node, every poll interval, forever. Each rule closes a concrete hole:
//
//   - IP literals only. A hostname is a level of indirection the advertiser
//     controls after adoption (DNS rebinding), and resolving it makes every
//     poll a lookup on attacker-chosen input.
//   - Strict host:port shape. Anything a URL parser might reinterpret —
//     path, query, fragment, userinfo, whitespace — is rejected before it
//     can smuggle a request elsewhere (the classic SSRF-via-parser-differential).
//   - Loopback, unspecified, link-local and multicast are never peers.
//     169.254.0.0/16 in particular is cloud metadata; 64:ff9b::/96 (NAT64)
//     is judged by the IPv4 it embeds so it cannot launder one of these.
//   - The Tailscale CGNAT range 100.64.0.0/10 is always allowed — it is
//     where real nodes live. RFC1918 and ULA space needs allow_private,
//     and public addresses are never dialled at all: this mesh has no
//     business crossing the open internet unauthenticated.

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
)

var (
	tailscaleCGNAT = netip.MustParsePrefix("100.64.0.0/10")

	// nat64 embeds an IPv4 address in its low 32 bits (RFC 6052). Validate
	// the embedded address, not the prefix, or 64:ff9b::a9fe:a9fe walks the
	// link-local rejection straight past an IPv6-only check.
	nat64 = netip.MustParsePrefix("64:ff9b::/96")

	privatePeerPrefixes = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("fc00::/7"),
	}
)

// validPeerAddr reports whether addr is safe to adopt as a peer address and
// dial unattended. It admits "ip:port" with an IP literal and a strict
// decimal port, on the Tailscale CGNAT range always and RFC1918/ULA space
// only when allowPrivate is set. Everything else is an error, and the error
// says which rule refused it.
func validPeerAddr(addr string, allowPrivate bool) error {
	if addr == "" {
		return fmt.Errorf("empty peer address")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("peer address %q is not host:port: %w", addr, err)
	}
	if err := strictDecimalPort(port); err != nil {
		return fmt.Errorf("peer address %q: %w", addr, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("peer address %q: host is not an IP literal", addr)
	}
	// ::ffff:a.b.c.d is IPv4 wearing IPv6 syntax; judge the IPv4.
	ip = ip.Unmap()
	if nat64.Contains(ip) {
		a16 := ip.As16()
		ip = netip.AddrFrom4([4]byte{a16[12], a16[13], a16[14], a16[15]})
	}

	switch {
	case ip.IsLoopback():
		return fmt.Errorf("peer address %q: loopback is not a peer", addr)
	case ip.IsUnspecified():
		return fmt.Errorf("peer address %q: unspecified address", addr)
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
		return fmt.Errorf("peer address %q: link-local is not a peer", addr)
	case ip.IsMulticast():
		return fmt.Errorf("peer address %q: multicast is not a peer", addr)
	}

	if tailscaleCGNAT.Contains(ip) {
		return nil
	}
	for _, pfx := range privatePeerPrefixes {
		if pfx.Contains(ip) {
			if allowPrivate {
				return nil
			}
			return fmt.Errorf("peer address %q: private range needs allow_private", addr)
		}
	}
	return fmt.Errorf("peer address %q: not on an allowed range", addr)
}

// strictDecimalPort admits exactly the strings strconv would print for
// 1..65535. Leading zeros, signs and any non-digit are rejected before
// ParseUint can be lenient about them.
func strictDecimalPort(port string) error {
	if port == "" || len(port) > 5 {
		return fmt.Errorf("port %q out of range", port)
	}
	if port[0] == '0' {
		return fmt.Errorf("port %q has a leading zero", port)
	}
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return fmt.Errorf("port %q is not a decimal number", port)
		}
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return fmt.Errorf("port %q out of range", port)
	}
	return nil
}
