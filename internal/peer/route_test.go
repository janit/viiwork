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
