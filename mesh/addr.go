package mesh

// Member address rules, ported from v1's peer address checks.
//
// The threat is unchanged from v1: an address a member reports about itself is
// dialled (capacity polls, forwards, status polls) before anything about it is
// proved, so whatever these rules admit gets probed by every node, every poll
// interval, for as long as the member stays alive. Each rule closes a hole:
//
//   - Loopback, unspecified, link-local and multicast are never members.
//     169.254.0.0/16 in particular is cloud metadata. An address in the NAT64
//     prefix 64:ff9b::/96 is judged by the IPv4 it embeds, so it cannot
//     launder one of these, and ::ffff:a.b.c.d is judged as the IPv4 it is.
//   - A mesh is single-network (spec decision 5), so the allowed range follows
//     it: the Tailscale CGNAT range 100.64.0.0/10 on a tailnet mesh, RFC1918
//     and ULA space on a LAN mesh. Public addresses are never members.
//
// What changed from v1: memberlist hands over IP and port separately, so the
// host:port shape and strict-port rules have nothing left to check; and the
// private ranges are no longer an opt-in beside the tailnet, they are the
// whole of a LAN mesh and none of a tailnet one.

import (
	"fmt"
	"net/netip"
)

// Mesh networks (C1 mesh.network).
const (
	NetworkTailnet = "tailnet"
	NetworkLAN     = "lan"
)

var (
	tailnetRange = netip.MustParsePrefix("100.64.0.0/10")
	nat64        = netip.MustParsePrefix("64:ff9b::/96")
	lanRanges    = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("fc00::/7"),
	}
)

// CheckMemberAddr reports whether ip may be a member of a mesh on network. The
// error names the rule that refused it.
func CheckMemberAddr(network string, ip netip.Addr) error {
	if !ip.IsValid() {
		return fmt.Errorf("member address: invalid (zero) address")
	}
	if network != NetworkTailnet && network != NetworkLAN {
		return fmt.Errorf("member address %s: unknown mesh network %q", ip, network)
	}
	judged := ip.Unmap()
	if nat64.Contains(judged) {
		a := judged.As16()
		judged = netip.AddrFrom4([4]byte{a[12], a[13], a[14], a[15]})
	}

	switch {
	case judged.IsLoopback():
		return fmt.Errorf("member address %s: loopback is not a member", ip)
	case judged.IsUnspecified():
		return fmt.Errorf("member address %s: unspecified address is not a member", ip)
	case judged.IsLinkLocalUnicast():
		return fmt.Errorf("member address %s: link-local is not a member", ip)
	case judged.IsMulticast(): // includes link-local multicast
		return fmt.Errorf("member address %s: multicast is not a member", ip)
	}

	if network == NetworkTailnet {
		if tailnetRange.Contains(judged) {
			return nil
		}
		return fmt.Errorf("member address %s: not on the tailnet range %s", ip, tailnetRange)
	}
	for _, p := range lanRanges {
		if p.Contains(judged) {
			return nil
		}
	}
	return fmt.Errorf("member address %s: not on a private LAN range", ip)
}
