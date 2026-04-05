package proxy

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/peer"
)

func NewStatusHandler(nodeID string, localModel string, backends []*balancer.BackendState, power peer.PowerReader, cost peer.CostReader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := peer.StatusResponse{
			NodeID:        nodeID,
			Models:        []string{localModel},
			TotalBackends: len(backends),
		}
		for _, b := range backends {
			resp.Backends = append(resp.Backends, peer.BackendInfo{
				GPUID: b.GPUID, Model: localModel, Status: b.Status().String(), InFlight: b.InFlight(),
			})
			resp.TotalInFlight += b.InFlight()
			if b.Status() == balancer.StatusHealthy { resp.HealthyBackends++ }
		}
		if power != nil {
			resp.PowerWatts = power.Watts()
			resp.PowerAvailable = power.Available()
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
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
}

func NewClusterHandler(reg *peer.Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := reg.ClusterState()
		state.Version = Version
		totalMB, usedMB := readHostMemory()
		state.Local.HostMemTotalMB = totalMB
		state.Local.HostMemUsedMB = usedMB
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(state)
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
