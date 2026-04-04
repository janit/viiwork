package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/peer"
)

func TestStatusEndpoint(t *testing.T) {
	backends := []*balancer.BackendState{
		balancer.NewBackendState(0, "localhost:9001"),
		balancer.NewBackendState(1, "localhost:9002"),
	}
	backends[0].SetStatus(balancer.StatusHealthy)
	backends[1].SetStatus(balancer.StatusHealthy)
	backends[0].IncrInFlight()

	h := NewStatusHandler("viiwork-test", "test-model", backends, nil, nil)
	req := httptest.NewRequest("GET", "/v1/status", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 { t.Fatalf("expected 200, got %d", w.Code) }
	var resp peer.StatusResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.NodeID != "viiwork-test" { t.Errorf("expected node_id viiwork-test, got %s", resp.NodeID) }
	if len(resp.Models) != 1 || resp.Models[0] != "test-model" { t.Errorf("expected [test-model], got %v", resp.Models) }
	if resp.TotalInFlight != 1 { t.Errorf("expected 1 in-flight, got %d", resp.TotalInFlight) }
	if resp.HealthyBackends != 2 { t.Errorf("expected 2 healthy, got %d", resp.HealthyBackends) }
}

type mockPowerReader struct {
	watts     float64
	available bool
}

func (m *mockPowerReader) Watts() float64  { return m.watts }
func (m *mockPowerReader) Available() bool { return m.available }

func TestStatusEndpointWithPower(t *testing.T) {
	backends := []*balancer.BackendState{balancer.NewBackendState(0, "localhost:9001")}
	backends[0].SetStatus(balancer.StatusHealthy)
	pw := &mockPowerReader{watts: 280.0, available: true}
	h := NewStatusHandler("viiwork-test", "test-model", backends, pw, nil)
	req := httptest.NewRequest("GET", "/v1/status", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var resp peer.StatusResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.PowerWatts != 280.0 { t.Errorf("expected 280.0, got %f", resp.PowerWatts) }
	if !resp.PowerAvailable { t.Error("expected power_available true") }
}

func TestStatusEndpointNoPower(t *testing.T) {
	backends := []*balancer.BackendState{balancer.NewBackendState(0, "localhost:9001")}
	backends[0].SetStatus(balancer.StatusHealthy)
	h := NewStatusHandler("viiwork-test", "test-model", backends, nil, nil)
	req := httptest.NewRequest("GET", "/v1/status", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var resp peer.StatusResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.PowerWatts != 0 { t.Errorf("expected 0, got %f", resp.PowerWatts) }
	if resp.PowerAvailable { t.Error("expected power_available false") }
}

type mockCostReader struct{}

func (m *mockCostReader) Available() bool           { return true }
func (m *mockCostReader) EURPerHour() float64       { return 0.42 }
func (m *mockCostReader) TodayEUR() float64         { return 3.85 }
func (m *mockCostReader) SpotCentsKWh() float64     { return 5.0 }
func (m *mockCostReader) TransferCentsKWh() float64 { return 4.28 }
func (m *mockCostReader) TaxCentsKWh() float64      { return 2.253 }
func (m *mockCostReader) VATPercent() float64       { return 25.5 }
func (m *mockCostReader) TotalCentsKWh() float64    { return 14.47 }

func TestStatusEndpointWithCost(t *testing.T) {
	backends := []*balancer.BackendState{balancer.NewBackendState(0, "localhost:9001")}
	backends[0].SetStatus(balancer.StatusHealthy)
	pw := &mockPowerReader{watts: 280.0, available: true}
	cr := &mockCostReader{}
	h := NewStatusHandler("viiwork-test", "test-model", backends, pw, cr)
	req := httptest.NewRequest("GET", "/v1/status", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var resp peer.StatusResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.CostAvailable { t.Error("expected cost_available true") }
	if resp.CostEURPerHour != 0.42 { t.Errorf("expected 0.42, got %f", resp.CostEURPerHour) }
	if resp.CostBreakdown == nil { t.Fatal("expected cost_breakdown") }
	if resp.CostBreakdown.SpotCentsKWh != 5.0 { t.Errorf("expected spot 5.0, got %f", resp.CostBreakdown.SpotCentsKWh) }
}

func TestClusterEndpoint(t *testing.T) {
	backends := []*balancer.BackendState{balancer.NewBackendState(0, "localhost:9001")}
	backends[0].SetStatus(balancer.StatusHealthy)
	reg := peer.NewRegistry("viiwork-test", "test-model", backends, nil, 3*time.Second)
	h := NewClusterHandler(reg)
	req := httptest.NewRequest("GET", "/v1/cluster", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 { t.Fatalf("expected 200, got %d", w.Code) }
	var resp peer.ClusterResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.NodeID != "viiwork-test" { t.Errorf("expected viiwork-test, got %s", resp.NodeID) }
	if resp.Local.Model != "test-model" { t.Errorf("expected test-model, got %s", resp.Local.Model) }
}
