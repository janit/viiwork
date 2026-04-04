package proxy

import (
	"encoding/json"
	"net/http"

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
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(state)
	})
}
