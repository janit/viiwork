package proxy

// The fleet aggregator is a pure function over the capacity reports a node
// already holds, so every rule in the design is testable without a mesh.
// Design: docs/superpowers/specs/2026-09-13-fleet-capacity-api-design.md

import (
	"testing"
	"time"

	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

var fleetNow = time.Date(2026, 9, 13, 15, 0, 0, 0, time.UTC)

// rep is a member report received age ago.
func rep(node, api string, age time.Duration, models ...meshapi.ModelCapacity) capacity.Report {
	return capacity.Report{Node: node, APIAddr: api, Received: fleetNow.Add(-age), Models: models}
}

func mc(name string, slots, busy, queued int, ctx int64) meshapi.ModelCapacity {
	return meshapi.ModelCapacity{Name: name, Engine: "llamacpp", Slots: slots, Busy: busy, Queued: queued, Ctx: ctx}
}

// model returns the named model from a response.
func model(t *testing.T, r meshapi.FleetCapacityResponse, name string) meshapi.FleetModel {
	t.Helper()
	for _, m := range r.Models {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("model %q not in response %+v", name, r.Models)
	return meshapi.FleetModel{}
}

func host(t *testing.T, m meshapi.FleetModel, node string) meshapi.FleetHost {
	t.Helper()
	for _, h := range m.Hosts {
		if h.Node == node {
			return h
		}
	}
	t.Fatalf("host %q not in %+v", node, m.Hosts)
	return meshapi.FleetHost{}
}

// The headline case: three hosts of one model sum, which is the number
// /v1/capacity cannot give.
func TestFleetSumsFreshHosts(t *testing.T) {
	got := aggregateFleet("gb1",
		[]meshapi.ModelCapacity{mc("tg", 12, 1, 0, 4096)},
		[]capacity.Report{
			rep("gb2", "10.0.0.2:8086", 300*time.Millisecond, mc("tg", 12, 0, 0, 4096)),
			rep("gb3", "10.0.0.3:8086", 400*time.Millisecond, mc("tg", 12, 2, 1, 4096)),
		}, fleetNow, 3*time.Second)

	m := model(t, got, "tg")
	if m.Slots != 36 {
		t.Errorf("slots = %d, want 36", m.Slots)
	}
	if m.Busy != 3 {
		t.Errorf("busy = %d, want 3", m.Busy)
	}
	if m.Free != 33 {
		t.Errorf("free = %d, want 33 (slots - busy)", m.Free)
	}
	// Decided in the design: queued is the fleet sum, not this node's.
	if m.Queued != 1 {
		t.Errorf("queued = %d, want 1 summed across hosts", m.Queued)
	}
	if len(m.Hosts) != 3 {
		t.Errorf("hosts = %d, want 3", len(m.Hosts))
	}
}

// The answering node is not in Reports(); it must be counted exactly once.
func TestFleetCountsTheAnsweringNodeExactlyOnce(t *testing.T) {
	got := aggregateFleet("gb1",
		[]meshapi.ModelCapacity{mc("tg", 12, 0, 0, 4096)},
		// A report from ourselves, which a mesh can produce transiently.
		[]capacity.Report{rep("gb1", "10.0.0.1:8086", time.Millisecond, mc("tg", 12, 0, 0, 4096))},
		fleetNow, 3*time.Second)

	m := model(t, got, "tg")
	if m.Slots != 12 {
		t.Errorf("slots = %d, want 12: the answering node must not be double counted", m.Slots)
	}
	if len(m.Hosts) != 1 {
		t.Errorf("hosts = %d, want 1", len(m.Hosts))
	}
}

// A stale host is listed and excluded, and — the rule that matters — its
// numbers are ABSENT rather than zero.
func TestFleetListsStaleHostWithoutNumbers(t *testing.T) {
	got := aggregateFleet("gb1",
		[]meshapi.ModelCapacity{mc("tg", 12, 0, 0, 4096)},
		[]capacity.Report{
			rep("gb3", "10.0.0.3:8086", 9*time.Second, mc("tg", 12, 5, 0, 4096)),
		}, fleetNow, 3*time.Second)

	m := model(t, got, "tg")
	if m.Slots != 12 {
		t.Errorf("slots = %d, want 12: a stale host contributes nothing", m.Slots)
	}
	h := host(t, m, "gb3")
	if !h.Stale {
		t.Error("gb3 must be marked stale")
	}
	if h.Slots != nil || h.Busy != nil || h.Free != nil || h.Ctx != nil {
		t.Errorf("a stale host must omit its numbers, not zero them: %+v", h)
	}
	if h.AgeMS < 8000 {
		t.Errorf("age_ms = %d, want ~9000: age is the one thing still assertable", h.AgeMS)
	}
}

// A fresh host's zero IS a measurement and must be present, which is why the
// fields are pointers rather than omitempty ints.
func TestFleetKeepsAFreshZeroAsAMeasurement(t *testing.T) {
	got := aggregateFleet("gb1", nil,
		[]capacity.Report{rep("gb2", "10.0.0.2:8086", time.Millisecond, mc("tg", 4, 0, 0, 4096))},
		fleetNow, 3*time.Second)

	h := host(t, model(t, got, "tg"), "gb2")
	if h.Busy == nil {
		t.Fatal("a fresh host's busy must be present even when 0")
	}
	if *h.Busy != 0 {
		t.Errorf("busy = %d, want 0", *h.Busy)
	}
}

// A model only a stale host serves must still appear. Vanishing reads as "no
// such model", which is a different and wrong answer.
func TestFleetKeepsAModelServedOnlyByAStaleHost(t *testing.T) {
	got := aggregateFleet("gb1", nil,
		[]capacity.Report{rep("gb3", "10.0.0.3:8086", 9*time.Second, mc("tg", 12, 0, 0, 4096))},
		fleetNow, 3*time.Second)

	m := model(t, got, "tg")
	if m.Slots != 0 {
		t.Errorf("slots = %d, want 0", m.Slots)
	}
	if len(m.Hosts) != 1 || !m.Hosts[0].Stale {
		t.Errorf("the model must remain listed with its stale host: %+v", m.Hosts)
	}
}

// Hosts may disagree about context. A consumer sizing prompts needs the floor.
func TestFleetCtxIsTheMinimumAcrossFreshHosts(t *testing.T) {
	got := aggregateFleet("gb1",
		[]meshapi.ModelCapacity{mc("tg", 4, 0, 0, 8192)},
		[]capacity.Report{
			rep("gb2", "10.0.0.2:8086", time.Millisecond, mc("tg", 4, 0, 0, 4096)),
			// A stale host must not drag the floor down with a value we do not
			// currently stand behind.
			rep("gb3", "10.0.0.3:8086", 9*time.Second, mc("tg", 4, 0, 0, 512)),
		}, fleetNow, 3*time.Second)

	m := model(t, got, "tg")
	if m.Ctx != 4096 {
		t.Errorf("ctx = %d, want 4096 (min over FRESH hosts)", m.Ctx)
	}
}

// Free must never go negative, however the counters arrive.
func TestFleetFreeNeverNegative(t *testing.T) {
	got := aggregateFleet("gb1", []meshapi.ModelCapacity{mc("tg", 2, 5, 0, 4096)}, nil, fleetNow, 3*time.Second)

	m := model(t, got, "tg")
	if m.Free != 0 {
		t.Errorf("free = %d, want 0", m.Free)
	}
	if h := host(t, m, "gb1"); h.Free == nil || *h.Free != 0 {
		t.Errorf("per-host free must also clamp at 0: %+v", h)
	}
}

// The view is the answering node, and stale_after is published so a consumer
// can interpret age_ms without guessing.
func TestFleetReportsItsOwnViewAndStaleAfter(t *testing.T) {
	got := aggregateFleet("gb1", nil, nil, fleetNow, 3*time.Second)

	if got.View != "gb1" {
		t.Errorf("view = %q, want gb1", got.View)
	}
	if got.StaleAfterS != 3 {
		t.Errorf("stale_after_s = %v, want 3", got.StaleAfterS)
	}
}

// Models are ordered by name, so a consumer diffing two polls sees real
// changes rather than map iteration order.
func TestFleetModelsAreOrderedByName(t *testing.T) {
	got := aggregateFleet("gb1",
		[]meshapi.ModelCapacity{mc("zeta", 1, 0, 0, 1), mc("alpha", 1, 0, 0, 1)},
		[]capacity.Report{rep("gb2", "10.0.0.2:8086", time.Millisecond, mc("mid", 1, 0, 0, 1))},
		fleetNow, 3*time.Second)

	var names []string
	for _, m := range got.Models {
		names = append(names, m.Name)
	}
	want := []string{"alpha", "mid", "zeta"}
	for i := range want {
		if i >= len(names) || names[i] != want[i] {
			t.Fatalf("models %v, want %v", names, want)
		}
	}
}
