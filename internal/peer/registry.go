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
	var routes []Route
	if modelName == r.localModel {
		for _, b := range r.backends {
			if b.Status() == balancer.StatusHealthy {
				routes = append(routes, Route{Type: RouteLocal, Backend: b, InFlight: b.InFlight()})
			}
		}
	}
	for _, p := range r.peers {
		if p.Status() != StatusReachable { continue }
		for _, m := range p.Models() {
			if m == modelName {
				routes = append(routes, Route{Type: RoutePeer, Addr: p.Addr, InFlight: p.TotalInFlight()})
				break
			}
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
	Model          string               `json:"model"`
	ListenAddr     string               `json:"listen_addr,omitempty"`
	PowerWatts     float64              `json:"power_watts"`
	PowerAvailable bool                 `json:"power_available"`
	Backends       []ClusterBackendInfo `json:"backends"`
	CostAvailable  bool                 `json:"cost_available,omitempty"`
	CostEURPerHour float64              `json:"cost_eur_per_hour,omitempty"`
	CostTodayEUR   float64              `json:"cost_today_eur,omitempty"`
	HostMemTotalMB int64                `json:"host_mem_total_mb,omitempty"`
	HostMemUsedMB  int64                `json:"host_mem_used_mb,omitempty"`
}

type ClusterBackendInfo struct {
	GPUID      int    `json:"gpu_id"`
	GPUIDs     []int  `json:"gpu_ids,omitempty"`
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
	Addr            string               `json:"addr"`
	Hostname        string               `json:"hostname,omitempty"`
	Status          string               `json:"status"`
	NodeID          string               `json:"node_id,omitempty"`
	Models          []string             `json:"models,omitempty"`
	Backends        []ClusterBackendInfo `json:"backends,omitempty"`
	TotalInFlight   int64                `json:"total_in_flight,omitempty"`
	HealthyBackends int                  `json:"healthy_backends,omitempty"`
	PowerWatts      float64              `json:"power_watts,omitempty"`
	PowerAvailable  bool                 `json:"power_available,omitempty"`
	CostAvailable   bool                 `json:"cost_available,omitempty"`
	CostEURPerHour  float64              `json:"cost_eur_per_hour,omitempty"`
	CostTodayEUR    float64              `json:"cost_today_eur,omitempty"`
}

func (r *Registry) ClusterState() ClusterResponse {
	resp := ClusterResponse{NodeID: r.nodeID, Hostname: r.hostname, Local: ClusterLocalInfo{Model: r.localModel, ListenAddr: r.listenAddr}}
	if r.power != nil {
		resp.Local.PowerWatts = r.power.Watts()
		resp.Local.PowerAvailable = r.power.Available()
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
			GPUID: b.GPUID, GPUIDs: gpuIDs, Status: b.Status().String(), InFlight: b.InFlight(),
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
		info := ClusterPeerInfo{Addr: p.Addr, Hostname: hostOfAddr(p.Addr), Status: p.Status().String()}
		if p.Status() == StatusReachable {
			reachableCount++
			info.NodeID = p.NodeID()
			info.Models = p.Models()
			info.TotalInFlight = p.TotalInFlight()
			info.HealthyBackends = p.HealthyBackends()
			info.PowerWatts = p.PowerWatts()
			info.PowerAvailable = p.PowerAvailable()
			info.CostAvailable = p.CostAvailable()
			info.CostEURPerHour = p.CostEURPerHour()
			info.CostTodayEUR = p.CostTodayEUR()
			for _, pb := range p.Backends() {
				info.Backends = append(info.Backends, ClusterBackendInfo{
					GPUID: pb.GPUID, GPUIDs: append([]int(nil), pb.GPUIDs...),
					Status: pb.Status, InFlight: pb.InFlight,
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
