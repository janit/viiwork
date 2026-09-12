package mesh

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/mdns"
)

// MDNSService is the service a LAN-mode node advertises and browses.
const MDNSService = "_viiwork._tcp"

// mdnsBrowser returns the answers to one browse; a seam for tests, since real
// multicast does not travel reliably through containers (Decision 13).
type mdnsBrowser func(ctx context.Context, iface *net.Interface) ([]*mdns.ServiceEntry, error)

// mdnsServiceRecord is the node's advertisement: instance and host name are the
// node name, the port is gossip's, and the TXT record node=<name> marks it as
// a viiwork node. Host name and IPs are given explicitly so the library does
// not try to resolve the host itself.
func mdnsServiceRecord(name string, ip netip.Addr, port int) (*mdns.MDNSService, error) {
	return mdns.NewMDNSService(name, MDNSService, "", name+".", port, []net.IP{ip.AsSlice()}, []string{"node=" + name})
}

// advertiseMDNS answers mDNS queries for the node on the interface holding its
// advertise address, until the returned shutdown is called.
func advertiseMDNS(name string, ip netip.Addr, port int, logger *log.Logger) (shutdown func() error, err error) {
	rec, err := mdnsServiceRecord(name, ip, port)
	if err != nil {
		return nil, fmt.Errorf("mdns: service record: %w", err)
	}
	iface, err := interfaceWithAddr(ip)
	if err != nil {
		return nil, fmt.Errorf("mdns: %w", err)
	}
	srv, err := mdns.NewServer(&mdns.Config{Zone: rec, Iface: iface, Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("mdns: advertising on %s: %w", iface.Name, err)
	}
	return srv.Shutdown, nil
}

// MDNSFeeder browses the LAN for other viiwork nodes on the interface holding
// self.
func MDNSFeeder(self netip.Addr, logger *log.Logger) (Feeder, error) {
	iface, err := interfaceWithAddr(self)
	if err != nil {
		return nil, fmt.Errorf("mdns: %w", err)
	}
	return newMDNSFeeder(self, iface, browseMDNS(logger)), nil
}

func browseMDNS(logger *log.Logger) mdnsBrowser {
	return func(ctx context.Context, iface *net.Interface) ([]*mdns.ServiceEntry, error) {
		ch := make(chan *mdns.ServiceEntry, 64)
		done := make(chan []*mdns.ServiceEntry)
		go func() {
			var entries []*mdns.ServiceEntry
			for e := range ch {
				entries = append(entries, e)
			}
			done <- entries
		}()
		err := mdns.QueryContext(ctx, &mdns.QueryParam{
			Service:     MDNSService,
			Domain:      "local",
			Timeout:     2 * time.Second,
			Interface:   iface,
			Entries:     ch,
			DisableIPv6: true,
			Logger:      logger,
		})
		close(ch)
		return <-done, err
	}
}

type mdnsFeeder struct {
	self   netip.Addr
	iface  *net.Interface
	browse mdnsBrowser
}

func newMDNSFeeder(self netip.Addr, iface *net.Interface, browse mdnsBrowser) Feeder {
	return mdnsFeeder{self: self, iface: iface, browse: browse}
}

func (mdnsFeeder) Name() string { return "mdns" }

func (f mdnsFeeder) Candidates(ctx context.Context) ([]string, error) {
	entries, err := f.browse(ctx, f.iface)
	if err != nil {
		return nil, err
	}
	return entriesToCandidates(entries, f.self), nil
}

// entriesToCandidates keeps answers that look like viiwork nodes (a node= TXT
// field, an IPv4, a port) on a private LAN range, other than this node, as
// sorted and de-duplicated ip:port strings. An mDNS answer comes from anyone on
// the link, so it must never point the dialler at a public or foreign address.
func entriesToCandidates(entries []*mdns.ServiceEntry, self netip.Addr) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		if e == nil || e.AddrV4 == nil || e.Port <= 0 || !hasNodeTXT(e.InfoFields) {
			continue
		}
		ip, ok := netip.AddrFromSlice(e.AddrV4.To4())
		if !ok || ip == self || CheckMemberAddr(NetworkLAN, ip) != nil {
			continue
		}
		c := netip.AddrPortFrom(ip, uint16(e.Port)).String()
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

func hasNodeTXT(fields []string) bool {
	for _, f := range fields {
		if strings.HasPrefix(f, "node=") {
			return true
		}
	}
	return false
}

// interfaceWithAddr finds the interface holding ip.
func interfaceWithAddr(ip netip.Addr) (*net.Interface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for i := range ifaces {
		addrs, err := ifaces[i].Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if have, ok := netip.AddrFromSlice(ipnet.IP); ok && have.Unmap() == ip.Unmap() {
				return &ifaces[i], nil
			}
		}
	}
	return nil, fmt.Errorf("no network interface holds %s", ip)
}
