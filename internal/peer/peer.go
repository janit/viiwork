// internal/peer/peer.go
package peer

import "sync"

type PeerStatus int

const (
	StatusUnreachable PeerStatus = iota
	StatusReachable
)

func (s PeerStatus) String() string {
	if s == StatusReachable { return "reachable" }
	return "unreachable"
}

type StatusResponse struct {
	NodeID          string        `json:"node_id"`
	Hostname        string        `json:"hostname,omitempty"`
	ListenAddr      string        `json:"listen_addr,omitempty"`
	Models          []string      `json:"models"`
	Backends        []BackendInfo `json:"backends"`
	TotalInFlight   int64         `json:"total_in_flight"`
	HealthyBackends int           `json:"healthy_backends"`
	TotalBackends   int           `json:"total_backends"`
	PowerWatts      float64       `json:"power_watts"`
	PowerAvailable  bool          `json:"power_available"`
	CostAvailable  bool               `json:"cost_available"`
	CostEURPerHour float64            `json:"cost_eur_per_hour,omitempty"`
	CostTodayEUR   float64            `json:"cost_today_eur,omitempty"`
	CostBreakdown  *CostBreakdownJSON `json:"cost_breakdown,omitempty"`
}

type BackendInfo struct {
	GPUID    int    `json:"gpu_id"`
	GPUIDs   []int  `json:"gpu_ids,omitempty"` // populated in tensor-split mode
	Model    string `json:"model"`
	Status   string `json:"status"`
	InFlight int64  `json:"in_flight"`
}

type CostBreakdownJSON struct {
	SpotCentsKWh     float64 `json:"spot_cents_kwh"`
	TransferCentsKWh float64 `json:"transfer_cents_kwh"`
	TaxCentsKWh      float64 `json:"tax_cents_kwh"`
	VATPercent       float64 `json:"vat_percent"`
	TotalCentsKWh    float64 `json:"total_cents_kwh"`
}

type PeerState struct {
	Addr string

	mu              sync.RWMutex
	nodeID          string
	hostname        string
	listenAddr      string
	status          PeerStatus
	models          []string
	backends        []BackendInfo
	totalInFlight   int64
	healthyBackends int
	totalBackends   int
	powerWatts      float64
	powerAvailable  bool
	costAvailable  bool
	costEURPerHour float64
	costTodayEUR   float64
}

func NewPeerState(addr string) *PeerState {
	return &PeerState{Addr: addr, status: StatusUnreachable}
}

func (p *PeerState) Update(resp StatusResponse) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nodeID = resp.NodeID
	p.hostname = resp.Hostname
	p.listenAddr = resp.ListenAddr
	p.status = StatusReachable
	p.models = resp.Models
	p.backends = append(p.backends[:0], resp.Backends...)
	p.totalInFlight = resp.TotalInFlight
	p.healthyBackends = resp.HealthyBackends
	p.totalBackends = resp.TotalBackends
	p.powerWatts = resp.PowerWatts
	p.powerAvailable = resp.PowerAvailable
	p.costAvailable = resp.CostAvailable
	p.costEURPerHour = resp.CostEURPerHour
	p.costTodayEUR = resp.CostTodayEUR
}

func (p *PeerState) MarkUnreachable() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status = StatusUnreachable
	p.models = nil
	p.backends = nil
	p.powerWatts = 0
	p.powerAvailable = false
	p.costAvailable = false
	p.costEURPerHour = 0
	p.costTodayEUR = 0
}

func (p *PeerState) NodeID() string { p.mu.RLock(); defer p.mu.RUnlock(); return p.nodeID }
func (p *PeerState) Status() PeerStatus { p.mu.RLock(); defer p.mu.RUnlock(); return p.status }
func (p *PeerState) Models() []string {
	p.mu.RLock(); defer p.mu.RUnlock()
	out := make([]string, len(p.models)); copy(out, p.models); return out
}
func (p *PeerState) TotalInFlight() int64 { p.mu.RLock(); defer p.mu.RUnlock(); return p.totalInFlight }
func (p *PeerState) HealthyBackends() int { p.mu.RLock(); defer p.mu.RUnlock(); return p.healthyBackends }

func (p *PeerState) PowerWatts() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.powerWatts
}

func (p *PeerState) PowerAvailable() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.powerAvailable
}

func (p *PeerState) CostAvailable() bool { p.mu.RLock(); defer p.mu.RUnlock(); return p.costAvailable }
func (p *PeerState) CostEURPerHour() float64 { p.mu.RLock(); defer p.mu.RUnlock(); return p.costEURPerHour }
func (p *PeerState) CostTodayEUR() float64 { p.mu.RLock(); defer p.mu.RUnlock(); return p.costTodayEUR }
func (p *PeerState) Hostname() string { p.mu.RLock(); defer p.mu.RUnlock(); return p.hostname }
func (p *PeerState) ListenAddr() string { p.mu.RLock(); defer p.mu.RUnlock(); return p.listenAddr }
func (p *PeerState) Backends() []BackendInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]BackendInfo, len(p.backends))
	copy(out, p.backends)
	return out
}
