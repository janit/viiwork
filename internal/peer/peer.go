// internal/peer/peer.go
package peer

import (
	"sync"
	"sync/atomic"
)

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
	// PowerSource names which IPMI reading this node settled on ("dcmi",
	// `sdr type "Power Supply"`, "sensor:SYS_POWER"). It exists so a fleet-wide
	// power outage of the reporting kind is visible without reading logs on
	// five hosts. Absent from nodes older than the probing sampler.
	PowerSource string `json:"power_source,omitempty"`
	CostAvailable  bool               `json:"cost_available"`
	CostEURPerHour float64            `json:"cost_eur_per_hour,omitempty"`
	CostTodayEUR   float64            `json:"cost_today_eur,omitempty"`
	CostBreakdown  *CostBreakdownJSON `json:"cost_breakdown,omitempty"`
	// GPUs lets any node render GPU load for every host in the mesh, not just
	// its own. omitempty keeps this compatible with nodes that predate it.
	GPUs []GPUInfo `json:"gpus,omitempty"`

	// PromptHistory is how many requests this node keeps prompt and output
	// for. Absent from nodes older than v1.1.1, which is why a consumer must
	// read zero as "unknown", not as "keeps nothing".
	PromptHistory int `json:"prompt_history,omitempty"`

	// Host RAM, so the mesh view can show memory pressure for every host and
	// not only the one it is served from. Used is MemTotal - MemAvailable, so
	// reclaimable page cache is not counted as pressure. Absent from older
	// nodes, and a zero total means "unknown", not "no memory".
	HostMemTotalMB int64 `json:"host_mem_total_mb,omitempty"`
	HostMemUsedMB  int64 `json:"host_mem_used_mb,omitempty"`

	// EnergyKWh24h is whole-node energy over the rolling last 24 hours, from
	// the durable store, which runs on exactly one instance per host — so on a
	// multi-model host the other instances report nothing rather than a second
	// copy of it.
	EnergyKWh24h float64 `json:"energy_kwh_24h,omitempty"`
}

type BackendInfo struct {
	GPUID    int    `json:"gpu_id"`
	GPUIDs   []int  `json:"gpu_ids,omitempty"` // populated in tensor-split mode
	Model    string `json:"model"`
	Status   string `json:"status"`
	InFlight int64  `json:"in_flight"`
	// Everything below is additive and omitempty: a node running an older
	// build simply omits these and the mesh view degrades to blanks for that
	// host rather than breaking. Without them a peer's backends arrive with
	// no RSS or context figures, which is most of what the mesh view shows.
	RSSMB      int64 `json:"rss_mb,omitempty"`
	SlotCtx    int64 `json:"slot_ctx,omitempty"`
	SlotCount  int   `json:"slot_count,omitempty"`
	SlotActive int   `json:"slot_active,omitempty"`
	TokDecoded int64 `json:"tok_decoded,omitempty"`
	TokRemain  int64 `json:"tok_remain,omitempty"`
}

// GPUInfo is a single GPU's live utilisation, propagated across the mesh so any
// node can render GPU load for every host. Locally this comes from the gpu
// collector; for peers it arrives on the /v1/status poll.
type GPUInfo struct {
	GPUID       int     `json:"gpu_id"`
	Util        float64 `json:"util"`
	VRAMUsedMB  float64 `json:"vram_used_mb"`
	VRAMTotalMB float64 `json:"vram_total_mb"`
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

	// localInFlight tracks requests this node has dispatched to the peer and
	// not yet received a response for. It's updated write-through on every
	// peer-bound proxy call so the picker isn't blind between the polls that
	// refresh totalInFlight (poll interval is typically 10s, much longer than
	// a burst). Combined with totalInFlight via max() in TotalInFlight().
	localInFlight atomic.Int64

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
	powerSource     string
	hostMemTotalMB  int64
	hostMemUsedMB   int64
	energyKWh24h    float64
	costAvailable  bool
	costEURPerHour float64
	costTodayEUR   float64
	gpus           []GPUInfo
	promptHistory  int
}

// GPUs returns a copy of the peer's last reported GPU utilisation.
func (p *PeerState) GPUs() []GPUInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]GPUInfo, len(p.gpus))
	copy(out, p.gpus)
	return out
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
	p.powerSource = resp.PowerSource
	p.hostMemTotalMB = resp.HostMemTotalMB
	p.hostMemUsedMB = resp.HostMemUsedMB
	p.energyKWh24h = resp.EnergyKWh24h
	p.costAvailable = resp.CostAvailable
	p.costEURPerHour = resp.CostEURPerHour
	p.costTodayEUR = resp.CostTodayEUR
	p.gpus = append(p.gpus[:0], resp.GPUs...)
	p.promptHistory = resp.PromptHistory
}

func (p *PeerState) MarkUnreachable() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status = StatusUnreachable
	p.models = nil
	p.backends = nil
	p.gpus = nil
	p.powerWatts = 0
	p.powerAvailable = false
	p.powerSource = ""
	p.hostMemTotalMB = 0
	p.hostMemUsedMB = 0
	p.energyKWh24h = 0
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

// HasModel reports whether this peer serves the named model.
//
// Callers on the request path must use this rather than ranging over Models():
// that method defensively copies the whole slice on every call, so route
// resolution allocated once per peer per request purely to run a membership
// test. Here the check happens under the same read lock with nothing escaping.
func (p *PeerState) HasModel(name string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, m := range p.models {
		if m == name {
			return true
		}
	}
	return false
}

// TotalInFlight returns the larger of the last-polled in-flight count and the
// write-through local count. Polled data reflects traffic from all mesh nodes
// (correct across nodes but stale between polls); local data tracks decisions
// this node has just made (fresh, but only ours). Max keeps the picker from
// underestimating load in either dimension.
func (p *PeerState) TotalInFlight() int64 {
	p.mu.RLock()
	polled := p.totalInFlight
	p.mu.RUnlock()
	local := p.localInFlight.Load()
	if local > polled {
		return local
	}
	return polled
}

// IncLocalInFlight is called by the proxy when a peer-bound request is
// dispatched. Pair with DecLocalInFlight when the request completes.
func (p *PeerState) IncLocalInFlight() { p.localInFlight.Add(1) }
func (p *PeerState) DecLocalInFlight() { p.localInFlight.Add(-1) }
func (p *PeerState) LocalInFlight() int64 { return p.localInFlight.Load() }
func (p *PeerState) HealthyBackends() int { p.mu.RLock(); defer p.mu.RUnlock(); return p.healthyBackends }

func (p *PeerState) PromptHistory() int { p.mu.RLock(); defer p.mu.RUnlock(); return p.promptHistory }

func (p *PeerState) PowerWatts() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.powerWatts
}

func (p *PeerState) EnergyKWh24h() float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.energyKWh24h
}

func (p *PeerState) HostMem() (totalMB, usedMB int64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.hostMemTotalMB, p.hostMemUsedMB
}

func (p *PeerState) PowerSource() string { p.mu.RLock(); defer p.mu.RUnlock(); return p.powerSource }

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
