package peer

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/model"
)

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

func (r *Registry) SetPowerReader(p PowerReader) {
	r.power = p
}

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
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil { p.MarkUnreachable(); return }
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
	seen := make(map[string]string)
	seen[r.localModel] = "local"
	for _, p := range r.peers {
		if p.Status() != StatusReachable { continue }
		for _, m := range p.Models() {
			if _, exists := seen[m]; !exists { seen[m] = "peer" }
		}
	}
	var models []model.ModelEntry
	for id, owner := range seen {
		models = append(models, model.ModelEntry{ID: id, Object: "model", OwnedBy: owner})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models
}

type ClusterResponse struct {
	NodeID                string            `json:"node_id"`
	Version               string            `json:"version,omitempty"`
	Local                 ClusterLocalInfo  `json:"local"`
	Peers                 []ClusterPeerInfo `json:"peers"`
	Models                []string          `json:"models"`
	ClusterCostEURPerHour float64           `json:"cluster_cost_eur_per_hour,omitempty"`
	ClusterCostTodayEUR   float64           `json:"cluster_cost_today_eur,omitempty"`
}

type ClusterLocalInfo struct {
	Model          string               `json:"model"`
	PowerWatts     float64              `json:"power_watts"`
	PowerAvailable bool                 `json:"power_available"`
	Backends       []ClusterBackendInfo `json:"backends"`
	CostAvailable  bool                 `json:"cost_available,omitempty"`
	CostEURPerHour float64              `json:"cost_eur_per_hour,omitempty"`
	CostTodayEUR   float64              `json:"cost_today_eur,omitempty"`
}

type ClusterBackendInfo struct {
	GPUID    int    `json:"gpu_id"`
	Status   string `json:"status"`
	InFlight int64  `json:"in_flight"`
}

type ClusterPeerInfo struct {
	Addr            string   `json:"addr"`
	Status          string   `json:"status"`
	NodeID          string   `json:"node_id,omitempty"`
	Models          []string `json:"models,omitempty"`
	TotalInFlight   int64    `json:"total_in_flight,omitempty"`
	HealthyBackends int      `json:"healthy_backends,omitempty"`
	PowerWatts      float64  `json:"power_watts,omitempty"`
	PowerAvailable  bool     `json:"power_available,omitempty"`
	CostAvailable   bool     `json:"cost_available,omitempty"`
	CostEURPerHour  float64  `json:"cost_eur_per_hour,omitempty"`
	CostTodayEUR    float64  `json:"cost_today_eur,omitempty"`
}

func (r *Registry) ClusterState() ClusterResponse {
	resp := ClusterResponse{NodeID: r.nodeID, Local: ClusterLocalInfo{Model: r.localModel}}
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
		resp.Local.Backends = append(resp.Local.Backends, ClusterBackendInfo{GPUID: b.GPUID, Status: b.Status().String(), InFlight: b.InFlight()})
	}
	modelSet := map[string]bool{r.localModel: true}
	for _, p := range r.peers {
		info := ClusterPeerInfo{Addr: p.Addr, Status: p.Status().String()}
		if p.Status() == StatusReachable {
			info.NodeID = p.NodeID()
			info.Models = p.Models()
			info.TotalInFlight = p.TotalInFlight()
			info.HealthyBackends = p.HealthyBackends()
			info.PowerWatts = p.PowerWatts()
			info.PowerAvailable = p.PowerAvailable()
			info.CostAvailable = p.CostAvailable()
			info.CostEURPerHour = p.CostEURPerHour()
			info.CostTodayEUR = p.CostTodayEUR()
			if p.CostAvailable() {
				resp.ClusterCostEURPerHour += p.CostEURPerHour()
				resp.ClusterCostTodayEUR += p.CostTodayEUR()
			}
			for _, m := range info.Models { modelSet[m] = true }
		}
		resp.Peers = append(resp.Peers, info)
	}
	for m := range modelSet { resp.Models = append(resp.Models, m) }
	sort.Strings(resp.Models)
	return resp
}
