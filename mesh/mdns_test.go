package mesh

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"testing"

	"github.com/hashicorp/mdns"
)

func TestMDNSServiceRecord(t *testing.T) {
	rec, err := mdnsServiceRecord("gb1", netip.MustParseAddr("192.168.42.144"), 7946)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Instance != "gb1" || rec.Service != "_viiwork._tcp" || rec.HostName != "gb1." || rec.Port != 7946 {
		t.Errorf("record = %+v", rec)
	}
	if len(rec.IPs) != 1 || !rec.IPs[0].Equal(net.ParseIP("192.168.42.144")) || !slices.Equal(rec.TXT, []string{"node=gb1"}) {
		t.Errorf("IPs=%v TXT=%v", rec.IPs, rec.TXT)
	}
}

func entry(ip string, port int, txt ...string) *mdns.ServiceEntry {
	e := &mdns.ServiceEntry{Port: port, InfoFields: txt}
	if ip != "" {
		e.AddrV4 = net.ParseIP(ip)
	}
	return e
}

func TestEntriesToCandidates(t *testing.T) {
	self := netip.MustParseAddr("192.168.42.144")
	entries := []*mdns.ServiceEntry{
		entry("192.168.42.10", 7946, "node=gb0"),
		entry("192.168.42.10", 7946, "node=gb0"),
		entry("192.168.42.11", 7946),
		{Port: 7946, InfoFields: []string{"node=v6"}, AddrV6IPAddr: &net.IPAddr{IP: net.ParseIP("fd00::1")}},
		entry("192.168.42.144", 7946, "node=gb1"),
		entry("8.8.8.8", 7946, "node=public"),
		entry("192.168.42.12", 0, "node=noport"),
	}
	if got := entriesToCandidates(entries, self); !slices.Equal(got, []string{"192.168.42.10:7946"}) {
		t.Errorf("candidates = %v", got)
	}
}

func TestMDNSFeeder(t *testing.T) {
	self := netip.MustParseAddr("192.168.42.144")
	f := newMDNSFeeder(self, nil, func(context.Context, *net.Interface) ([]*mdns.ServiceEntry, error) {
		return []*mdns.ServiceEntry{entry("192.168.42.20", 7946, "node=b"), entry("192.168.42.10", 7946, "node=a")}, nil
	})
	if f.Name() != "mdns" {
		t.Errorf("Name = %q", f.Name())
	}
	got, err := f.Candidates(context.Background())
	if err != nil || !slices.Equal(got, []string{"192.168.42.10:7946", "192.168.42.20:7946"}) {
		t.Errorf("candidates = %v, %v", got, err)
	}

	boom := errors.New("no multicast")
	failing := newMDNSFeeder(self, nil, func(context.Context, *net.Interface) ([]*mdns.ServiceEntry, error) { return nil, boom })
	if _, err := failing.Candidates(context.Background()); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the browse error", err)
	}
}

func TestInterfaceWithAddr(t *testing.T) {
	iface, err := interfaceWithAddr(netip.MustParseAddr("127.0.0.1"))
	if err != nil || iface.Flags&net.FlagLoopback == 0 {
		t.Errorf("127.0.0.1: iface=%v err=%v", iface, err)
	}
	if _, err := interfaceWithAddr(netip.MustParseAddr("203.0.113.9")); err == nil {
		t.Error("an address no interface holds must be an error")
	}
}
