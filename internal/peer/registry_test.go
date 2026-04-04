package peer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/model"
)

func TestRegistryFindRoutesLocalOnly(t *testing.T) {
	backends := []*balancer.BackendState{balancer.NewBackendState(0, "localhost:9001")}
	backends[0].SetStatus(balancer.StatusHealthy)
	r := NewRegistry("viiwork-test", model.IDFromPath("/models/test-model.gguf"), backends, nil, 3*time.Second)
	routes := r.FindRoutesForModel("test-model")
	if len(routes) != 1 { t.Fatalf("expected 1 route, got %d", len(routes)) }
	if routes[0].Type != RouteLocal { t.Errorf("expected local route, got %s", routes[0].Type) }
}

func TestRegistryFindRoutesPeerModel(t *testing.T) {
	backends := []*balancer.BackendState{balancer.NewBackendState(0, "localhost:9001")}
	backends[0].SetStatus(balancer.StatusHealthy)
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(StatusResponse{NodeID: "viiwork-peer1", Models: []string{"other-model"}, TotalInFlight: 1, HealthyBackends: 2, TotalBackends: 2})
	}))
	defer peerSrv.Close()
	peers := []*PeerState{NewPeerState(peerSrv.Listener.Addr().String())}
	reg := NewRegistry("viiwork-test", model.IDFromPath("/models/test-model.gguf"), backends, peers, 3*time.Second)
	reg.PollOnce(context.Background())
	routes := reg.FindRoutesForModel("other-model")
	if len(routes) != 1 { t.Fatalf("expected 1 peer route, got %d", len(routes)) }
	if routes[0].Type != RoutePeer { t.Errorf("expected peer route, got %s", routes[0].Type) }
}

func TestRegistryFindRoutesModelNotFound(t *testing.T) {
	backends := []*balancer.BackendState{balancer.NewBackendState(0, "localhost:9001")}
	backends[0].SetStatus(balancer.StatusHealthy)
	reg := NewRegistry("viiwork-test", "test-model", backends, nil, 3*time.Second)
	routes := reg.FindRoutesForModel("nonexistent-model")
	if len(routes) != 0 { t.Errorf("expected 0 routes, got %d", len(routes)) }
}

func TestRegistryAllModels(t *testing.T) {
	backends := []*balancer.BackendState{balancer.NewBackendState(0, "localhost:9001")}
	backends[0].SetStatus(balancer.StatusHealthy)
	peer := NewPeerState("192.168.1.10:8080")
	peer.Update(StatusResponse{NodeID: "viiwork-peer1", Models: []string{"peer-model"}})
	reg := NewRegistry("viiwork-test", "local-model", backends, []*PeerState{peer}, 3*time.Second)
	models := reg.AllModels()
	if len(models) != 2 { t.Fatalf("expected 2 models, got %d", len(models)) }
	found := false
	for _, m := range models {
		if m.ID == "local-model" && m.OwnedBy == "local" { found = true }
	}
	if !found { t.Error("expected local-model with owned_by=local") }
}

func TestRegistryAllModelsDeduplicated(t *testing.T) {
	backends := []*balancer.BackendState{balancer.NewBackendState(0, "localhost:9001")}
	backends[0].SetStatus(balancer.StatusHealthy)
	peer := NewPeerState("192.168.1.10:8080")
	peer.Update(StatusResponse{NodeID: "viiwork-peer1", Models: []string{"same-model"}})
	reg := NewRegistry("viiwork-test", "same-model", backends, []*PeerState{peer}, 3*time.Second)
	models := reg.AllModels()
	if len(models) != 1 { t.Fatalf("expected 1 deduplicated model, got %d", len(models)) }
	if models[0].OwnedBy != "local" { t.Errorf("expected owned_by=local for deduplicated model, got %s", models[0].OwnedBy) }
}

func TestRegistrySelfDetection(t *testing.T) {
	selfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(StatusResponse{NodeID: "viiwork-self", Models: []string{"model-a"}})
	}))
	defer selfSrv.Close()
	peers := []*PeerState{NewPeerState(selfSrv.Listener.Addr().String())}
	reg := NewRegistry("viiwork-self", "model-a", nil, peers, 3*time.Second)
	reg.PollOnce(context.Background())
	if peers[0].Status() != StatusUnreachable { t.Errorf("expected self-peer to be unreachable, got %v", peers[0].Status()) }
}

func TestRegistryPeerGoesDown(t *testing.T) {
	peer := NewPeerState("127.0.0.1:1") // closed port, fast fail
	reg := NewRegistry("viiwork-test", "model-a", nil, []*PeerState{peer}, 1*time.Second)
	reg.PollOnce(context.Background())
	if peer.Status() != StatusUnreachable { t.Errorf("expected unreachable, got %v", peer.Status()) }
}
