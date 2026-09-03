package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/meshauth"
	"github.com/janit/viiwork/internal/model"
	"github.com/janit/viiwork/meshapi"
)

func hostOfAddr(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[:i]
	}
	return addr
}

type PowerReader interface {
	Watts() float64
	Available() bool
}

// EnergyReader is the durable kWh store, as the API layer needs it: one
// rolling figure. Kept narrow like PowerReader so a node without the store
// enabled simply supplies nothing.
type EnergyReader interface {
	KWh24h() float64
	KWh30d() float64
}

type CostReader interface {
	Available() bool
	EURPerHour() float64
	TodayEUR() float64
	SpotCentsKWh() float64
	TransferCentsKWh() float64
	TaxCentsKWh() float64
	VATPercent() float64
	TotalCentsKWh() float64
}

// peerSet is the peer list as readers see it. Built fresh whenever membership
// changes and swapped in one store, so the request path — FindRoutesForModel,
// AllModels, ClusterState, IsKnownPeer, all called from request goroutines —
// takes no lock and never observes a half-built list.
type peerSet struct {
	all    []*PeerState
	byAddr map[string]*PeerState
	byNode map[string]*PeerState
}

func buildPeerSet(peers []*PeerState) *peerSet {
	s := &peerSet{
		all:    peers,
		byAddr: make(map[string]*PeerState, len(peers)),
		byNode: make(map[string]*PeerState, len(peers)),
	}
	for _, p := range peers {
		s.byAddr[p.Addr] = p
		// byNode backs IsKnownPeer and so decides which forwards are treated
		// as peer traffic. Configured peers belong here whether or not they
		// can sign: during a rollout the un-upgraded half of the fleet cannot,
		// and dropping them would break routing to exactly the nodes that
		// still depend on the old path.
		if id := p.NodeID(); id != "" && (p.Origin() == OriginConfig || p.Verified()) {
			s.byNode[id] = p
		}
	}
	return s
}

type Registry struct {
	nodeID     string
	localModel string
	backends   []*balancer.BackendState
	peers      atomic.Pointer[peerSet] // readers lock-free; writers hold mu
	mu         sync.Mutex              // serialises membership changes (poll goroutine only)
	timeout    time.Duration
	logger     *log.Logger
	client     *http.Client
	power      PowerReader
	cost       CostReader
	listenAddr string
	hostname   string
	// promptHistory is this node's prompt-store capacity, published on
	// /v1/status and /v1/cluster so the dashboard sizes its own list from the
	// server's number instead of keeping a second copy that can drift.
	promptHistory int

	signer *meshauth.Signer
	gossip GossipOptions

	round atomic.Uint64
	// justVerified holds peers that proved membership since the last
	// discovery round and so deserve an immediate cluster poll. Without it a
	// chain converges in multiples of DiscoveryEvery instead of in rounds.
	justVerified []*PeerState
	capLogged    atomic.Bool

	// skipAddrValidation disables validPeerAddr, for tests in this package
	// only: httptest servers necessarily bind loopback, which the allow-list
	// rejects by design. Never exported, never set outside a _test.go file,
	// and never after the first round — nothing in a production build has any
	// way to turn address validation off.
	skipAddrValidation bool
}

// GossipOptions is the adoption policy. Zero value means no adoption, which
// is the default and is exactly pre-gossip behaviour.
type GossipOptions struct {
	Enabled         bool
	DiscoveryEvery  int
	MaxLearnedPeers int
	AllowPrivate    bool
}

// SetSigner supplies the mesh membership proof. Set it whenever a secret is
// configured, whether or not this node adopts: signing is what makes this
// node adoptable by others, and the two switches are deliberately separate.
// Set once at startup before Run, so it needs no lock.
func (r *Registry) SetSigner(s *meshauth.Signer) { r.signer = s }

func (r *Registry) SetGossip(o GossipOptions) {
	if o.DiscoveryEvery <= 0 {
		o.DiscoveryEvery = 6
	}
	if o.MaxLearnedPeers <= 0 {
		o.MaxLearnedPeers = 200
	}
	r.gossip = o
}

func NewRegistry(nodeID string, localModel string, backends []*balancer.BackendState, peers []*PeerState, timeout time.Duration) *Registry {
	r := &Registry{
		nodeID: nodeID, localModel: localModel, backends: backends,
		timeout: timeout, logger: log.New(os.Stdout, "[mesh] ", log.LstdFlags),
		client: &http.Client{Timeout: timeout},
	}
	r.peers.Store(buildPeerSet(peers))
	return r
}

func (r *Registry) NodeID() string                     { return r.nodeID }
func (r *Registry) LocalModel() string                 { return r.localModel }
func (r *Registry) Backends() []*balancer.BackendState { return r.backends }
func (r *Registry) Peers() []*PeerState                { return r.peers.Load().all }

// IsKnownPeer returns true if the node ID belongs to a peer this node counts
// as mesh traffic.
func (r *Registry) IsKnownPeer(nodeID string) bool {
	_, ok := r.peers.Load().byNode[nodeID]
	return ok
}

// addPeers appends peers that are not already held and republishes the set.
// Callers hold no lock; this one does its own. Only the poll goroutine calls
// it, but the lock is what makes that an implementation detail rather than an
// invariant a future caller can break silently.
func (r *Registry) addPeers(ps ...*PeerState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.peers.Load()
	var added []*PeerState
	for _, p := range ps {
		if _, dup := cur.byAddr[p.Addr]; dup {
			continue
		}
		added = append(added, p)
	}
	if len(added) == 0 {
		return
	}
	next := make([]*PeerState, 0, len(cur.all)+len(added))
	next = append(next, cur.all...)
	next = append(next, added...)
	r.peers.Store(buildPeerSet(next))
}

// republish rebuilds the reader-visible set from the current membership. Call
// after a round changes anything buildPeerSet derives — a node ID learned, a
// peer verified — since those live in PeerState, not in the set.
func (r *Registry) republish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.peers.Store(buildPeerSet(r.peers.Load().all))
}

func (r *Registry) SetPowerReader(p PowerReader) {
	r.power = p
}

// SetLocation records the host:port this viiwork node listens on so it can
// be surfaced in /v1/status and used by peers to detect co-located instances.
// Hostname should be os.Hostname() or a DNS-resolvable name (not 0.0.0.0).
func (r *Registry) SetLocation(hostname, listenAddr string) {
	r.hostname = hostname
	r.listenAddr = listenAddr
}
func (r *Registry) SetPromptHistory(n int) { r.promptHistory = n }
func (r *Registry) PromptHistory() int     { return r.promptHistory }

func (r *Registry) Hostname() string   { return r.hostname }
func (r *Registry) ListenAddr() string { return r.listenAddr }

func (r *Registry) Power() PowerReader {
	return r.power
}

func (r *Registry) SetCostReader(c CostReader) { r.cost = c }
func (r *Registry) Cost() CostReader            { return r.cost }

func (r *Registry) Run(ctx context.Context, interval time.Duration) {
	// With gossip off, no peers means nothing will ever appear and the loop
	// is pointless. With gossip on, peers can be adopted after startup — the
	// operator may add a seed via a peer's advertisement of this node — so
	// the loop must keep running even from an empty start.
	if len(r.peers.Load().all) == 0 && !r.gossip.Enabled { return }
	r.logger.Printf("starting peer poll loop (%d peers, interval %s)", len(r.peers.Load().all), interval)
	r.PollOnce(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done(): return
		case <-ticker.C: r.PollOnce(ctx)
		}
	}
}

func (r *Registry) PollOnce(ctx context.Context) {
	for _, p := range r.peers.Load().all {
		before := p.Verified()
		r.pollPeer(ctx, p)
		if !before && p.Verified() {
			r.mu.Lock()
			r.justVerified = append(r.justVerified, p)
			r.mu.Unlock()
		}
	}
	// A node ID learned this round has to reach byNode before the next
	// forward asks IsKnownPeer about it.
	r.republish()
	r.discoveryRound(ctx)
}

// discoveryRound cluster-polls the peers due this round and adopts what they
// report. Every peer once per DiscoveryEvery rounds, plus anything verified
// since the last round.
func (r *Registry) discoveryRound(ctx context.Context) {
	if !r.gossip.Enabled || r.signer == nil {
		return
	}
	n := r.round.Add(1)

	all := r.peers.Load().all
	var targets []*PeerState
	if uint64(r.gossip.DiscoveryEvery) <= 1 || n%uint64(r.gossip.DiscoveryEvery) == 1 {
		targets = all
	} else {
		r.mu.Lock()
		targets = r.justVerified
		r.justVerified = nil
		r.mu.Unlock()
	}

	for _, p := range targets {
		if !p.Verified() {
			continue // its peer list is hearsay until it proves membership
		}
		cluster, err := r.pollCluster(ctx, p)
		if err != nil {
			continue
		}
		r.adopt(cluster.Peers)
	}
}

// pollCluster fetches a peer's cluster snapshot and returns it only if the
// response proved membership. An unproven snapshot is discarded entirely
// rather than used for its non-peer fields: a caller cannot be trusted with
// half a verified document.
func (r *Registry) pollCluster(ctx context.Context, p *PeerState) (*ClusterResponse, error) {
	url := fmt.Sprintf("http://%s%s", p.Addr, meshapi.PathCluster)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	nonce, err := r.signer.SignRequest(req, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if err := r.signer.VerifyResponse(resp.Header, meshapi.PathCluster, nonce, body); err != nil {
		return nil, err
	}
	var out ClusterResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// adopt takes the peer addresses from a verified snapshot. Each is validated
// before it can ever be dialled, and the total is capped: the dial precedes
// any proof, so an unbounded, unvalidated intake is a resource-exhaustion and
// SSRF primitive handed to whichever node advertises the most addresses.
func (r *Registry) adopt(advertised []ClusterPeerInfo) {
	cur := r.peers.Load()
	learned := 0
	for _, p := range cur.all {
		if p.Origin() == OriginLearned {
			learned++
		}
	}

	var add []*PeerState
	var rejected int
	for _, a := range advertised {
		if a.Addr == "" {
			continue
		}
		if _, dup := cur.byAddr[a.Addr]; dup {
			continue
		}
		if a.NodeID != "" && a.NodeID == r.nodeID {
			continue // ourselves, as seen by someone else
		}
		if _, dup := cur.byNode[a.NodeID]; a.NodeID != "" && dup {
			continue // a node we already hold, at a second address
		}
		if !r.skipAddrValidation {
			if err := validPeerAddr(a.Addr, r.gossip.AllowPrivate); err != nil {
				rejected++
				continue
			}
		}
		if learned+len(add) >= r.gossip.MaxLearnedPeers {
			// One line however many overflow: a node flooding addresses must
			// not become a log flood in its own right.
			if !r.capLogged.Swap(true) {
				r.logger.Printf("learned-peer cap reached (%d); further advertised addresses ignored", r.gossip.MaxLearnedPeers)
			}
			break
		}
		add = append(add, NewLearnedPeerState(a.Addr, a.Hostname))
	}
	if rejected > 0 {
		r.logger.Printf("rejected %d advertised peer address(es) by validation; dropped, never dialled", rejected)
	}
	for _, p := range add {
		r.logger.Printf("adopted peer %s (%s) via gossip", p.Addr, p.Hostname())
	}
	r.addPeers(add...)
}

func (r *Registry) pollPeer(ctx context.Context, p *PeerState) {
	url := fmt.Sprintf("http://%s%s", p.Addr, meshapi.PathStatus)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil { p.MarkUnreachable(); return }
	var nonce string
	if r.signer != nil {
		if nonce, err = r.signer.SignRequest(req, nil); err != nil {
			p.MarkUnreachable()
			return
		}
	}
	resp, err := r.client.Do(req)
	if err != nil {
		if p.Status() == StatusReachable { r.logger.Printf("peer %s unreachable: %v", p.Addr, err) }
		p.MarkUnreachable(); return
	}
	defer resp.Body.Close()

	// Read before decoding: the proof covers the exact bytes, so the digest
	// has to be taken over what arrived rather than over a re-encoding of
	// what was parsed. 1 MB cap unchanged — a rogue peer must not be able to
	// make one poll balloon this process.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil { p.MarkUnreachable(); return }
	var status StatusResponse
	if err := json.Unmarshal(body, &status); err != nil { p.MarkUnreachable(); return }
	if status.NodeID == r.nodeID { p.MarkUnreachable(); return } // self-detection

	// A failed or absent proof is not a failed poll. The peer is reachable
	// and, if configured, routable; it is simply not a source of gossip and
	// not adoptable. That is what carries a mixed-version fleet.
	if r.signer != nil && nonce != "" {
		if err := r.signer.VerifyResponse(resp.Header, meshapi.PathStatus, nonce, body); err == nil {
			p.MarkVerified()
		} else if !errors.Is(err, meshauth.ErrNoProof) {
			r.logger.Printf("peer %s offered a mesh proof that did not verify: %v", p.Addr, err)
		}
	}

	wasUnreachable := p.Status() == StatusUnreachable
	p.Update(status)
	if wasUnreachable { r.logger.Printf("peer %s (%s) is now reachable, models: %v", p.Addr, status.NodeID, status.Models) }
}

func (r *Registry) FindRoutesForModel(modelName string) []Route {
	snap := r.peers.Load()
	// Pre-sized: this runs once per request, and the upper bound is known.
	routes := make([]Route, 0, len(r.backends)+len(snap.all))
	if modelName == r.localModel {
		for _, b := range r.backends {
			if b.Status() == balancer.StatusHealthy {
				routes = append(routes, Route{Type: RouteLocal, Backend: b, InFlight: b.InFlight()})
			}
		}
	}
	for _, p := range snap.all {
		if !p.Routable() {
			continue
		}
		// HasModel, not range over Models(): the latter copies the peer's model
		// slice on every call.
		if p.HasModel(modelName) {
			routes = append(routes, Route{Type: RoutePeer, Addr: p.Addr, Peer: p, InFlight: p.TotalInFlight()})
		}
	}
	return routes
}

func (r *Registry) AllModels() []model.ModelEntry {
	seen := make(map[string]bool)
	seen[r.localModel] = true
	// Local model always first — callers can assume models[0] is what this node serves.
	models := []model.ModelEntry{{ID: r.localModel, Object: "model", OwnedBy: "local"}}
	var peerModels []model.ModelEntry
	for _, p := range r.peers.Load().all {
		if !p.Routable() { continue }
		for _, m := range p.Models() {
			if seen[m] { continue }
			seen[m] = true
			peerModels = append(peerModels, model.ModelEntry{ID: m, Object: "model", OwnedBy: "peer"})
		}
	}
	sort.Slice(peerModels, func(i, j int) bool { return peerModels[i].ID < peerModels[j].ID })
	return append(models, peerModels...)
}

// The cluster snapshot types live in meshapi with the rest of the wire
// contract, and are aliased here so this package keeps its familiar names.
// See internal/peer/peer.go for why these are aliases rather than wrappers.
type (
	ClusterResponse    = meshapi.ClusterResponse
	PowerControlInfo   = meshapi.PowerControlInfo
	ClusterLocalInfo   = meshapi.ClusterLocalInfo
	ClusterBackendInfo = meshapi.ClusterBackendInfo
	ClusterPeerInfo    = meshapi.ClusterPeerInfo
)

func (r *Registry) ClusterState() ClusterResponse {
	resp := ClusterResponse{NodeID: r.nodeID, Hostname: r.hostname, Local: ClusterLocalInfo{Model: r.localModel, ListenAddr: r.listenAddr, PromptHistory: r.promptHistory}}
	if r.power != nil {
		resp.Local.PowerWatts = r.power.Watts()
		resp.Local.PowerAvailable = r.power.Available()
		if named, ok := r.power.(interface{ SourceName() string }); ok && resp.Local.PowerAvailable {
			resp.Local.PowerSource = named.SourceName()
		}
	}
	if r.cost != nil && r.cost.Available() {
		resp.Local.CostAvailable = true
		resp.Local.CostEURPerHour = r.cost.EURPerHour()
		resp.Local.CostTodayEUR = r.cost.TodayEUR()
		resp.ClusterCostEURPerHour += r.cost.EURPerHour()
		resp.ClusterCostTodayEUR += r.cost.TodayEUR()
	}
	for _, b := range r.backends {
		var gpuIDs []int
		if len(b.GPUIDs) > 0 {
			gpuIDs = append(gpuIDs, b.GPUIDs...)
		}
		resp.Local.Backends = append(resp.Local.Backends, ClusterBackendInfo{
			GPUID: b.GPUID, GPUIDs: gpuIDs, Model: r.localModel, Status: b.Status().String(), InFlight: b.InFlight(),
			RSSMB: b.RSSMB(), SlotCtx: b.SlotCtx(), SlotCount: b.SlotCount(), SlotActive: b.SlotActive(),
			TokDecoded: b.TokDecoded(), TokRemain: b.TokRemain(),
		})
	}
	snap := r.peers.Load()
	modelSet := map[string]bool{r.localModel: true}
	// Count reachable peers whose host (from p.Addr) matches our hostname.
	// single_host is true when: hostname known AND we have peers AND every
	// reachable peer shares that hostname. Unreachable peers don't disqualify
	// the topology — they're just temporarily down.
	singleHost := r.hostname != "" && len(snap.all) > 0
	reachableCount := 0
	for _, p := range snap.all {
		// Prefer the hostname the peer reports over the one derived from the
		// address we happen to dial it on. They disagree exactly where it
		// matters: co-located instances are configured by IP, so deriving
		// from the address splits one machine into "gb1" (this node) and
		// "192.168.1.41" (its own co-tenants) -- two hosts on screen, one
		// host in the rack, and a per-host wattage that cannot be deduplicated.
		// Falls back to the address for a peer too old to report it.
		// A learned peer that has not proved membership is this node's
		// private suspicion, not a fact about the mesh. Relaying it would let
		// one node's unvalidated intake become the whole fleet's.
		if p.Origin() == OriginLearned && !p.Verified() {
			continue
		}
		host := p.Hostname()
		if host == "" { host = hostOfAddr(p.Addr) }
		info := ClusterPeerInfo{Addr: p.Addr, Hostname: host, Status: p.Status().String(), Origin: p.Origin()}
		if p.Status() == StatusReachable {
			reachableCount++
			info.NodeID = p.NodeID()
			info.Models = p.Models()
			info.TotalInFlight = p.TotalInFlight()
			info.HealthyBackends = p.HealthyBackends()
			info.PromptHistory = p.PromptHistory()
			info.PowerWatts = p.PowerWatts()
			info.PowerAvailable = p.PowerAvailable()
			info.PowerSource = p.PowerSource()
			info.HostMemTotalMB, info.HostMemUsedMB = p.HostMem()
			info.EnergyKWh24h = p.EnergyKWh24h()
			info.EnergyKWh30d = p.EnergyKWh30d()
			info.CostAvailable = p.CostAvailable()
			info.CostEURPerHour = p.CostEURPerHour()
			info.CostTodayEUR = p.CostTodayEUR()
			info.GPUs = p.GPUs()
			for _, pb := range p.Backends() {
				info.Backends = append(info.Backends, ClusterBackendInfo{
					GPUID: pb.GPUID, GPUIDs: append([]int(nil), pb.GPUIDs...),
					Model: pb.Model, Status: pb.Status, InFlight: pb.InFlight,
					RSSMB: pb.RSSMB, SlotCtx: pb.SlotCtx, SlotCount: pb.SlotCount,
					SlotActive: pb.SlotActive, TokDecoded: pb.TokDecoded, TokRemain: pb.TokRemain,
				})
			}
			if p.CostAvailable() {
				resp.ClusterCostEURPerHour += p.CostEURPerHour()
				resp.ClusterCostTodayEUR += p.CostTodayEUR()
			}
			for _, m := range info.Models { modelSet[m] = true }
			if info.Hostname != r.hostname {
				singleHost = false
			}
		}
		resp.Peers = append(resp.Peers, info)
	}
	if reachableCount == 0 {
		singleHost = false
	}
	resp.SingleHost = singleHost
	for m := range modelSet { resp.Models = append(resp.Models, m) }
	sort.Strings(resp.Models)
	return resp
}

// GPUModels maps every GPU on this host to the model occupying it.
//
// A multi-model host runs one viiwork instance per model, so this node's own
// backends account for only a fraction of the box -- on the reference 10-GPU
// host, one card in ten. Co-located instances already publish the rest on
// /v1/status: their backends carry gpu_ids and a model, and a matching
// hostname is what identifies them as sharing this machine rather than being
// a remote peer.
//
// Unreachable peers are skipped: their last-known backends are no longer
// serving, so their cards are idle and labelling them would attribute energy
// to a model that is not running.
func (r *Registry) GPUModels() map[int]string {
	owned := make(map[int]string)
	for _, p := range r.peers.Load().all {
		if p.Status() != StatusReachable { continue }
		if h := p.Hostname(); h == "" || r.hostname == "" || h != r.hostname { continue }
		for _, b := range p.Backends() {
			for _, id := range backendGPUs(b.GPUID, b.GPUIDs) { owned[id] = b.Model }
		}
	}
	// Local last: this node is authoritative for its own cards, so a peer
	// entry that points back at this node cannot override them.
	for _, b := range r.backends {
		for _, id := range backendGPUs(b.GPUID, b.GPUIDs) { owned[id] = r.localModel }
	}
	return owned
}

// backendGPUs normalises the two ways a backend reports its cards: a
// tensor-split group fills the group slice, a single-GPU replica only the
// scalar. Negative ids mean "unassigned" and are dropped.
func backendGPUs(single int, group []int) []int {
	ids := group
	if len(ids) == 0 { ids = []int{single} }
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		if id >= 0 { out = append(out, id) }
	}
	return out
}
