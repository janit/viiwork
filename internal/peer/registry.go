package peer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/model"
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

type Registry struct {
	nodeID     string
	localModel string
	backends   []*balancer.BackendState
	peers      []*PeerState
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
}

func NewRegistry(nodeID string, localModel string, backends []*balancer.BackendState, peers []*PeerState, timeout time.Duration) *Registry {
	return &Registry{
		nodeID: nodeID, localModel: localModel, backends: backends, peers: peers,
		timeout: timeout, logger: log.New(os.Stdout, "[mesh] ", log.LstdFlags),
		client: &http.Client{Timeout: timeout},
	}
}

func (r *Registry) NodeID() string                     { return r.nodeID }
func (r *Registry) LocalModel() string                 { return r.localModel }
func (r *Registry) Backends() []*balancer.BackendState { return r.backends }
func (r *Registry) Peers() []*PeerState                { return r.peers }

// IsKnownPeer returns true if the given node ID matches any configured peer.
func (r *Registry) IsKnownPeer(nodeID string) bool {
	for _, p := range r.peers {
		if p.NodeID() == nodeID {
			return true
		}
	}
	return false
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
	if len(r.peers) == 0 { return }
	r.logger.Printf("starting peer poll loop (%d peers, interval %s)", len(r.peers), interval)
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
	for _, p := range r.peers { r.pollPeer(ctx, p) }
}

func (r *Registry) pollPeer(ctx context.Context, p *PeerState) {
	url := fmt.Sprintf("http://%s/v1/status", p.Addr)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil { p.MarkUnreachable(); return }
	resp, err := r.client.Do(req)
	if err != nil {
		if p.Status() == StatusReachable { r.logger.Printf("peer %s unreachable: %v", p.Addr, err) }
		p.MarkUnreachable(); return
	}
	defer resp.Body.Close()
	var status StatusResponse
	// Limit peer response body to 1 MB to prevent rogue peers from exhausting memory
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&status); err != nil { p.MarkUnreachable(); return }
	io.Copy(io.Discard, resp.Body) // drain remaining bytes for connection reuse
	if status.NodeID == r.nodeID { p.MarkUnreachable(); return } // self-detection
	wasUnreachable := p.Status() == StatusUnreachable
	p.Update(status)
	if wasUnreachable { r.logger.Printf("peer %s (%s) is now reachable, models: %v", p.Addr, status.NodeID, status.Models) }
}

func (r *Registry) FindRoutesForModel(modelName string) []Route {
	// Pre-sized: this runs once per request, and the upper bound is known.
	routes := make([]Route, 0, len(r.backends)+len(r.peers))
	if modelName == r.localModel {
		for _, b := range r.backends {
			if b.Status() == balancer.StatusHealthy {
				routes = append(routes, Route{Type: RouteLocal, Backend: b, InFlight: b.InFlight()})
			}
		}
	}
	for _, p := range r.peers {
		if p.Status() != StatusReachable {
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
	for _, p := range r.peers {
		if p.Status() != StatusReachable { continue }
		for _, m := range p.Models() {
			if seen[m] { continue }
			seen[m] = true
			peerModels = append(peerModels, model.ModelEntry{ID: m, Object: "model", OwnedBy: "peer"})
		}
	}
	sort.Slice(peerModels, func(i, j int) bool { return peerModels[i].ID < peerModels[j].ID })
	return append(models, peerModels...)
}

type ClusterResponse struct {
	NodeID                string            `json:"node_id"`
	Version               string            `json:"version,omitempty"`
	Hostname              string            `json:"hostname,omitempty"`
	SingleHost            bool              `json:"single_host,omitempty"`
	Local                 ClusterLocalInfo  `json:"local"`
	Peers                 []ClusterPeerInfo `json:"peers"`
	Models                []string          `json:"models"`
	ClusterCostEURPerHour float64           `json:"cluster_cost_eur_per_hour,omitempty"`
	ClusterCostTodayEUR   float64           `json:"cluster_cost_today_eur,omitempty"`
}

type ClusterLocalInfo struct {
	GPUs []GPUInfo `json:"gpus,omitempty"`
	Model          string               `json:"model"`
	ListenAddr     string               `json:"listen_addr,omitempty"`
	PowerWatts     float64              `json:"power_watts"`
	PowerAvailable bool                 `json:"power_available"`
	// PowerSource names the IPMI reading this host settled on ("dcmi",
	// "sdr:Power Supply", "sensor:PSU1"). Board-specific and probed at
	// startup, so the mesh view shows which one a host actually adopted.
	PowerSource string `json:"power_source,omitempty"`
	Backends       []ClusterBackendInfo `json:"backends"`
	CostAvailable  bool                 `json:"cost_available,omitempty"`
	CostEURPerHour float64              `json:"cost_eur_per_hour,omitempty"`
	CostTodayEUR   float64              `json:"cost_today_eur,omitempty"`
	PromptHistory  int                  `json:"prompt_history,omitempty"`
	HostMemTotalMB int64                `json:"host_mem_total_mb,omitempty"`
	HostMemUsedMB  int64                `json:"host_mem_used_mb,omitempty"`
}

type ClusterBackendInfo struct {
	GPUID int   `json:"gpu_id"`
	GPUIDs []int `json:"gpu_ids,omitempty"`
	// Model is per-backend rather than per-node because the mesh view groups by
	// it. A node serves one model today, but reading it off the backend keeps
	// the grouping correct if that ever stops being true.
	Model      string `json:"model,omitempty"`
	Status     string `json:"status"`
	InFlight   int64  `json:"in_flight"`
	RSSMB      int64  `json:"rss_mb,omitempty"`
	SlotCtx    int64  `json:"slot_ctx,omitempty"`
	SlotCount  int    `json:"slot_count,omitempty"`
	SlotActive int    `json:"slot_active,omitempty"`
	TokDecoded int64  `json:"tok_decoded,omitempty"`
	TokRemain  int64  `json:"tok_remain,omitempty"`
}

type ClusterPeerInfo struct {
	GPUs            []GPUInfo            `json:"gpus,omitempty"`
	Addr            string               `json:"addr"`
	Hostname        string               `json:"hostname,omitempty"`
	Status          string               `json:"status"`
	NodeID          string               `json:"node_id,omitempty"`
	Models          []string             `json:"models,omitempty"`
	Backends        []ClusterBackendInfo `json:"backends,omitempty"`
	TotalInFlight   int64                `json:"total_in_flight,omitempty"`
	HealthyBackends int                  `json:"healthy_backends,omitempty"`
	PromptHistory   int                  `json:"prompt_history,omitempty"`
	PowerWatts      float64              `json:"power_watts,omitempty"`
	PowerAvailable  bool                 `json:"power_available,omitempty"`
	PowerSource     string               `json:"power_source,omitempty"`
	HostMemTotalMB  int64                `json:"host_mem_total_mb,omitempty"`
	HostMemUsedMB   int64                `json:"host_mem_used_mb,omitempty"`
	CostAvailable   bool                 `json:"cost_available,omitempty"`
	CostEURPerHour  float64              `json:"cost_eur_per_hour,omitempty"`
	CostTodayEUR    float64              `json:"cost_today_eur,omitempty"`
}

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
	modelSet := map[string]bool{r.localModel: true}
	// Count reachable peers whose host (from p.Addr) matches our hostname.
	// single_host is true when: hostname known AND we have peers AND every
	// reachable peer shares that hostname. Unreachable peers don't disqualify
	// the topology — they're just temporarily down.
	singleHost := r.hostname != "" && len(r.peers) > 0
	reachableCount := 0
	for _, p := range r.peers {
		// Prefer the hostname the peer reports over the one derived from the
		// address we happen to dial it on. They disagree exactly where it
		// matters: co-located instances are configured by IP, so deriving
		// from the address splits one machine into "gb1" (this node) and
		// "192.168.1.41" (its own co-tenants) -- two hosts on screen, one
		// host in the rack, and a per-host wattage that cannot be deduplicated.
		// Falls back to the address for a peer too old to report it.
		host := p.Hostname()
		if host == "" { host = hostOfAddr(p.Addr) }
		info := ClusterPeerInfo{Addr: p.Addr, Hostname: host, Status: p.Status().String()}
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
	for _, p := range r.peers {
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
