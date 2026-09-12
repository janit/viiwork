package mesh

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/janit/viiwork/v2/meshapi"
)

// DefaultTailnetSocket is where tailscaled's LocalAPI listens on Linux. A
// tailnet mesh needs a socket to learn its own address even when tailnet
// discovery is off; this one is used when Options names none.
const DefaultTailnetSocket = "/var/run/tailscale/tailscaled.sock"

const (
	joinConcurrency = 8
	feederTimeout   = 10 * time.Second
)

// Mesh is a running membership: this node's memberlist, its discovery
// feeders and the member table.
type Mesh struct {
	o         Options
	mode      string
	advertise netip.Addr
	ml        *memberlist.Memberlist
	d         *delegate
	table     *memberTable
	filter    *logFilter
	feeders   []Feeder
	mdnsStop  func() error

	rejoinMu     sync.Mutex // rounds never overlap
	stopLoop     context.CancelFunc
	loopDone     chan struct{}
	stopDispatch chan struct{}
	dispatchDone chan struct{}

	logMu       sync.Mutex
	feederErr   map[string]string // feeder -> last error message logged
	mismatchLog map[string]bool   // candidates whose mismatch was logged

	leaveOnce    sync.Once
	shutdownOnce sync.Once
}

// Start joins the mesh: validate, encode the metadata, resolve the advertise
// address, create memberlist, build the feeders, run one discovery round
// synchronously, then keep rejoining every RejoinInterval. Any failure after
// memberlist is created shuts down what was started.
func Start(ctx context.Context, o Options) (*Mesh, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	meta, err := EncodeMeta(NodeMeta{V: MetaVersion, API: o.APIPort, Ver: o.Version, Role: o.Role})
	if err != nil {
		return nil, fmt.Errorf("mesh: %w", err)
	}
	out := o.Log
	if out == nil {
		out = os.Stdout
	}
	filter := newLogFilter(out, o.Debug, time.Now)

	advertise, err := resolveAdvertise(ctx, o.Network, o.Advertise, defaultAdvertiseDeps(o.advertiseSocket(), filter.logf))
	if err != nil {
		return nil, fmt.Errorf("mesh: advertise address: %w", err)
	}
	if o.SecretKey == nil {
		check := o.AddrCheck
		if check == nil {
			check = func(a netip.Addr) error { return CheckMemberAddr(o.Network, a) }
		}
		if err := check(advertise); err != nil {
			return nil, fmt.Errorf("mesh: advertise address %s is outside the %s range: %w", advertise, o.Network, err)
		}
		if o.Network == NetworkLAN {
			filter.logf("open mesh on a LAN: any machine on this LAN running viiwork can join")
		}
	}

	m := &Mesh{
		o:            o,
		mode:         o.mode(),
		advertise:    advertise,
		filter:       filter,
		stopDispatch: make(chan struct{}),
		dispatchDone: make(chan struct{}),
		loopDone:     make(chan struct{}),
		feederErr:    map[string]string{},
		mismatchLog:  map[string]bool{},
	}
	decoded, _ := DecodeMeta(meta)
	m.table = newMemberTable(Member{Name: o.Name, Addr: advertise, Port: o.BindPort, Meta: decoded}, time.Now)
	queue := &memberlist.TransmitLimitedQueue{NumNodes: m.numMembers}
	m.d = newDelegate(o, meta, m.table, queue, filter.logf, time.Now(), time.Now)

	cfg, err := buildMemberlistConfig(o, advertise, m.d, log.New(filter, "", 0))
	if err != nil {
		return nil, err
	}
	queue.RetransmitMult = cfg.RetransmitMult
	go m.dispatch()
	ml, err := memberlist.Create(cfg)
	if err != nil {
		m.stopDispatcher()
		return nil, fmt.Errorf("mesh: starting memberlist on %s:%d: %w", advertise, o.BindPort, err)
	}
	m.ml = ml

	if err := m.buildFeeders(log.New(filter, "", 0)); err != nil {
		_ = ml.Shutdown()
		m.stopDispatcher()
		return nil, err
	}

	names := make([]string, 0, len(m.feeders))
	for _, f := range m.feeders {
		names = append(names, f.Name())
	}
	discovery := "none"
	if len(names) > 0 {
		discovery = strings.Join(names, ", ")
	}
	filter.logf("%s %s on %s, gossip %s:%d, discovery: %s", o.Name, m.mode, o.Network, advertise, o.BindPort, discovery)

	m.Rejoin(ctx)

	loopCtx, cancel := context.WithCancel(context.Background())
	m.stopLoop = cancel
	go m.rejoinLoop(loopCtx)
	return m, nil
}

func (m *Mesh) numMembers() int {
	if m.ml == nil {
		return 1
	}
	return m.ml.NumMembers()
}

func (m *Mesh) buildFeeders(logger *log.Logger) error {
	if m.o.Feeders != nil {
		m.feeders = m.o.Feeders
		return nil
	}
	if m.o.TailnetSocket != "" {
		m.feeders = append(m.feeders, TailnetFeeder(m.o.TailnetSocket, m.o.BindPort))
	}
	if m.o.MDNS {
		stop, err := advertiseMDNS(m.o.Name, m.advertise, m.o.BindPort, logger)
		if err != nil {
			return fmt.Errorf("mesh: %w", err)
		}
		m.mdnsStop = stop
		f, err := MDNSFeeder(m.advertise, logger)
		if err != nil {
			_ = stop()
			return fmt.Errorf("mesh: %w", err)
		}
		m.feeders = append(m.feeders, f)
	}
	if len(m.o.Seeds) > 0 {
		m.feeders = append(m.feeders, SeedFeeder(m.o.Seeds))
	}
	return nil
}

// dispatch calls OnChange for every member event, in order, from one
// goroutine. On shutdown it delivers what is already queued, then stops.
func (m *Mesh) dispatch() {
	defer close(m.dispatchDone)
	deliver := func(ev MemberEvent) {
		if m.o.OnChange != nil {
			m.o.OnChange(ev)
		}
	}
	for {
		select {
		case ev := <-m.d.events():
			deliver(ev)
		case <-m.stopDispatch:
			for {
				select {
				case ev := <-m.d.events():
					deliver(ev)
				default:
					return
				}
			}
		}
	}
}

func (m *Mesh) stopDispatcher() {
	close(m.stopDispatch)
	<-m.dispatchDone
}

func (m *Mesh) rejoinLoop(ctx context.Context) {
	defer close(m.loopDone)
	t := time.NewTicker(m.o.RejoinInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Rejoin(ctx)
		}
	}
}

// Rejoin runs one discovery round and returns how many candidates it joined.
// Candidates already alive, and this node itself, are skipped. Joins run
// concurrently, at most 8 at a time and one address per Join, because Join
// dials addresses one after another and a device that drops packets costs a
// full TCP timeout (Decision 7). Rounds never overlap.
func (m *Mesh) Rejoin(ctx context.Context) int {
	m.rejoinMu.Lock()
	defer m.rejoinMu.Unlock()

	var lists [][]string
	for _, f := range m.feeders {
		fctx, cancel := context.WithTimeout(ctx, feederTimeout)
		cands, err := f.Candidates(fctx)
		cancel()
		m.noteFeeder(f.Name(), err)
		if err == nil {
			lists = append(lists, cands)
		}
	}
	exclude := m.table.aliveAddrs()
	exclude[netip.AddrPortFrom(m.advertise, uint16(m.o.BindPort)).String()] = true
	cands := mergeCandidates(lists, exclude)
	if len(cands) == 0 {
		return 0
	}

	var joined atomic.Int64
	sem := make(chan struct{}, joinConcurrency)
	var wg sync.WaitGroup
	for _, addr := range cands {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			if _, err := m.ml.Join([]string{addr}); err != nil {
				m.noteJoinFailure(addr, err)
				return
			}
			joined.Add(1)
			m.logMu.Lock()
			delete(m.mismatchLog, addr)
			m.logMu.Unlock()
		}()
	}
	wg.Wait()
	if k := joined.Load(); k > 0 {
		m.filter.logf("rejoin reached %d of %d candidates, %d members alive", k, len(cands), m.NumAlive())
	}
	return int(joined.Load())
}

func (m *Mesh) noteFeeder(name string, err error) {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	if err == nil {
		delete(m.feederErr, name)
		return
	}
	if m.feederErr[name] != err.Error() {
		m.feederErr[name] = err.Error()
		m.filter.logf("discovery %s: %v", name, err)
	}
}

func (m *Mesh) noteJoinFailure(addr string, err error) {
	// memberlist's multierror spans lines; keep one join failure on one line.
	err = fmt.Errorf("%s", strings.Join(strings.Fields(err.Error()), " "))
	switch classifyJoinError(err) {
	case joinMismatch:
		m.logMu.Lock()
		first := !m.mismatchLog[addr]
		m.mismatchLog[addr] = true
		m.logMu.Unlock()
		if first {
			m.filter.logf("mesh mode mismatch with %s: it accepted the connection and closed it without answering; the two nodes differ in mesh.open or the mesh secret, or it is not a viiwork v2 mesh", addr)
		}
		if m.o.Debug {
			m.filter.logf("[DEBUG] join %s: %v", addr, err)
		}
	default:
		if m.o.Debug {
			m.filter.logf("[DEBUG] join %s: %v", addr, err)
		}
	}
}

func (m *Mesh) Name() string          { return m.o.Name }
func (m *Mesh) Mode() string          { return m.mode }
func (m *Mesh) Advertise() netip.Addr { return m.advertise }

// Members is every member this node knows, self included, sorted by name,
// departed members included for 24 h.
func (m *Mesh) Members() []Member { return m.table.snapshot() }

func (m *Mesh) Member(name string) (Member, bool) {
	for _, mem := range m.table.snapshot() {
		if mem.Name == name {
			return mem, true
		}
	}
	return Member{}, false
}

// NumAlive counts alive members, self included.
func (m *Mesh) NumAlive() int {
	n := 0
	for _, mem := range m.table.snapshot() {
		if mem.State == meshapi.MemberAlive {
			n++
		}
	}
	return n
}

// Broadcast gossips one payload message; a newer message with the same key
// replaces one still queued.
func (m *Mesh) Broadcast(key string, msg []byte) { m.d.broadcast(key, msg) }

// Fatal yields a duplicate-name error for a process that must exit non-zero.
func (m *Mesh) Fatal() <-chan error { return m.d.fatal() }

// Leave tells every other alive member best-effort that this node is leaving,
// so they record left rather than dead, then leaves memberlist. A second call
// does nothing.
func (m *Mesh) Leave(timeout time.Duration) error {
	var err error
	m.leaveOnce.Do(func() {
		notice := frame(frameLeave, []byte(m.o.Name))
		for _, n := range m.ml.Members() {
			if n.Name != m.o.Name {
				_ = m.ml.SendBestEffort(n, notice)
			}
		}
		err = m.ml.Leave(timeout)
	})
	return err
}

// Shutdown stops the rejoin loop, mDNS and memberlist, then the event
// dispatcher once queued events are delivered. It is idempotent. Callers
// Leave, drain, then Shutdown.
func (m *Mesh) Shutdown() error {
	var err error
	m.shutdownOnce.Do(func() {
		if m.stopLoop != nil {
			m.stopLoop()
			<-m.loopDone
		}
		if m.mdnsStop != nil {
			_ = m.mdnsStop()
		}
		err = m.ml.Shutdown()
		m.stopDispatcher()
	})
	return err
}
