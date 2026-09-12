package mesh

import (
	"net/netip"
	"strings"
	"testing"
)

func TestCheckMemberAddr(t *testing.T) {
	cases := []struct {
		network string
		addr    string // "" = the zero netip.Addr
		want    string // "" = ok; otherwise a substring of the error
	}{
		{NetworkTailnet, "100.64.0.105", ""},
		{NetworkTailnet, "::ffff:100.64.0.105", ""},
		{NetworkTailnet, "100.63.255.255", "not on the tailnet range"},
		{NetworkTailnet, "100.128.0.0", "not on the tailnet range"},
		{NetworkTailnet, "192.168.42.144", "not on the tailnet range"},
		{NetworkTailnet, "8.8.8.8", "not on the tailnet range"},
		{NetworkTailnet, "127.0.0.1", "loopback"},
		{NetworkTailnet, "0.0.0.0", "unspecified"},
		{NetworkTailnet, "169.254.169.254", "link-local"},
		{NetworkTailnet, "64:ff9b::a9fe:a9fe", "link-local"},
		// The NAT64 and IPv4-mapped forms are unwrapped before judging, so an
		// address cannot be smuggled past the range check by encoding it in
		// one. Pinned for the general case, not only for the metadata address
		// above: the unwrap is what makes every other rule here reachable.
		{NetworkTailnet, "64:ff9b::8.8.8.8", "not on the tailnet range"},
		{NetworkTailnet, "64:ff9b::7f00:1", "loopback"},
		{NetworkTailnet, "::ffff:8.8.8.8", "not on the tailnet range"},
		{NetworkTailnet, "64:ff9b::100.64.0.2", ""},
		{NetworkTailnet, "255.255.255.255", "not on the tailnet range"},
		{NetworkTailnet, "fd7a:115c:a1e0::1", "not on the tailnet range"},
		{NetworkTailnet, "224.0.0.251", "multicast"},
		{NetworkLAN, "192.168.42.144", ""},
		{NetworkLAN, "10.1.2.3", ""},
		{NetworkLAN, "172.16.0.1", ""},
		{NetworkLAN, "fd7a:115c:a1e0::1", ""},
		{NetworkLAN, "172.32.0.1", "not on a private LAN range"},
		{NetworkLAN, "100.64.0.105", "not on a private LAN range"},
		{NetworkLAN, "fe80::1", "link-local"},
		{"wan", "100.64.0.105", "wan"},
		{NetworkTailnet, "", "invalid"},
	}
	for _, tc := range cases {
		var ip netip.Addr
		if tc.addr != "" {
			ip = netip.MustParseAddr(tc.addr)
		}
		err := CheckMemberAddr(tc.network, ip)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s %s: unexpected error %v", tc.network, tc.addr, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s %s: err = %v, want it to contain %q", tc.network, tc.addr, err, tc.want)
		}
	}
	if err := CheckMemberAddr(NetworkTailnet, netip.MustParseAddr("127.0.0.1")); err == nil || err.Error() != "member address 127.0.0.1: loopback is not a member" {
		t.Errorf("error text = %v", err)
	}
}
