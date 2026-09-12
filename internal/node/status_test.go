package node

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/gpu"
	"github.com/janit/viiwork/v2/internal/proxy"
	"github.com/janit/viiwork/v2/meshapi"
)

type fakeEnergy struct{ day, month float64 }

func (f fakeEnergy) KWh24h() float64 { return f.day }
func (f fakeEnergy) KWh30d() float64 { return f.month }

type fakeCost struct{ available bool }

func (f fakeCost) Available() bool           { return f.available }
func (f fakeCost) EURPerHour() float64       { return 0.25 }
func (f fakeCost) TodayEUR() float64         { return 3.5 }
func (f fakeCost) SpotCentsKWh() float64     { return 4.1 }
func (f fakeCost) TransferCentsKWh() float64 { return 3.2 }
func (f fakeCost) TaxCentsKWh() float64      { return 2.8 }
func (f fakeCost) VATPercent() float64       { return 25.5 }
func (f fakeCost) TotalCentsKWh() float64    { return 12.7 }

func statusFixture() StatusSources {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	return StatusSources{
		Name:      "gb1",
		NodeID:    "viiwork-0123456789abcdef",
		Version:   "v2.0.0-test",
		APIPort:   8086,
		Advertise: func() netip.Addr { return netip.MustParseAddr("100.64.0.1") },
		Started:   now.Add(-90 * time.Second),
		Models: func() []meshapi.ModelStatus {
			return []meshapi.ModelStatus{
				{Name: "a", Engine: "llamacpp", Slots: 2, Backends: []meshapi.BackendStatus{{ID: "a/0", Status: "healthy", Slots: 2}}},
				{Name: "b", Engine: "llamacpp", Slots: 1, Backends: []meshapi.BackendStatus{{ID: "b/0", Status: "healthy", Slots: 1}}},
			}
		},
		QueueLen: func(m string) int {
			if m == "a" {
				return 2
			}
			return 0
		},
		Counters: func(m string) proxy.ModelCounters {
			if m == "a" {
				return proxy.ModelCounters{Requests: 5, Tokens: 120}
			}
			return proxy.ModelCounters{}
		},
		GPUs:          fakeGPUs{{GPUID: 1, Utilization: 10, VRAMUsedMB: 100, VRAMTotalMB: 16000, PowerW: 20}, {GPUID: 0, Utilization: 90, VRAMUsedMB: 15000, VRAMTotalMB: 16000, PowerW: 150}},
		Inventory:     []gpu.Identity{{Index: 0, UUID: "GPU-aaaa", Name: "Test Card"}},
		Vendor:        gpu.VendorNVIDIA,
		Power:         NewNodePower(&fakeIPMI{watts: 412, available: true, source: "dcmi"}, nil, gpu.VendorNVIDIA, false),
		Energy:        fakeEnergy{1.5, 40.2},
		Cost:          fakeCost{available: true},
		PromptHistory: 1000,
		HostMemory:    func() (int64, int64) { return 64000, 12000 },
		Now:           func() time.Time { return now },
	}
}

func TestBuildStatusFull(t *testing.T) {
	st := BuildStatus(statusFixture())
	if st.Node != "gb1" || st.NodeID != "viiwork-0123456789abcdef" || st.Ver != "v2.0.0-test" || st.Addr != "100.64.0.1" || st.APIPort != 8086 || st.UptimeS != 90 {
		t.Errorf("S1 identity: %+v", st)
	}
	if st.HostMemTotalMB != 64000 || st.HostMemUsedMB != 12000 || st.PromptHistory != 1000 {
		t.Errorf("S1 memory/history: %+v", st)
	}
	if len(st.Models) != 2 || st.Models[0].Queued != 2 || st.Models[0].RequestsTotal != 5 || st.Models[0].TokensTotal != 120 || st.Models[1].Queued != 0 {
		t.Errorf("S1 models: %+v", st.Models)
	}
	if len(st.GPUs) != 2 || st.GPUs[0].Index != 0 || st.GPUs[0].UUID != "GPU-aaaa" || st.GPUs[0].Name != "Test Card" || st.GPUs[0].Util != 90 || st.GPUs[0].PowerW != 150 ||
		st.GPUs[1].Index != 1 || st.GPUs[1].UUID != "" || st.GPUs[0].Vendor != "nvidia" || st.GPUs[1].Vendor != "nvidia" {
		t.Errorf("S1 GPUs: %+v", st.GPUs)
	}
	if st.Power != (meshapi.PowerInfo{Watts: 412, Available: true, Source: "dcmi"}) {
		t.Errorf("S1 power: %+v", st.Power)
	}
	if st.EnergyKWh24h != 1.5 || st.EnergyKWh30d != 40.2 {
		t.Errorf("S1 energy: %v %v", st.EnergyKWh24h, st.EnergyKWh30d)
	}
	if !st.Cost.Available || st.Cost.EURPerHour != 0.25 || st.Cost.TodayEUR != 3.5 || st.Cost.Breakdown == nil || st.Cost.Breakdown.VATPercent != 25.5 || st.Cost.Breakdown.TotalCentsKWh != 12.7 {
		t.Errorf("S1 cost: %+v", st.Cost)
	}
}

func TestBuildStatusOptional(t *testing.T) {
	s := statusFixture()
	s.Energy, s.Cost = nil, nil
	raw, _ := json.Marshal(BuildStatus(s))
	if strings.Contains(string(raw), "energy_kwh_24h") || !strings.Contains(string(raw), `"cost":{"available":false}`) {
		t.Errorf("S2: %s", raw)
	}

	s = statusFixture()
	s.Advertise = func() netip.Addr { return netip.Addr{} }
	s.Cost = fakeCost{available: false}
	if st := BuildStatus(s); st.Addr != "" || st.Cost.Available || st.Cost.Breakdown != nil {
		t.Errorf("S3: addr=%q cost=%+v", st.Addr, st.Cost)
	}

	s = statusFixture()
	s.Vendor, s.GPUs = gpu.VendorNone, nil
	if st := BuildStatus(s); len(st.GPUs) != 0 {
		t.Errorf("no GPUs: %+v", st.GPUs)
	}
}

func TestReadHostMemory(t *testing.T) {
	meminfo := "MemTotal:       65536000 kB\nMemFree:         1000000 kB\nMemAvailable:   16384000 kB\n"
	if total, used := parseMemInfo(strings.NewReader(meminfo)); total != 64000 || used != 48000 {
		t.Errorf("S4: total=%d used=%d", total, used)
	}
}
