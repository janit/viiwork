package proxy

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/gpu"
	"github.com/janit/viiwork/internal/peer"
	"github.com/janit/viiwork/internal/power"
)

// StatusLocation carries the hostname and listen addr this node publishes in
// /v1/status. Zero-value is fine; peers that leave it empty will fall back to
// the address the registry probes them at.
type StatusLocation struct {
	Hostname   string
	ListenAddr string
	// PromptHistory is this node's configured prompt-history capacity. It
	// travels on /v1/status so peers and the dashboard read the real number
	// instead of each keeping their own copy of it.
	PromptHistory int
}

// gpuLatest is the subset of *gpu.History the status payload needs. Kept as an
// interface so status.go does not depend on the gpu package and so tests can
// supply fixed samples.
type gpuLatest interface {
	Latest() []gpu.GPUSample
}

// statusGPUSource is set by SetStatusGPUSource. It is a package-level hook
// rather than a constructor argument because NewStatusHandler is called from
// NewMeshHandler before metrics are wired up in main.go, and threading it
// through would change that constructor's signature for every caller.
var statusGPUSource gpuLatest

// SetStatusGPUSource attaches the GPU history that /v1/status publishes to
// peers. Safe to leave unset: the gpus field is omitempty, and a node without
// rocm-smi simply reports no GPUs.
func SetStatusGPUSource(g gpuLatest) { statusGPUSource = g }

// statusEnergySource is the durable kWh store, attached the same way and for
// the same reason: the store is opened after the backends are up, long after
// the status handler is built.
var statusEnergySource peer.EnergyReader

// SetStatusEnergySource attaches the energy store that /v1/status and
// /v1/cluster publish. Safe to leave unset — the field is omitempty, and the
// store runs on one instance per host, so most nodes report nothing here.
func SetStatusEnergySource(e peer.EnergyReader) { statusEnergySource = e }

// statusPowerControl publishes the power-control allowlist on /v1/cluster.
var statusPowerControl *power.Controller

// SetStatusPowerControl attaches the controller whose allowlist /v1/cluster
// advertises. Separate from SetPowerControl on the handler because the cluster
// payload is built here, and a node that can control nothing must publish
// nothing rather than an empty list that reads as "enabled, no hosts".
func SetStatusPowerControl(c *power.Controller) { statusPowerControl = c }

func NewStatusHandler(nodeID string, localModel string, backends []*balancer.BackendState, power peer.PowerReader, cost peer.CostReader, loc StatusLocation) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := peer.StatusResponse{
			NodeID:        nodeID,
			Hostname:      loc.Hostname,
			ListenAddr:    loc.ListenAddr,
			Models:        []string{localModel},
			TotalBackends: len(backends),
			PromptHistory: loc.PromptHistory,
		}
		// Host RAM travels with the status payload so any node can render
		// memory pressure for every host, the same way GPU load already does.
		resp.HostMemTotalMB, resp.HostMemUsedMB = readHostMemory()
		if statusEnergySource != nil {
			resp.EnergyKWh24h = statusEnergySource.KWh24h()
			resp.EnergyKWh30d = statusEnergySource.KWh30d()
		}
		for _, b := range backends {
			var gpuIDs []int
			if len(b.GPUIDs) > 0 {
				gpuIDs = append(gpuIDs, b.GPUIDs...)
			}
			resp.Backends = append(resp.Backends, peer.BackendInfo{
				GPUID: b.GPUID, GPUIDs: gpuIDs, Model: localModel, Status: b.Status().String(), InFlight: b.InFlight(),
				RSSMB: b.RSSMB(), SlotCtx: b.SlotCtx(), SlotCount: b.SlotCount(),
				SlotActive: b.SlotActive(), TokDecoded: b.TokDecoded(), TokRemain: b.TokRemain(),
			})
			resp.TotalInFlight += b.InFlight()
			if b.Status() == balancer.StatusHealthy { resp.HealthyBackends++ }
		}
		if power != nil {
			resp.PowerWatts = power.Watts()
			resp.PowerAvailable = power.Available()
			// Optional interface rather than a widened PowerReader: reporting
			// the source is diagnostic, not something every reader must supply.
			if named, ok := power.(interface{ SourceName() string }); ok && resp.PowerAvailable {
				resp.PowerSource = named.SourceName()
			}
		}
		if cost != nil && cost.Available() {
			resp.CostAvailable = true
			resp.CostEURPerHour = cost.EURPerHour()
			resp.CostTodayEUR = cost.TodayEUR()
			resp.CostBreakdown = &peer.CostBreakdownJSON{
				SpotCentsKWh:     cost.SpotCentsKWh(),
				TransferCentsKWh: cost.TransferCentsKWh(),
				TaxCentsKWh:      cost.TaxCentsKWh(),
				VATPercent:       cost.VATPercent(),
				TotalCentsKWh:    cost.TotalCentsKWh(),
			}
		}
		if statusGPUSource != nil {
			for _, g := range statusGPUSource.Latest() {
				resp.GPUs = append(resp.GPUs, peer.GPUInfo{
					GPUID: g.GPUID, Util: g.Utilization,
					VRAMUsedMB: g.VRAMUsedMB, VRAMTotalMB: g.VRAMTotalMB,
				})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
}

// BuildClusterState assembles the full cluster snapshot: registry state plus
// the local-only extras (version, host memory, GPU load) that the registry has
// no access to. Shared by the /v1/cluster endpoint and the mesh push stream so
// both cannot drift.
func BuildClusterState(reg *peer.Registry) peer.ClusterResponse {
	state := reg.ClusterState()
	state.Version = Version
	totalMB, usedMB := readHostMemory()
	state.Local.HostMemTotalMB = totalMB
	state.Local.HostMemUsedMB = usedMB
	// Local energy is filled here rather than in Registry.ClusterState because
	// the store is wired to this package, not to the registry; peers' values
	// arrive through their own /v1/status and are set there.
	if statusEnergySource != nil {
		state.Local.EnergyKWh24h = statusEnergySource.KWh24h()
		state.Local.EnergyKWh30d = statusEnergySource.KWh30d()
	}
	if statusPowerControl != nil && statusPowerControl.Enabled() {
		info := &peer.PowerControlInfo{Hosts: statusPowerControl.Hosts()}
		for _, h := range info.Hosts {
			if statusPowerControl.HasBMC(h) {
				info.OutOfBand = append(info.OutOfBand, h)
			}
		}
		state.PowerControl = info
	}
	if statusGPUSource != nil {
		for _, g := range statusGPUSource.Latest() {
			state.Local.GPUs = append(state.Local.GPUs, peer.GPUInfo{
				GPUID: g.GPUID, Util: g.Utilization,
				VRAMUsedMB: g.VRAMUsedMB, VRAMTotalMB: g.VRAMTotalMB,
			})
		}
	}
	return state
}

func NewClusterHandler(reg *peer.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(BuildClusterState(reg))
	})
}

// readHostMemory reads /proc/meminfo and returns total and used memory in MB.
func readHostMemory() (totalMB, usedMB int64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	var memTotal, memAvailable int64
	scanner := bufio.NewScanner(f)
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
