package node

import (
	"net/netip"
	"regexp"
	"sort"
	"time"

	"github.com/janit/viiwork/v2/energy"
	"github.com/janit/viiwork/v2/internal/cost"
	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/gpu"
	"github.com/janit/viiwork/v2/internal/hostinfo"
	"github.com/janit/viiwork/v2/internal/perf"
	"github.com/janit/viiwork/v2/internal/power"
	"github.com/janit/viiwork/v2/internal/proxy"
	"github.com/janit/viiwork/v2/meshapi"
)

// CostReader is the electricity cost; *cost.Tracker satisfies it.
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

// EnergyReader is the energy store's rolling totals.
type EnergyReader interface {
	KWh24h() float64
	KWh30d() float64
}

var (
	_ ipmiReader   = (*power.Sampler)(nil)
	_ gpuLatest    = (*gpu.History)(nil)
	_ CostReader   = (*cost.Tracker)(nil)
	_ EnergyReader = (*energy.Store)(nil)
)

// StatusSources is everything /v1/status is built from.
type StatusSources struct {
	Name      string
	NodeID    string
	Version   string
	APIPort   int
	Advertise func() netip.Addr
	Started   time.Time
	Models    func() []meshapi.ModelStatus // Supervisor.Status
	QueueLen  func(model string) int       // Router.QueueLen
	Counters  func(model string) proxy.ModelCounters
	Perf      func(model string) (perf.Score, bool)
	// EngineVersions is the version of each engine by id, as far as the node
	// knows; nil, or a nil map, is "not known". It must not block: a status
	// never waits for an engine's --version.
	EngineVersions func() map[string]string
	GPUs           gpuLatest // nil = none
	Inventory      []gpu.Identity
	Vendor         gpu.Vendor
	Power          NodePower
	Energy         EnergyReader // nil = none
	Cost           CostReader   // nil = none
	PromptHistory  int
	HostMemory     func() (totalMB, usedMB int64) // nil = hostinfo.HostMemoryMB
	Now            func() time.Time
}

// BuildStatus is this node's C4 NodeStatus.
func BuildStatus(s StatusSources) meshapi.NodeStatus {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	st := meshapi.NodeStatus{
		Node:          s.Name,
		NodeID:        s.NodeID,
		Ver:           s.Version,
		APIPort:       s.APIPort,
		UptimeS:       int64(now().Sub(s.Started) / time.Second),
		PromptHistory: s.PromptHistory,
		Models:        []meshapi.ModelStatus{},
	}
	if s.Advertise != nil {
		if a := s.Advertise(); a.IsValid() {
			st.Addr = a.String()
		}
	}
	memory := hostinfo.HostMemoryMB
	if s.HostMemory != nil {
		memory = s.HostMemory
	}
	st.HostMemTotalMB, st.HostMemUsedMB = memory()

	if s.GPUs != nil {
		names := map[int]gpu.Identity{}
		for _, id := range s.Inventory {
			names[id.Index] = id
		}
		samples := s.GPUs.Latest()
		sort.Slice(samples, func(i, j int) bool { return samples[i].GPUID < samples[j].GPUID })
		for _, sample := range samples {
			info := meshapi.GPUInfo{
				Index:       sample.GPUID,
				Util:        sample.Utilization,
				VRAMUsedMB:  sample.VRAMUsedMB,
				VRAMTotalMB: sample.VRAMTotalMB,
				PowerW:      sample.PowerW,
			}
			if id, ok := names[sample.GPUID]; ok {
				info.UUID, info.Name = id.UUID, id.Name
			}
			if s.Vendor != gpu.VendorNone {
				info.Vendor = string(s.Vendor)
			}
			st.GPUs = append(st.GPUs, info)
		}
	}

	if s.Models != nil {
		var versions map[string]string
		if s.EngineVersions != nil {
			versions = s.EngineVersions()
		}
		for _, m := range s.Models() {
			m.EngineName = engine.DisplayName(m.Engine)
			m.EngineVersion = publishableVersion(versions[m.Engine])
			if s.QueueLen != nil {
				m.Queued = s.QueueLen(m.Name)
			}
			if s.Counters != nil {
				c := s.Counters(m.Name)
				m.RequestsTotal, m.TokensTotal = c.Requests, c.Tokens
			}
			if s.Perf != nil {
				if sc, ok := s.Perf(m.Name); ok {
					m.Perf = &meshapi.PerfScore{OverheadMs: sc.OverheadMs, MsPer1k: sc.MsPer1k, Samples: sc.Samples, BaselineAgeS: int64(sc.BaselineAge / time.Second)}
				}
			}
			st.Models = append(st.Models, m)
		}
	}

	if s.Power != nil {
		st.Power = meshapi.PowerInfo{Watts: s.Power.Watts(), Available: s.Power.Available(), Source: s.Power.Source()}
	}
	if s.Energy != nil {
		st.EnergyKWh24h, st.EnergyKWh30d = s.Energy.KWh24h(), s.Energy.KWh30d()
	}
	if s.Cost != nil && s.Cost.Available() {
		st.Cost = meshapi.CostInfo{
			Available:  true,
			EURPerHour: s.Cost.EURPerHour(),
			TodayEUR:   s.Cost.TodayEUR(),
			Breakdown: &meshapi.CostBreakdown{
				SpotCentsKWh:     s.Cost.SpotCentsKWh(),
				TransferCentsKWh: s.Cost.TransferCentsKWh(),
				TaxCentsKWh:      s.Cost.TaxCentsKWh(),
				VATPercent:       s.Cost.VATPercent(),
				TotalCentsKWh:    s.Cost.TotalCentsKWh(),
			},
		}
	}
	return st
}

// versionRe is what an engine version may look like on a status: a build or
// a release number.
var versionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,39}$`)

// publishableVersion is v when it is a version and nothing else, and ""
// otherwise. A status is read by dashboards that are public, and what an
// engine prints after its number can be a path or a build host.
func publishableVersion(v string) string {
	if versionRe.MatchString(v) {
		return v
	}
	return ""
}
