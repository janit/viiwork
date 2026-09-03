//go:build integration

package peer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/meshauth"
	"github.com/janit/viiwork/meshapi"
)

// fleetNode is a full in-process mesh node: a real Registry serving its own
// real ClusterState over HTTP, signing responses for proven callers exactly
// the way proxy.signedJSON does. What it exercises end to end: status-poll
// verification, cluster polling, adoption, the advertise-only-verified
// filter, and transitive convergence.
type fleetNode struct {
	nodeID string
	reg    *Registry
	signer *meshauth.Signer
	srv    *httptest.Server
}

func (n *fleetNode) addr() string { return n.srv.Listener.Addr().String() }

func startFleetNode(t *testing.T, nodeID string, secret []byte) *fleetNode {
	t.Helper()
	n := &fleetNode{nodeID: nodeID}
	s, err := meshauth.NewSigner(secret, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	n.signer = s
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		switch r.URL.Path {
		case meshapi.PathStatus:
			body, _ = json.Marshal(StatusResponse{NodeID: n.nodeID, Hostname: n.nodeID, Models: []string{"m-" + n.nodeID}})
		case meshapi.PathCluster:
			body, _ = json.Marshal(n.reg.ClusterState())
		default:
			http.NotFound(w, r)
			return
		}
		if nonce, _, err := n.signer.VerifyRequest(r, nil); err == nil {
			n.signer.SignResponse(w.Header(), r.URL.RequestURI(), nonce, body)
		}
		w.Write(body)
	}))
	t.Cleanup(n.srv.Close)
	return n
}

// startGossipFleet launches n nodes in a ring: node i is seeded with node
// i-1 only, and node 0 with node n-1. One seed per node; only gossip can
// close the ring into a full mesh, which is exactly what the test asserts.
// (A chain with an unseeded head cannot converge under pull-only discovery —
// a node nobody seeds it with polls no one and learns nothing.)
func startGossipFleet(t *testing.T, secret []byte, count int) []*fleetNode {
	t.Helper()
	nodes := make([]*fleetNode, 0, count)
	for i := 0; i < count; i++ {
		nodes = append(nodes, startFleetNode(t, fmt.Sprintf("gb%d-test", i), secret))
	}
	for i, n := range nodes {
		seed := nodes[(i+count-1)%count]
		n.reg = NewRegistry(n.nodeID, "m-"+n.nodeID, nil, []*PeerState{NewPeerState(seed.addr())}, 2*time.Second)
		n.reg.SetSigner(n.signer)
		n.reg.SetLocation(n.nodeID, n.addr())
		n.reg.SetGossip(GossipOptions{Enabled: true, DiscoveryEvery: 1, MaxLearnedPeers: 200})
		n.reg.skipAddrValidation = true // httptest binds loopback; see the field's doc
	}
	return nodes
}

// fleetIsFullyConnected reports whether every node can see every other node
// as a reachable peer in its own cluster snapshot.
func fleetIsFullyConnected(nodes []*fleetNode) bool {
	ids := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		ids[n.nodeID] = true
	}
	for _, n := range nodes {
		state := n.reg.ClusterState()
		seen := map[string]bool{n.nodeID: true}
		for _, p := range state.Peers {
			if p.Status == "reachable" {
				seen[p.NodeID] = true
			}
		}
		for id := range ids {
			if !seen[id] {
				return false
			}
		}
	}
	return true
}

// describeFleet dumps who sees whom, so a convergence failure names the gap
// rather than just reporting a timeout.
func describeFleet(nodes []*fleetNode) string {
	var b strings.Builder
	for _, n := range nodes {
		state := n.reg.ClusterState()
		fmt.Fprintf(&b, "\n%s sees:", n.nodeID)
		for _, p := range state.Peers {
			fmt.Fprintf(&b, " %s/%s/%s", p.NodeID, p.Status, p.Origin)
		}
	}
	return b.String()
}

func TestMeshConvergesFromOneSeed(t *testing.T) {
	// Four nodes in a ring, each configured with one seed. With gossip on,
	// every node should end up able to route to every other.
	secret := []byte("0123456789abcdef0123456789abcdef")
	nodes := startGossipFleet(t, secret, 4)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			n.reg.PollOnce(context.Background())
		}
		if fleetIsFullyConnected(nodes) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("fleet did not converge: %s", describeFleet(nodes))
}
