//go:build integration

package node

// /v1/fleet/capacity over a real three-node mesh. The unit tests in
// internal/proxy prove the aggregation rules; this proves the endpoint is
// wired, that a node's view includes its peers, and that losing a peer moves it
// to stale rather than deleting it.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/mesh/meshtest"
	"github.com/janit/viiwork/v2/meshapi"
)

func fleetOf(t *testing.T, tn *testNode) meshapi.FleetCapacityResponse {
	t.Helper()
	resp, err := http.Get(tn.url(meshapi.PathFleetCapacity))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %s", resp.Status)
	}
	var out meshapi.FleetCapacityResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func fleetModel(r meshapi.FleetCapacityResponse, name string) (meshapi.FleetModel, bool) {
	for _, m := range r.Models {
		if m.Name == name {
			return m, true
		}
	}
	return meshapi.FleetModel{}, false
}

// The point of the endpoint: one model on three nodes reads as its sum from
// every node's view, which /v1/capacity cannot say.
func TestFleetCapacitySumsAcrossTheMesh(t *testing.T) {
	net := meshtest.NewNetwork()
	model := []fakeModel{{name: "m", parallel: 2}}
	a := startNode(t, net, "A", model, "", seeded(net, "A"))
	b := startNode(t, net, "B", model, "", seeded(net, "B", a))
	c := startNode(t, net, "C", model, "", seeded(net, "C", a, b))

	// Every node must agree on the total once capacity has been polled.
	for _, tn := range []*testNode{a, b, c} {
		deadline := time.Now().Add(20 * time.Second)
		var got meshapi.FleetModel
		for time.Now().Before(deadline) {
			if m, ok := fleetModel(fleetOf(t, tn), "m"); ok && m.Slots == 6 {
				got = m
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if got.Slots != 6 {
			t.Fatalf("%s: slots = %d, want 6 (three nodes x parallel 2)", tn.Node.name, got.Slots)
		}
		if len(got.Hosts) != 3 {
			t.Errorf("%s: hosts = %d, want 3: %+v", tn.Node.name, len(got.Hosts), got.Hosts)
		}
		if got.Free != 6 {
			t.Errorf("%s: free = %d, want 6", tn.Node.name, got.Free)
		}
		// A fresh host's numbers are present, including a measured zero.
		for _, h := range got.Hosts {
			if h.Stale {
				t.Errorf("%s: host %s unexpectedly stale", tn.Node.name, h.Node)
			}
			if h.Slots == nil || h.Busy == nil || h.Free == nil {
				t.Errorf("%s: fresh host %s must carry its numbers: %+v", tn.Node.name, h.Node, h)
			}
		}
	}

	// The view names the answering node rather than claiming a fleet truth.
	if v := fleetOf(t, a).View; v != "A" {
		t.Errorf("view = %q, want A", v)
	}
}

// Losing a node must shrink the totals AND leave the host visible, because
// "the fleet shrank" and "we lost sight of C" are different problems.
func TestFleetCapacityKeepsALostHostVisible(t *testing.T) {
	net := meshtest.NewNetwork()
	model := []fakeModel{{name: "m", parallel: 2}}
	a := startNode(t, net, "A", model, "", seeded(net, "A"))
	b := startNode(t, net, "B", model, "", seeded(net, "B", a))

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if m, ok := fleetModel(fleetOf(t, a), "m"); ok && m.Slots == 4 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Cancel, but do NOT read b.done: the harness's own t.Cleanup waits on that
	// channel, and draining it here makes cleanup block for its full timeout.
	b.cancel()

	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		m, ok := fleetModel(fleetOf(t, a), "m")
		if ok && m.Slots == 2 {
			// A's own slots only. B is either gone from the member list or
			// present and stale; if present it must carry no numbers.
			for _, h := range m.Hosts {
				if h.Node == "B" && !h.Stale {
					t.Errorf("B is still counted as fresh: %+v", h)
				}
				if h.Node == "B" && h.Slots != nil {
					t.Errorf("a stale host must omit its numbers: %+v", h)
				}
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("A still reports B's capacity 30s after B stopped")
}

// ?model= filters, and an unknown name is an empty list rather than a 404:
// "no capacity for that model" is an answer.
func TestFleetCapacityModelFilter(t *testing.T) {
	net := meshtest.NewNetwork()
	tn := startNode(t, net, "A", []fakeModel{{name: "m"}, {name: "n"}}, "", seeded(net, "A"))

	resp, err := http.Get(tn.url(meshapi.PathFleetCapacity + "?model=m"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got meshapi.FleetCapacityResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 1 || got.Models[0].Name != "m" {
		t.Errorf("?model=m gave %+v", got.Models)
	}

	resp2, err := http.Get(tn.url(meshapi.PathFleetCapacity + "?model=nope"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("unknown model status %s, want 200: absence is an answer", resp2.Status)
	}
	var none meshapi.FleetCapacityResponse
	if err := json.NewDecoder(resp2.Body).Decode(&none); err != nil {
		t.Fatal(err)
	}
	if len(none.Models) != 0 {
		t.Errorf("unknown model gave %+v, want empty", none.Models)
	}
}
