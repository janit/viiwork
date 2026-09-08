package peer

import (
	"context"
	"fmt"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/meshauth"
	"github.com/janit/viiwork/internal/model"
	"github.com/janit/viiwork/meshapi"
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

// GPUModels is what makes energy attribution useful on a multi-model host: the
// recording instance owns a fraction of the cards and has to learn the rest
// from the co-located instances it already polls.
func TestRegistryGPUModelsCoLocatedPeers(t *testing.T) {
	local := balancer.NewBackendState(2, "localhost:9801")
	reg := NewRegistry("viiwork-granite", "granite-4.1-8b", []*balancer.BackendState{local}, nil, 3*time.Second)
	reg.SetLocation("gb1", "0.0.0.0:9102")

	sameHost := NewPeerState("127.0.0.1:9302")
	sameHost.Update(StatusResponse{NodeID: "viiwork-qwen", Hostname: "gb1", Backends: []BackendInfo{
		{GPUID: 0, GPUIDs: []int{0, 1}, Model: "qwen3.8-27b"},
	}})
	otherHost := NewPeerState("192.168.1.63:9101")
	otherHost.Update(StatusResponse{NodeID: "viiwork-gb0", Hostname: "gb0", Backends: []BackendInfo{
		{GPUID: 5, Model: "should-not-appear"},
	}})
	reg.peers.Store(buildPeerSet([]*PeerState{sameHost, otherHost}))

	got := reg.GPUModels()
	want := map[int]string{0: "qwen3.8-27b", 1: "qwen3.8-27b", 2: "granite-4.1-8b"}
	if len(got) != len(want) { t.Fatalf("expected %d labelled GPUs, got %d: %v", len(want), len(got), got) }
	for id, model := range want {
		if got[id] != model { t.Errorf("gpu %d: expected %q, got %q", id, model, got[id]) }
	}
}

// An instance that has stopped serving leaves its cards idle, so labelling
// them from its last-known status would charge energy to a model that is not
// running.
func TestRegistryGPUModelsSkipsUnreachableAndUnknownHost(t *testing.T) {
	reg := NewRegistry("viiwork-granite", "granite-4.1-8b", nil, nil, 3*time.Second)
	reg.SetLocation("gb1", "0.0.0.0:9102")

	dead := NewPeerState("127.0.0.1:9302")
	dead.Update(StatusResponse{NodeID: "viiwork-qwen", Hostname: "gb1", Backends: []BackendInfo{{GPUID: 0, Model: "qwen3.8-27b"}}})
	dead.MarkUnreachable()
	// A pre-v1.0 peer omits hostname entirely; it cannot be assumed local.
	noHostname := NewPeerState("127.0.0.1:9404")
	noHostname.Update(StatusResponse{NodeID: "viiwork-old", Backends: []BackendInfo{{GPUID: 3, Model: "translategemma"}}})
	reg.peers.Store(buildPeerSet([]*PeerState{dead, noHostname}))

	if got := reg.GPUModels(); len(got) != 0 { t.Errorf("expected no labels, got %v", got) }
}

// The local instance is authoritative for its own cards: a peer entry that
// points back at this node must not relabel them.
func TestRegistryGPUModelsLocalWins(t *testing.T) {
	local := balancer.NewBackendState(2, "localhost:9801")
	reg := NewRegistry("viiwork-granite", "granite-4.1-8b", []*balancer.BackendState{local}, nil, 3*time.Second)
	reg.SetLocation("gb1", "0.0.0.0:9102")

	self := NewPeerState("127.0.0.1:9102")
	self.Update(StatusResponse{NodeID: "viiwork-granite", Hostname: "gb1", Backends: []BackendInfo{{GPUID: 2, Model: "stale-name"}}})
	reg.peers.Store(buildPeerSet([]*PeerState{self}))

	if got := reg.GPUModels()[2]; got != "granite-4.1-8b" { t.Errorf("expected local model to win, got %q", got) }
}

// A backend with no GPU assigned yet must not create a label for gpu -1.
func TestRegistryGPUModelsDropsUnassigned(t *testing.T) {
	reg := NewRegistry("viiwork-test", "local-model", []*balancer.BackendState{balancer.NewBackendState(-1, "localhost:9001")}, nil, 3*time.Second)
	reg.SetLocation("gb1", "0.0.0.0:8080")
	if got := reg.GPUModels(); len(got) != 0 { t.Errorf("expected no labels for unassigned gpu, got %v", got) }
}

// A co-located instance is configured by IP, so deriving its hostname from the
// dial address splits one machine into two on the cluster view -- and makes a
// per-host wattage impossible to deduplicate.
func TestClusterStatePrefersReportedHostname(t *testing.T) {
	reg := NewRegistry("viiwork-granite", "granite-4.1-8b", nil, nil, 3*time.Second)
	reg.SetLocation("gb1", "gb1:9102")

	coLocated := NewPeerState("192.168.1.41:9302")
	coLocated.Update(StatusResponse{NodeID: "viiwork-qwen", Hostname: "gb1", Models: []string{"qwen3.8-27b"}})
	// A peer too old to report a hostname still has to land somewhere.
	legacy := NewPeerState("192.168.1.63:9101")
	legacy.Update(StatusResponse{NodeID: "viiwork-old", Models: []string{"granite"}})
	reg.peers.Store(buildPeerSet([]*PeerState{coLocated, legacy}))

	state := reg.ClusterState()
	if got := state.Peers[0].Hostname; got != "gb1" {
		t.Errorf("co-located peer: expected reported hostname gb1, got %q", got)
	}
	if got := state.Peers[1].Hostname; got != "192.168.1.63" {
		t.Errorf("legacy peer: expected address fallback, got %q", got)
	}
}

// power_source is diagnostic: the reading is board-specific and probed at
// startup, so the mesh view has to be able to say which one a host adopted.
func TestClusterStateCarriesPowerSource(t *testing.T) {
	reg := NewRegistry("viiwork-granite", "granite-4.1-8b", nil, nil, 3*time.Second)
	reg.SetLocation("gb1", "gb1:9102")
	p := NewPeerState("192.168.1.63:9101")
	p.Update(StatusResponse{NodeID: "viiwork-gb0", Hostname: "gb0", PowerWatts: 210, PowerAvailable: true, PowerSource: "dcmi"})
	reg.peers.Store(buildPeerSet([]*PeerState{p}))

	state := reg.ClusterState()
	if got := state.Peers[0].PowerSource; got != "dcmi" {
		t.Errorf("expected power_source dcmi, got %q", got)
	}
	// An unreachable peer must not keep publishing a stale source alongside a
	// zeroed wattage.
	p.MarkUnreachable()
	if got := reg.ClusterState().Peers[0].PowerSource; got != "" {
		t.Errorf("expected no power_source when unreachable, got %q", got)
	}
}

func TestPeerSetIsSafeUnderConcurrentReadsAndAdds(t *testing.T) {
	reg := NewRegistry("gb1-a1b2", "test-model", nil, []*PeerState{NewPeerState("100.64.0.11:9100")}, time.Second)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			reg.addPeers(NewLearnedPeerState(fmt.Sprintf("100.64.0.%d:9100", i%200+20), ""))
		}
	}()

	for i := 0; i < 500; i++ {
		reg.FindRoutesForModel("test-model")
		reg.AllModels()
		reg.ClusterState()
		reg.IsKnownPeer("gb2-c3d4")
	}
	<-done

	if got := len(reg.Peers()); got < 2 {
		t.Fatalf("peer count = %d, want the seed plus learned peers", got)
	}
}

func TestPollPeerMarksVerifiedOnASignedResponse(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	local, _ := meshauth.NewSigner(secret, "gb1-a1b2")
	remote, _ := meshauth.NewSigner(secret, "gb2-c3d4")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce, _, err := remote.VerifyRequest(r, nil)
		body, _ := json.Marshal(StatusResponse{NodeID: "gb2-c3d4", Models: []string{"m1"}})
		if err == nil {
			remote.SignResponse(w.Header(), r.URL.RequestURI(), nonce, body)
		}
		w.Write(body)
	}))
	defer srv.Close()

	p := NewPeerState(srv.Listener.Addr().String())
	reg := NewRegistry("gb1-a1b2", "local-model", nil, []*PeerState{p}, 2*time.Second)
	reg.SetSigner(local)

	reg.PollOnce(context.Background())

	if p.Status() != StatusReachable {
		t.Fatal("peer should be reachable")
	}
	if !p.Verified() {
		t.Fatal("peer returned a valid proof and should be verified")
	}
}

func TestPollPeerLeavesAnUnsignedPeerReachableButUnverified(t *testing.T) {
	// An older build in a mixed fleet: it answers, it is routable because it
	// is configured, and it is simply never a source of gossip.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(StatusResponse{NodeID: "gb9-old1", Models: []string{"m9"}})
	}))
	defer srv.Close()

	secret := []byte("0123456789abcdef0123456789abcdef")
	local, _ := meshauth.NewSigner(secret, "gb1-a1b2")

	p := NewPeerState(srv.Listener.Addr().String())
	reg := NewRegistry("gb1-a1b2", "local-model", nil, []*PeerState{p}, 2*time.Second)
	reg.SetSigner(local)

	reg.PollOnce(context.Background())

	if p.Status() != StatusReachable {
		t.Fatal("an unsigned peer must still be reachable")
	}
	if p.Verified() {
		t.Fatal("an unsigned peer must not be verified")
	}
	if !p.Routable() {
		t.Fatal("a configured peer must be routable without a proof")
	}
}

// darkPeers stands up n servers that accept a connection and never answer.
// That is what a powered-off host looks like to the poller — the handshake
// completes (here) or hangs (there), and either way the request runs
// peers.timeout out in full. A closed port would fail instantly and prove
// nothing. Handlers return when the poller gives up, and release is closed
// before the servers so Close never waits on one.
func darkPeers(t *testing.T, n int, onRequest func(*http.Request)) []*PeerState {
	t.Helper()
	release := make(chan struct{})
	var peers []*PeerState
	for i := 0; i < n; i++ {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if onRequest != nil {
				onRequest(r)
			}
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		t.Cleanup(srv.Close)
		peers = append(peers, NewPeerState(srv.Listener.Addr().String()))
	}
	t.Cleanup(func() { close(release) })
	return peers
}

// A round has to cost about one timeout however many peers are dark. Polled
// in sequence, eight unreachable peers at a 5s timeout made a 40s round
// against a 10s interval, and for that whole window a node that had died
// kept its routes and its frozen, attractive in-flight count.
func TestPollOnceIsConcurrent(t *testing.T) {
	const timeout = 300 * time.Millisecond
	peers := darkPeers(t, 8, nil)
	reg := NewRegistry("gb1-a1b2", "local-model", nil, peers, timeout)

	start := time.Now()
	reg.PollOnce(context.Background())
	took := time.Since(start)

	if limit := 3 * timeout; took > limit {
		t.Fatalf("PollOnce took %s for %d unreachable peers (limit %s): peers appear to be polled sequentially, so one round costs peers x timeout", took, len(peers), limit)
	}
	for _, p := range peers {
		if p.Status() != StatusUnreachable {
			t.Errorf("peer %s: expected unreachable, got %v", p.Addr, p.Status())
		}
	}
}

// A verified peer keeps its standing when it goes dark (see
// TestALearnedPeerIsKeptWhenItStopsAnswering), so without this check every
// full discovery round would cluster-poll it and run the timeout out again,
// in sequence: the PollOnce stall, back once every DiscoveryEvery rounds.
func TestDiscoveryRoundSkipsUnreachablePeers(t *testing.T) {
	var clusterPolls atomic.Int32
	peers := darkPeers(t, 1, func(r *http.Request) {
		if r.URL.Path == meshapi.PathCluster {
			clusterPolls.Add(1)
		}
	})
	p := peers[0]
	p.MarkVerified() // proved membership once; its host has since gone dark

	reg := NewRegistry("gb1-a1b2", "local-model", nil, peers, 300*time.Millisecond)
	signer, err := meshauth.NewSigner([]byte("0123456789abcdef0123456789abcdef"), "gb1-a1b2")
	if err != nil {
		t.Fatal(err)
	}
	reg.SetSigner(signer)
	reg.SetGossip(GossipOptions{Enabled: true, DiscoveryEvery: 1})

	reg.PollOnce(context.Background())

	if p.Status() != StatusUnreachable {
		t.Fatalf("expected unreachable, got %v", p.Status())
	}
	if !p.Verified() {
		t.Fatal("verification is monotonic and must survive the failed poll")
	}
	if n := clusterPolls.Load(); n != 0 {
		t.Fatalf("cluster polled %d time(s) on a peer whose status poll just failed; discovery should skip it", n)
	}
}
