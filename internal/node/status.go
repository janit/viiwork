package node

import (
	"bufio"
	"io"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/janit/viiwork/v2/energy"
	"github.com/janit/viiwork/v2/internal/cost"
	"github.com/janit/viiwork/v2/internal/gpu"
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
	Name          string
	NodeID        string
	Version       string
	APIPort       int
	Advertise     func() netip.Addr
	Started       time.Time
	Models        func() []meshapi.ModelStatus // Supervisor.Status
	QueueLen      func(model string) int       // Router.QueueLen
	Counters      func(model string) proxy.ModelCounters
	GPUs          gpuLatest // nil = none
	Inventory     []gpu.Identity
	Vendor        gpu.Vendor
	Power         NodePower
	Energy        EnergyReader // nil = none
	Cost          CostReader   // nil = none
	PromptHistory int
	HostMemory    func() (totalMB, usedMB int64) // nil = readHostMemory
	Now           func() time.Time
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
	memory := readHostMemory
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
		for _, m := range s.Models() {
			if s.QueueLen != nil {
				m.Queued = s.QueueLen(m.Name)
			}
			if s.Counters != nil {
				c := s.Counters(m.Name)
				m.RequestsTotal, m.TokensTotal = c.Requests, c.Tokens
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

// readHostMemory is the v1 proxy's reading of /proc/meminfo: used is total
// minus MemAvailable, the figure the RAM strip on /mesh renders.
func readHostMemory() (totalMB, usedMB int64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	return parseMemInfo(f)
}

// parseMemInfo is v1's readHostMemory loop, split from the file so it can be
// tested against a fixture.
func parseMemInfo(r io.Reader) (totalMB, usedMB int64) {
	var memTotal, memAvailable int64
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			memTotal = parseMemInfoKB(line)
		} else if strings.HasPrefix(line, "MemAvailable:") {
			memAvailable = parseMemInfoKB(line)
		}
		if memTotal > 0 && memAvailable > 0 {
			break
		}
	}
	totalMB = memTotal / 1024
	usedMB = (memTotal - memAvailable) / 1024
	return totalMB, usedMB
}

func parseMemInfoKB(line string) int64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, _ := strconv.ParseInt(fields[1], 10, 64)
	return v
}
