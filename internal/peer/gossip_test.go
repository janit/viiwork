package peer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/meshauth"
	"github.com/janit/viiwork/meshapi"
)

// meshNode is a fake viiwork node: it answers /v1/status and /v1/cluster,
// signs when it can, and reports whatever peers the test gives it.
type meshNode struct {
	*httptest.Server
	signer *meshauth.Signer
	nodeID string
	peers  []ClusterPeerInfo
}

func newMeshNode(t *testing.T, nodeID string, secret []byte) *meshNode {
	t.Helper()
	n := &meshNode{nodeID: nodeID}
	if secret != nil {
		s, err := meshauth.NewSigner(secret, nodeID)
		if err != nil {
			t.Fatal(err)
		}
		n.signer = s
	}
	n.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		switch r.URL.Path {
		case meshapi.PathStatus:
			body, _ = json.Marshal(StatusResponse{NodeID: n.nodeID, Hostname: n.nodeID, Models: []string{"m-" + n.nodeID}})
		case meshapi.PathCluster:
			body, _ = json.Marshal(ClusterResponse{NodeID: n.nodeID, Peers: n.peers})
		default:
			http.NotFound(w, r)
			return
		}
		if n.signer != nil {
			if nonce, _, err := n.signer.VerifyRequest(r, nil); err == nil {
				n.signer.SignResponse(w.Header(), r.URL.RequestURI(), nonce, body)
			}
		}
		w.Write(body)
	}))
	t.Cleanup(n.Close)
	return n
}

func (n *meshNode) addr() string { return n.Listener.Addr().String() }

func testRegistry(t *testing.T, secret []byte, seeds ...string) *Registry {
	t.Helper()
	var ps []*PeerState
	for _, s := range seeds {
		ps = append(ps, NewPeerState(s))
	}
	reg := NewRegistry("gb1-a1b2", "local-model", nil, ps, 2*time.Second)
	signer, err := meshauth.NewSigner(secret, "gb1-a1b2")
	if err != nil {
		t.Fatal(err)
	}
	reg.SetSigner(signer)
	reg.SetGossip(GossipOptions{Enabled: true, DiscoveryEvery: 1, MaxLearnedPeers: 200})
	reg.skipAddrValidation = true // httptest binds loopback; see the field's doc
	return reg
}

func TestAdoptsPeersFromAVerifiedClusterResponse(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	c := newMeshNode(t, "gb3-e5f6", secret)
	b := newMeshNode(t, "gb2-c3d4", secret)
	b.peers = []ClusterPeerInfo{{Addr: c.addr(), Hostname: "gb3", Status: "reachable"}}

	reg := testRegistry(t, secret, b.addr())
	reg.PollOnce(context.Background()) // learns C from B, polls it

	found := false
	for _, p := range reg.Peers() {
		if p.Addr == c.addr() {
			found = true
			if p.Origin() != OriginLearned {
				t.Errorf("origin = %q, want learned", p.Origin())
			}
		}
	}
	if !found {
		t.Fatal("C was advertised by a verified B and should have been adopted")
	}
}

func TestIgnoresPeersFromAnUnverifiedClusterResponse(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	c := newMeshNode(t, "gb3-e5f6", secret)
	b := newMeshNode(t, "gb2-c3d4", nil) // cannot sign
	b.peers = []ClusterPeerInfo{{Addr: c.addr(), Hostname: "gb3", Status: "reachable"}}

	reg := testRegistry(t, secret, b.addr())
	reg.PollOnce(context.Background())

	for _, p := range reg.Peers() {
		if p.Addr == c.addr() {
			t.Fatal("an unverified node's peer list is hearsay and must not be adopted")
		}
	}
}

func TestALearnedPeerIsNotRoutableUntilItProvesItself(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	c := newMeshNode(t, "gb3-e5f6", nil) // reachable, but cannot prove membership
	b := newMeshNode(t, "gb2-c3d4", secret)
	b.peers = []ClusterPeerInfo{{Addr: c.addr(), Status: "reachable"}}

	reg := testRegistry(t, secret, b.addr())
	reg.PollOnce(context.Background())
	reg.PollOnce(context.Background()) // C now polled in its own right

	for _, p := range reg.Peers() {
		if p.Addr == c.addr() && p.Routable() {
			t.Fatal("a learned peer that cannot prove membership must never be routable")
		}
	}
}

func TestTransitiveDiscoveryReachesTheWholeChain(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	d := newMeshNode(t, "gb4-g7h8", secret)
	c := newMeshNode(t, "gb3-e5f6", secret)
	c.peers = []ClusterPeerInfo{{Addr: d.addr(), Status: "reachable"}}
	b := newMeshNode(t, "gb2-c3d4", secret)
	b.peers = []ClusterPeerInfo{{Addr: c.addr(), Status: "reachable"}}

	reg := testRegistry(t, secret, b.addr()) // only B is configured
	for i := 0; i < 3; i++ {
		reg.PollOnce(context.Background())
	}

	want := map[string]bool{b.addr(): false, c.addr(): false, d.addr(): false}
	for _, p := range reg.Peers() {
		if _, ok := want[p.Addr]; ok {
			want[p.Addr] = p.Routable()
		}
	}
	for addr, routable := range want {
		if !routable {
			t.Errorf("%s not routable after three rounds", addr)
		}
	}
}

func TestAdoptionRespectsTheCap(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	b := newMeshNode(t, "gb2-c3d4", secret)
	for i := 0; i < 50; i++ {
		b.peers = append(b.peers, ClusterPeerInfo{Addr: fmt.Sprintf("100.64.1.%d:9100", i), Status: "reachable"})
	}

	reg := testRegistry(t, secret, b.addr())
	reg.SetGossip(GossipOptions{Enabled: true, DiscoveryEvery: 1, MaxLearnedPeers: 10})
	reg.PollOnce(context.Background())

	learned := 0
	for _, p := range reg.Peers() {
		if p.Origin() == OriginLearned {
			learned++
		}
	}
	if learned > 10 {
		t.Fatalf("adopted %d learned peers, cap is 10", learned)
	}
}

func TestNeverAdoptsItself(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	b := newMeshNode(t, "gb2-c3d4", secret)
	self := newMeshNode(t, "gb1-a1b2", secret) // same node id as the registry
	b.peers = []ClusterPeerInfo{{Addr: self.addr(), Status: "reachable"}}

	reg := testRegistry(t, secret, b.addr())
	reg.PollOnce(context.Background())
	reg.PollOnce(context.Background())

	for _, p := range reg.Peers() {
		if p.Addr == self.addr() && p.Routable() {
			t.Fatal("a node must never route to itself")
		}
	}
}

func TestDoesNotAdoptANodeItAlreadyHoldsAtAnotherAddress(t *testing.T) {
	// A co-located or multi-NIC node advertised twice otherwise doubles its
	// apparent capacity and splits its wattage across two dashboard rows.
	secret := []byte("0123456789abcdef0123456789abcdef")
	c := newMeshNode(t, "gb3-e5f6", secret)
	b := newMeshNode(t, "gb2-c3d4", secret)
	b.peers = []ClusterPeerInfo{{Addr: c.addr(), NodeID: "gb3-e5f6", Status: "reachable"}}

	reg := testRegistry(t, secret, b.addr())
	reg.PollOnce(context.Background()) // adopts and verifies C

	// The same node id turns up again at a second address.
	b.peers = append(b.peers, ClusterPeerInfo{Addr: "100.64.9.9:9100", NodeID: "gb3-e5f6", Status: "reachable"})
	reg.PollOnce(context.Background())

	for _, p := range reg.Peers() {
		if p.Addr == "100.64.9.9:9100" {
			t.Fatal("a node already held must not be adopted again at a second address")
		}
	}
}

func TestALearnedPeerIsKeptWhenItStopsAnswering(t *testing.T) {
	// Decided deliberately: nothing is forgotten, so a node that comes back
	// is instantly usable and the topology never flaps.
	secret := []byte("0123456789abcdef0123456789abcdef")
	c := newMeshNode(t, "gb3-e5f6", secret)
	b := newMeshNode(t, "gb2-c3d4", secret)
	b.peers = []ClusterPeerInfo{{Addr: c.addr(), Status: "reachable"}}

	reg := testRegistry(t, secret, b.addr())
	reg.PollOnce(context.Background())
	reg.PollOnce(context.Background())

	cAddr := c.addr()
	c.Close() // the node goes away
	reg.PollOnce(context.Background())

	var found *PeerState
	for _, p := range reg.Peers() {
		if p.Addr == cAddr {
			found = p
		}
	}
	if found == nil {
		t.Fatal("a learned peer must be kept after it stops answering, not evicted")
	}
	if found.Status() != StatusUnreachable {
		t.Error("it should be marked unreachable")
	}
	if found.Routable() {
		t.Error("and must not be routable while unreachable")
	}
	if !found.Verified() {
		t.Error("verification is monotonic: a peer that proved membership keeps its standing")
	}
}

func TestGossipDisabledAdoptsNothing(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	c := newMeshNode(t, "gb3-e5f6", secret)
	b := newMeshNode(t, "gb2-c3d4", secret)
	b.peers = []ClusterPeerInfo{{Addr: c.addr(), Status: "reachable"}}

	reg := testRegistry(t, secret, b.addr())
	reg.SetGossip(GossipOptions{Enabled: false})
	reg.PollOnce(context.Background())

	if got := len(reg.Peers()); got != 1 {
		t.Fatalf("peer count = %d, want 1 with gossip off", got)
	}
}

func TestClusterStateAdvertisesOnlyVerifiedLearnedPeers(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	verified := newMeshNode(t, "gb3-e5f6", secret)
	unproven := newMeshNode(t, "gb4-g7h8", nil)
	b := newMeshNode(t, "gb2-c3d4", secret)
	b.peers = []ClusterPeerInfo{
		{Addr: verified.addr(), Status: "reachable"},
		{Addr: unproven.addr(), Status: "reachable"},
	}

	reg := testRegistry(t, secret, b.addr())
	for i := 0; i < 2; i++ {
		reg.PollOnce(context.Background())
	}

	state := reg.ClusterState()
	var sawVerified, sawUnproven bool
	for _, p := range state.Peers {
		switch p.Addr {
		case verified.addr():
			sawVerified = true
			if p.Origin != OriginLearned {
				t.Errorf("origin = %q, want %q", p.Origin, OriginLearned)
			}
		case unproven.addr():
			sawUnproven = true
		}
	}
	if !sawVerified {
		t.Error("a verified learned peer must be advertised onward")
	}
	if sawUnproven {
		t.Error("an unproven learned peer must not be advertised: hearsay does not become fact by being relayed")
	}
	for _, p := range state.Peers {
		if p.Addr == b.addr() && p.Origin != OriginConfig {
			t.Errorf("configured peer origin = %q, want %q", p.Origin, OriginConfig)
		}
	}
}

func TestSingleHostSurvivesSkippedLearnedPeers(t *testing.T) {
	reg := NewRegistry("gb1-a1b2", "m", nil, []*PeerState{NewPeerState("100.64.0.11:9100")}, time.Second)
	reg.SetLocation("gb1", "100.64.0.10:9100")
	unproven := NewLearnedPeerState("100.64.0.99:9100", "gb9")
	reg.addPeers(unproven)

	if got := len(reg.ClusterState().Peers); got != 1 {
		t.Fatalf("advertised %d peers, want 1", got)
	}
}
