//go:build integration

package node

// /v1/fleet/capacity over a real three-node mesh. The unit tests in
// internal/proxy prove the aggregation rules; this proves the endpoint is
// wired, that a node's view includes its peers, and that losing a peer moves it
// to stale rather than deleting it.

import (
	"encoding/json"
	"net/http"
	"strings"
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

// An alias is an acceptable ?model= value, because an alias is the model name
// we tell consumers to configure. internal/proxy tests the filter's four rows;
// this proves the handler actually asks the resolver — a refactor that drops
// Deps.Resolve from the fleet path fails here.
func TestFleetCapacityResolvesAnAlias(t *testing.T) {
	net := meshtest.NewNetwork()
	tn := startNode(t, net, "A", []fakeModel{{name: "m"}, {name: "n"}}, "", seeded(net, "A"))

	req, _ := http.NewRequest(http.MethodPut, tn.url(meshapi.AliasPath("stable")), strings.NewReader(`{"target":"m"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT alias: %d", resp.StatusCode)
	}

	until(t, 5*time.Second, "the fleet view resolves the alias", func() bool {
		var got meshapi.FleetCapacityResponse
		if getJSON(t, tn.url(meshapi.PathFleetCapacity+"?model=stable"), &got) != http.StatusOK {
			return false
		}
		if len(got.Models) != 1 || got.Models[0].Name != "m" {
			return false
		}
		// The alias is echoed, never put in Name: two nodes must not disagree
		// about a model's name depending on how it was asked for.
		if got.ResolvedFrom != "stable" {
			t.Fatalf("resolved_from = %q, want stable", got.ResolvedFrom)
		}
		return true
	})

	// An alias nobody serves is zero capacity, not an outage: empty list, 200.
	req2, _ := http.NewRequest(http.MethodPut, tn.url(meshapi.AliasPath("dangling")), strings.NewReader(`{"target":"gone","force":true}`))
	resp3, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("PUT dangling alias: %d", resp3.StatusCode)
	}
	var dangling meshapi.FleetCapacityResponse
	if code := getJSON(t, tn.url(meshapi.PathFleetCapacity+"?model=dangling"), &dangling); code != http.StatusOK {
		t.Fatalf("dangling alias status %d, want 200: no capacity is an answer, not a failure", code)
	}
	if len(dangling.Models) != 0 {
		t.Errorf("dangling alias gave %+v, want empty", dangling.Models)
	}
	if dangling.ResolvedFrom != "" {
		t.Errorf("resolved_from = %q, want empty: nothing was resolved", dangling.ResolvedFrom)
	}
}
