package accept

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/janit/viiwork/v2/meshapi"
)

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// capacityModel is model a with free = slots - busy on healthy backends.
func capacityModel(slots, busy int) meshapi.ModelStatus {
	m := meshapi.ModelStatus{Name: "a", Engine: "llamacpp", Slots: slots, Busy: busy}
	m.Backends = []meshapi.BackendStatus{{ID: "a/0", Status: meshapi.StatusHealthy, Slots: slots, Busy: busy}}
	return m
}

// saturateNode is node-a with local slots for a, in a cluster where the other
// members serve a with the given free slots.
func saturateNode(t *testing.T, local int, peers map[string]int) *fakeNode {
	t.Helper()
	node := newFakeNode(t, "node-a")
	self := nodeStatus("node-a", capacityModel(local, 0))
	node.SetStatus(self)
	members := []meshapi.Member{member("node-a", meshapi.MemberAlive, &self)}
	for name, free := range peers {
		st := nodeStatus(name, capacityModel(free, 0))
		members = append(members, member(name, meshapi.MemberAlive, &st))
	}
	node.SetCluster(clusterOf("node-a", members...))
	return node
}

func TestSaturateSpills(t *testing.T) { // S1
	node := saturateNode(t, 2, map[string]int{"node-b": 2})
	var n atomic.Int32
	node.OnChat(func(r *http.Request, body map[string]any) (int, http.Header, any) {
		if r.URL.RawQuery != "" || body["max_tokens"] != float64(200) || !strings.Contains(toJSON(body["messages"]), "Count from 1 to 200") {
			t.Errorf("request ?%s %v", r.URL.RawQuery, body)
		}
		if n.Add(1) <= 2 {
			return 200, nodeHeaders("node-a"), chatReply("a", "1 2 3", "length")
		}
		return 200, nodeHeaders("node-b", meshapi.HeaderQueuedMs, "40"), chatReply("a", "1 2 3", "length")
	})
	r := Saturate(context.Background(), DefaultEnv(), node.URL(), "a", SaturateOptions{})
	if n.Load() != 6 {
		t.Errorf("%d requests sent, want 6", n.Load())
	}
	if got := strings.Join(checkNames(r), "|"); got != "no 429 with free slots|spilled to a peer|distribution" {
		t.Fatalf("checks: %+v", r.Checks)
	}
	if !r.Pass() {
		t.Errorf("checks: %+v", r.Checks)
	}
	if d := checkByName(t, r, "distribution").Detail; !strings.Contains(d, "node-a:2 node-b:4") || !strings.Contains(d, "queued:4") {
		t.Errorf("distribution %q", d)
	}
	if d := checkByName(t, r, "no 429 with free slots").Detail; d != "n=6 200:6 meshFree=4" {
		t.Errorf("no 429 detail %q", d)
	}
}

func TestSaturateNeverSpills(t *testing.T) { // S2
	node := saturateNode(t, 2, map[string]int{"node-b": 2})
	node.OnChat(func(r *http.Request, body map[string]any) (int, http.Header, any) {
		return 200, nodeHeaders("node-a"), chatReply("a", "1 2 3", "length")
	})
	r := Saturate(context.Background(), DefaultEnv(), node.URL(), "a", SaturateOptions{})
	if c := checkByName(t, r, "spilled to a peer"); c.Pass {
		t.Errorf("spilled: %+v", c)
	}
}

func TestSaturate429WithoutFreeSlots(t *testing.T) { // S3
	node := saturateNode(t, 2, nil)
	var n atomic.Int32
	node.OnChat(func(r *http.Request, body map[string]any) (int, http.Header, any) {
		if n.Add(1) <= 2 {
			return 429, nil, map[string]any{"error": map[string]any{"message": "queue full"}}
		}
		return 200, nodeHeaders("node-a"), chatReply("a", "1 2 3", "length")
	})
	r := Saturate(context.Background(), DefaultEnv(), node.URL(), "a", SaturateOptions{})
	if c := checkByName(t, r, "no 429 with free slots"); !c.Pass || c.Detail != "n=6 200:4 429:2 meshFree=2" {
		t.Errorf("no 429: %+v", c)
	}
	if c := checkByName(t, r, "spilled to a peer"); !c.Pass || !strings.HasPrefix(c.Detail, "not applicable: ") {
		t.Errorf("spilled: %+v", c)
	}
}

func TestSaturate429WithFreeSlots(t *testing.T) { // S4
	node := saturateNode(t, 2, map[string]int{"node-b": 6})
	var n atomic.Int32
	node.OnChat(func(r *http.Request, body map[string]any) (int, http.Header, any) {
		if n.Add(1) == 1 {
			return 429, nil, map[string]any{}
		}
		return 200, nodeHeaders("node-b"), chatReply("a", "1 2 3", "length")
	})
	r := Saturate(context.Background(), DefaultEnv(), node.URL(), "a", SaturateOptions{})
	if c := checkByName(t, r, "no 429 with free slots"); c.Pass {
		t.Errorf("no 429: %+v", c)
	}
}

func TestSaturateOtherStatusFails(t *testing.T) { // S5
	node := saturateNode(t, 2, nil)
	var n atomic.Int32
	node.OnChat(func(r *http.Request, body map[string]any) (int, http.Header, any) {
		if n.Add(1) == 1 {
			return 502, nil, map[string]any{}
		}
		return 200, nodeHeaders("node-a"), chatReply("a", "1 2 3", "length")
	})
	r := Saturate(context.Background(), DefaultEnv(), node.URL(), "a", SaturateOptions{})
	if c := checkByName(t, r, "no 429 with free slots"); c.Pass || !strings.Contains(c.Detail, "502") {
		t.Errorf("no 429: %+v", c)
	}
}

func TestSaturateNotServedLocally(t *testing.T) { // S6
	node := saturateNode(t, 2, nil)
	r := Saturate(context.Background(), DefaultEnv(), node.URL(), "b", SaturateOptions{})
	if len(r.Checks) != 1 || r.Checks[0].Name != "served locally" || r.Checks[0].Pass {
		t.Errorf("checks: %+v", r.Checks)
	}
}
