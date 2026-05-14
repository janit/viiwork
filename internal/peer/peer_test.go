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

func TestPeerStateLocalInFlight(t *testing.T) {
	p := NewPeerState("192.168.1.10:8080")
	p.Update(StatusResponse{NodeID: "viiwork-x", Models: []string{"m"}, TotalInFlight: 2})

	// max(polled=2, local=0) == 2
	if got := p.TotalInFlight(); got != 2 {
		t.Fatalf("expected 2 from polled, got %d", got)
	}

	// Dispatch three requests this node has not yet seen reflected by the
	// peer poll. TotalInFlight should reflect the larger local count.
	p.IncLocalInFlight()
	p.IncLocalInFlight()
	p.IncLocalInFlight()
	if got := p.TotalInFlight(); got != 3 {
		t.Fatalf("expected 3 from local write-through, got %d (polled=2 local=%d)", got, p.LocalInFlight())
	}

	// As responses come back, local drains; polled (older) wins again.
	p.DecLocalInFlight()
	p.DecLocalInFlight()
	if got := p.TotalInFlight(); got != 2 {
		t.Fatalf("expected 2 from polled after drain, got %d", got)
	}

	// A fresh poll bumps the polled count; max still works.
	p.Update(StatusResponse{NodeID: "viiwork-x", Models: []string{"m"}, TotalInFlight: 7})
	if got := p.TotalInFlight(); got != 7 {
		t.Fatalf("expected 7 after poll, got %d", got)
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
