// internal/peer/route_test.go
package peer

import (
	"testing"
	"time"

	"github.com/janit/viiwork/internal/balancer"
)

func TestPickRouteLocalPreferred(t *testing.T) {
	local := balancer.NewBackendState(0, "localhost:9001")
	local.SetStatus(balancer.StatusHealthy)
	local.RecordLatency(50*time.Millisecond, 30*time.Second)

	// Peer listed first to exercise the tiebreaker (local should still win)
	routes := []Route{
		{Type: RoutePeer, Addr: "192.168.1.10:8080", InFlight: 0},
		{Type: RouteLocal, Backend: local, InFlight: 0},
	}
	picked, err := PickRoute(routes, 4)
	if err != nil {
		t.Fatal(err)
	}
	if picked.Type != RouteLocal {
		t.Errorf("expected local route, got %s", picked.Type)
	}
}

func TestPickRoutePeerWhenLocalBusy(t *testing.T) {
	local := balancer.NewBackendState(0, "localhost:9001")
	local.SetStatus(balancer.StatusHealthy)
	local.RecordLatency(50*time.Millisecond, 30*time.Second)

	routes := []Route{
		{Type: RouteLocal, Backend: local, InFlight: 4},
		{Type: RoutePeer, Addr: "192.168.1.10:8080", InFlight: 0},
	}
	picked, err := PickRoute(routes, 4)
	if err != nil {
		t.Fatal(err)
	}
	if picked.Type != RoutePeer {
		t.Errorf("expected peer route, got %s", picked.Type)
	}
}

func TestPickRouteLeastLoaded(t *testing.T) {
	routes := []Route{
		{Type: RoutePeer, Addr: "192.168.1.10:8080", InFlight: 5},
		{Type: RoutePeer, Addr: "192.168.1.11:8080", InFlight: 1},
	}
	picked, err := PickRoute(routes, 4)
	if err != nil {
		t.Fatal(err)
	}
	if picked.Addr != "192.168.1.11:8080" {
		t.Errorf("expected least loaded peer, got %s", picked.Addr)
	}
}

func TestPickRouteNoRoutes(t *testing.T) {
	_, err := PickRoute(nil, 4)
	if err == nil {
		t.Error("expected error for no routes")
	}
}

func TestPickRouteAllAtCapacity(t *testing.T) {
	local := balancer.NewBackendState(0, "localhost:9001")
	local.SetStatus(balancer.StatusHealthy)
	routes := []Route{
		{Type: RouteLocal, Backend: local, InFlight: 4},
	}
	_, err := PickRoute(routes, 4)
	if err == nil {
		t.Error("expected error when all at capacity")
	}
}

// TestPickRouteTiedPeersRoundRobin guards the bug where two peers tied at
// equal InFlight always returned the slice-first peer, pinning all overflow
// burst traffic to one peer until its polled count finally moved.
func TestPickRouteTiedPeersRoundRobin(t *testing.T) {
	const n = 400
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		// New slice each iteration: PickRoute must not depend on routes ordering.
		routes := []Route{
			{Type: RoutePeer, Addr: "gb2:8096", InFlight: 0},
			{Type: RoutePeer, Addr: "gb4:8096", InFlight: 0},
			{Type: RoutePeer, Addr: "gb5:8096", InFlight: 0},
			{Type: RoutePeer, Addr: "gb6:8096", InFlight: 0},
		}
		picked, err := PickRoute(routes, 4)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		counts[picked.Addr]++
	}
	// Strict equality is fine because round-robin is deterministic with
	// atomic increments and four peers cleanly divide n=400.
	for _, addr := range []string{"gb2:8096", "gb4:8096", "gb5:8096", "gb6:8096"} {
		got := counts[addr]
		if got < n/4-5 || got > n/4+5 {
			t.Errorf("peer %s got %d picks, expected ~%d (round-robin among ties)", addr, got, n/4)
		}
	}
}

// TestPickRouteTiedPeersWithBusyLocal: when local is at capacity and two
// peers are tied at zero, rotation should still happen.
func TestPickRouteTiedPeersWithLocalAtCapacity(t *testing.T) {
	local := balancer.NewBackendState(0, "localhost:9001")
	local.SetStatus(balancer.StatusHealthy)
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		routes := []Route{
			{Type: RouteLocal, Backend: local, InFlight: 4},
			{Type: RoutePeer, Addr: "gb2:8096", InFlight: 0},
			{Type: RoutePeer, Addr: "gb4:8096", InFlight: 0},
		}
		picked, err := PickRoute(routes, 4)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		seen[picked.Addr] = true
	}
	if !seen["gb2:8096"] || !seen["gb4:8096"] {
		t.Errorf("expected round-robin to hit both peers, got %v", seen)
	}
}

// TestPickRouteLeastLoadedAcrossPeersBeatsRoundRobin: the tiebreaker must
// only activate at equal InFlight — least-loaded still wins overall.
func TestPickRouteLeastLoadedAcrossPeersBeatsRoundRobin(t *testing.T) {
	for i := 0; i < 50; i++ {
		routes := []Route{
			{Type: RoutePeer, Addr: "busy:8096", InFlight: 3},
			{Type: RoutePeer, Addr: "idle:8096", InFlight: 0},
		}
		picked, err := PickRoute(routes, 4)
		if err != nil {
			t.Fatal(err)
		}
		if picked.Addr != "idle:8096" {
			t.Fatalf("iteration %d: expected idle peer, got %s (InFlight=%d)", i, picked.Addr, picked.InFlight)
		}
	}
}
