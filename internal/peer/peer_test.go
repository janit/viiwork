// internal/peer/peer_test.go
package peer

import "testing"

func TestNewPeerState(t *testing.T) {
	p := NewPeerState("192.168.1.10:8080")
	if p.Addr != "192.168.1.10:8080" { t.Errorf("expected addr 192.168.1.10:8080, got %s", p.Addr) }
	if p.Status() != StatusUnreachable { t.Errorf("expected unreachable, got %v", p.Status()) }
}

func TestPeerStateUpdate(t *testing.T) {
	p := NewPeerState("192.168.1.10:8080")
	p.Update(StatusResponse{
		NodeID: "viiwork-abc123", Models: []string{"gpt-oss-20b-Q4_K_M"},
		TotalInFlight: 3, HealthyBackends: 4, TotalBackends: 5,
	})
	if p.Status() != StatusReachable { t.Errorf("expected reachable, got %v", p.Status()) }
	if p.NodeID() != "viiwork-abc123" { t.Errorf("expected node id viiwork-abc123, got %s", p.NodeID()) }
	if len(p.Models()) != 1 || p.Models()[0] != "gpt-oss-20b-Q4_K_M" { t.Errorf("expected [gpt-oss-20b-Q4_K_M], got %v", p.Models()) }
	if p.TotalInFlight() != 3 { t.Errorf("expected 3 in-flight, got %d", p.TotalInFlight()) }
}

func TestPeerStateMarkUnreachable(t *testing.T) {
	p := NewPeerState("192.168.1.10:8080")
	p.Update(StatusResponse{NodeID: "viiwork-abc123", Models: []string{"model-a"}})
	p.MarkUnreachable()
	if p.Status() != StatusUnreachable { t.Errorf("expected unreachable, got %v", p.Status()) }
	if len(p.Models()) != 0 { t.Errorf("expected no models when unreachable, got %v", p.Models()) }
}

func TestPeerStatePowerFields(t *testing.T) {
	p := NewPeerState("192.168.1.10:8080")
	p.Update(StatusResponse{
		NodeID:         "viiwork-abc123",
		Models:         []string{"model-a"},
		PowerWatts:     280.0,
		PowerAvailable: true,
	})
	if p.PowerWatts() != 280.0 {
		t.Errorf("expected 280.0, got %f", p.PowerWatts())
	}
	if !p.PowerAvailable() {
		t.Error("expected PowerAvailable = true")
	}
}

func TestPeerStatePowerUnavailable(t *testing.T) {
	p := NewPeerState("192.168.1.10:8080")
	p.Update(StatusResponse{
		NodeID:         "viiwork-abc123",
		Models:         []string{"model-a"},
		PowerWatts:     0,
		PowerAvailable: false,
	})
	if p.PowerWatts() != 0 {
		t.Errorf("expected 0, got %f", p.PowerWatts())
	}
	if p.PowerAvailable() {
		t.Error("expected PowerAvailable = false")
	}
}

func TestPeerStateCostFields(t *testing.T) {
	p := NewPeerState("192.168.1.10:8080")
	p.Update(StatusResponse{
		NodeID: "viiwork-abc123", Models: []string{"model-a"},
		CostAvailable: true, CostEURPerHour: 0.42, CostTodayEUR: 3.85,
	})
	if !p.CostAvailable() { t.Error("expected cost available") }
	if p.CostEURPerHour() != 0.42 { t.Errorf("expected 0.42, got %f", p.CostEURPerHour()) }
	if p.CostTodayEUR() != 3.85 { t.Errorf("expected 3.85, got %f", p.CostTodayEUR()) }
}
