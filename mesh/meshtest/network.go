// Package meshtest is an in-process network for memberlist that can take a
// node away. memberlist's own MockNetwork cannot: its Shutdown does nothing
// and a send to a node nobody reads blocks the sender forever, so "member
// killed", "power on" and "split and heal" cannot be expressed with it (P3
// Decision 1).
//
// Addresses look like the tailnet (100.64.0.<n>:7946), so mesh's address
// rules accept them without a test override. Delivery is by address, as on a
// real network: a packet sent to a node's old address never reaches the node
// after it has moved.
package meshtest

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"
)

const (
	packetBuffer = 1024
	streamBuffer = 64
	gossipPort   = 7946
)

// Network is a set of transports that can reach each other.
type Network struct {
	mu        sync.Mutex
	next      int                           // last host number handed out
	addrs     map[string]netip.AddrPort     // name -> address
	readdress map[string]bool               // next Transport(name) takes a fresh address
	byAddr    map[netip.AddrPort]*Transport // current transport at each address
	byName    map[string]*Transport         // current transport of each name
	unplugged map[string]bool
	group     map[string]int // partition group per name; absent = reachable from all
	pipes     map[*pipe]struct{}
}

func NewNetwork() *Network {
	return &Network{
		addrs:     map[string]netip.AddrPort{},
		readdress: map[string]bool{},
		byAddr:    map[netip.AddrPort]*Transport{},
		byName:    map[string]*Transport{},
		unplugged: map[string]bool{},
		group:     map[string]int{},
		pipes:     map[*pipe]struct{}{},
	}
}

func (n *Network) freshAddrLocked() netip.AddrPort {
	n.next++
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{100, 64, byte(n.next >> 8), byte(n.next)}), gossipPort)
}

// Transport returns a new transport for name at the name's address, assigned
// on first use. A previous transport for the name is replaced and becomes
// unreachable, which is a machine powering back on at its fixed address.
func (n *Network) Transport(name string) *Transport {
	n.mu.Lock()
	defer n.mu.Unlock()
	addr, ok := n.addrs[name]
	if !ok || n.readdress[name] {
		addr = n.freshAddrLocked()
		n.addrs[name] = addr
		delete(n.readdress, name)
	}
	return n.installLocked(name, addr)
}

// TransportAt returns a new transport for name pinned to addr, which may be
// outside the tailnet range (a rogue member, say).
func (n *Network) TransportAt(name string, addr netip.AddrPort) *Transport {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.addrs[name] = addr
	return n.installLocked(name, addr)
}

func (n *Network) installLocked(name string, addr netip.AddrPort) *Transport {
	if old := n.byName[name]; old != nil {
		old.gone = true
		delete(n.byAddr, old.addr)
		n.closePipesLocked(func(p *pipe) bool { return p.a == old || p.b == old })
	}
	t := &Transport{
		net:      n,
		name:     name,
		addr:     addr,
		packetCh: make(chan *memberlist.Packet, packetBuffer),
		streamCh: make(chan net.Conn, streamBuffer),
	}
	n.byName[name] = t
	n.byAddr[addr] = t
	return t
}

// Readdress makes the next Transport(name) take a fresh address, like a DHCP
// lease that changed across a power cycle.
func (n *Network) Readdress(name string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.readdress[name] = true
}

// Addr is name's address, assigned on first use.
func (n *Network) Addr(name string) netip.AddrPort {
	n.mu.Lock()
	defer n.mu.Unlock()
	addr, ok := n.addrs[name]
	if !ok {
		addr = n.freshAddrLocked()
		n.addrs[name] = addr
	}
	return addr
}

// Unplug takes name off the network: nothing reaches it and nothing it sends
// arrives. Open streams to and from it are closed.
func (n *Network) Unplug(name string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.unplugged[name] = true
	n.closePipesLocked(func(p *pipe) bool { return p.a.name == name || p.b.name == name })
}

// Plug undoes Unplug.
func (n *Network) Plug(name string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.unplugged, name)
}

// Partition splits the named nodes into groups that cannot reach each other.
// A node in no group still reaches everyone.
func (n *Network) Partition(groups ...[]string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.group = map[string]int{}
	for i, g := range groups {
		for _, name := range g {
			n.group[name] = i
		}
	}
	n.closePipesLocked(func(p *pipe) bool { return !n.reachableLocked(p.a, p.b) })
}

// Heal removes every partition.
func (n *Network) Heal() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.group = map[string]int{}
}

func (n *Network) reachableLocked(from, to *Transport) bool {
	if from == nil || to == nil || from.gone || to.gone || from.shutdown || to.shutdown {
		return false
	}
	if n.unplugged[from.name] || n.unplugged[to.name] {
		return false
	}
	gf, okf := n.group[from.name]
	gt, okt := n.group[to.name]
	return !okf || !okt || gf == gt
}

func (n *Network) closePipesLocked(cut func(*pipe) bool) {
	for p := range n.pipes {
		if cut(p) {
			p.close()
			delete(n.pipes, p)
		}
	}
}

func (n *Network) resolveLocked(addr string) *Transport {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return nil
	}
	return n.byAddr[ap]
}

// Transport is one node's memberlist transport.
type Transport struct {
	net      *Network
	name     string
	addr     netip.AddrPort
	packetCh chan *memberlist.Packet
	streamCh chan net.Conn
	gone     bool // replaced by a newer transport for the same name
	shutdown bool
}

var (
	_ memberlist.NodeAwareTransport      = (*Transport)(nil)
	_ memberlist.IngestionAwareTransport = (*Transport)(nil) //nolint:staticcheck // the plan pins both interfaces
)

func (t *Transport) udpAddr() *net.UDPAddr {
	return &net.UDPAddr{IP: t.addr.Addr().AsSlice(), Port: int(t.addr.Port())}
}

func (t *Transport) tcpAddr() *net.TCPAddr {
	return &net.TCPAddr{IP: t.addr.Addr().AsSlice(), Port: int(t.addr.Port())}
}

// FinalAdvertiseAddr is the transport's assigned address, whatever was asked.
func (t *Transport) FinalAdvertiseAddr(string, int) (net.IP, int, error) {
	return t.addr.Addr().AsSlice(), int(t.addr.Port()), nil
}

// WriteTo delivers without ever blocking. A packet to an unreachable node is
// dropped silently, like UDP to a powered-off host, and so is one to a node
// whose buffer is full.
func (t *Transport) WriteTo(b []byte, addr string) (time.Time, error) {
	now := time.Now()
	n := t.net
	n.mu.Lock()
	dest := n.resolveLocked(addr)
	ok := n.reachableLocked(t, dest)
	n.mu.Unlock()
	if !ok {
		return now, nil
	}
	buf := append([]byte(nil), b...)
	select {
	case dest.packetCh <- &memberlist.Packet{Buf: buf, From: t.udpAddr(), Timestamp: now}:
	default:
	}
	return now, nil
}

func (t *Transport) WriteToAddress(b []byte, addr memberlist.Address) (time.Time, error) {
	return t.WriteTo(b, addr.Addr)
}

func (t *Transport) PacketCh() <-chan *memberlist.Packet { return t.packetCh }

// DialTimeout opens an in-memory stream. To an unreachable node it waits
// min(timeout, 100 ms) and fails, so a test never sits through a real TCP
// timeout.
func (t *Transport) DialTimeout(addr string, timeout time.Duration) (net.Conn, error) {
	n := t.net
	n.mu.Lock()
	dest := n.resolveLocked(addr)
	if !n.reachableLocked(t, dest) {
		n.mu.Unlock()
		time.Sleep(min(timeout, 100*time.Millisecond))
		return nil, fmt.Errorf("meshtest: %s unreachable", addr)
	}
	local, remote := net.Pipe()
	p := &pipe{a: t, b: dest, conns: []net.Conn{local, remote}}
	n.pipes[p] = struct{}{}
	n.mu.Unlock()

	select {
	case dest.streamCh <- &addrConn{Conn: remote, local: dest.tcpAddr(), remote: t.tcpAddr()}:
	default:
		n.mu.Lock()
		p.close()
		delete(n.pipes, p)
		n.mu.Unlock()
		return nil, fmt.Errorf("meshtest: %s stream backlog full", addr)
	}
	return &addrConn{Conn: local, local: t.tcpAddr(), remote: dest.tcpAddr()}, nil
}

func (t *Transport) DialAddressTimeout(addr memberlist.Address, timeout time.Duration) (net.Conn, error) {
	return t.DialTimeout(addr.Addr, timeout)
}

func (t *Transport) StreamCh() <-chan net.Conn { return t.streamCh }

// IngestPacket copies the connection into a packet, as memberlist's mock does.
func (t *Transport) IngestPacket(conn net.Conn, addr net.Addr, now time.Time, shouldClose bool) error {
	if shouldClose {
		defer conn.Close()
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, conn); err != nil {
		return fmt.Errorf("meshtest: reading packet: %w", err)
	}
	if buf.Len() < 1 {
		return fmt.Errorf("meshtest: packet too short")
	}
	select {
	case t.packetCh <- &memberlist.Packet{Buf: buf.Bytes(), From: addr, Timestamp: now}:
	default:
	}
	return nil
}

// IngestStream delivers the connection, as memberlist's mock does.
func (t *Transport) IngestStream(conn net.Conn) error {
	select {
	case t.streamCh <- conn:
		return nil
	default:
		return fmt.Errorf("meshtest: stream backlog full")
	}
}

// Shutdown makes the transport unreachable and closes its streams. It never
// closes its channels: memberlist stops its readers through its own shutdown.
func (t *Transport) Shutdown() error {
	n := t.net
	n.mu.Lock()
	defer n.mu.Unlock()
	t.shutdown = true
	n.closePipesLocked(func(p *pipe) bool { return p.a == t || p.b == t })
	return nil
}

// pipe is one open stream between two transports.
type pipe struct {
	a, b  *Transport
	conns []net.Conn
}

func (p *pipe) close() {
	for _, c := range p.conns {
		_ = c.Close()
	}
}

// addrConn gives a net.Pipe end real-looking addresses.
type addrConn struct {
	net.Conn
	local, remote net.Addr
}

func (c *addrConn) LocalAddr() net.Addr  { return c.local }
func (c *addrConn) RemoteAddr() net.Addr { return c.remote }
