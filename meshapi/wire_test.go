package meshapi

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// jsonTags returns the wire name of every field on a struct type, plus the
// subset carrying omitempty.
func jsonTags(t *testing.T, v any) (names []string, omit map[string]bool) {
	t.Helper()
	rt := reflect.TypeOf(v)
	omit = map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" {
			// An embedded struct contributes its own fields inline; recurse so
			// MeshEvent is checked against the union it actually serialises as.
			if f.Anonymous {
				sub, subOmit := jsonTags(t, reflect.Zero(f.Type).Interface())
				names = append(names, sub...)
				for k := range subOmit {
					omit[k] = true
				}
				continue
			}
			t.Fatalf("%s.%s has no json tag: every wire field must name itself explicitly", rt.Name(), f.Name)
		}
		parts := strings.Split(tag, ",")
		if parts[0] == "-" {
			continue
		}
		names = append(names, parts[0])
		for _, p := range parts[1:] {
			if p == "omitempty" {
				omit[parts[0]] = true
			}
		}
	}
	sort.Strings(names)
	return names, omit
}

func assertFields(t *testing.T, v any, want []string) {
	t.Helper()
	got, _ := jsonTags(t, v)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%T wire fields drifted.\n got: %v\nwant: %v\n\nRenaming or removing a field is a breaking change with no migration path — the two ends of the wire are upgraded days apart. Add a new field instead.", v, got, want)
	}
}

// The field names below are the contract. A failure here is not a test to
// update: it means a change is about to break every node in the fleet running
// a different build. Adding a name to the list is fine; changing one is not.

func TestStatusResponseWireFields(t *testing.T) {
	assertFields(t, StatusResponse{}, []string{
		"node_id", "hostname", "listen_addr", "models", "backends",
		"total_in_flight", "healthy_backends", "total_backends",
		"power_watts", "power_available", "power_source",
		"cost_available", "cost_eur_per_hour", "cost_today_eur", "cost_breakdown",
		"gpus", "prompt_history",
		"host_mem_total_mb", "host_mem_used_mb",
		"energy_kwh_24h", "energy_kwh_30d",
	})
}

func TestBackendInfoWireFields(t *testing.T) {
	assertFields(t, BackendInfo{}, []string{
		"gpu_id", "gpu_ids", "model", "status", "in_flight",
		"rss_mb", "slot_ctx", "slot_count", "slot_active",
		"tok_decoded", "tok_remain",
	})
}

func TestGPUInfoWireFields(t *testing.T) {
	assertFields(t, GPUInfo{}, []string{"gpu_id", "util", "vram_used_mb", "vram_total_mb"})
}

func TestClusterResponseWireFields(t *testing.T) {
	assertFields(t, ClusterResponse{}, []string{
		"node_id", "version", "hostname", "single_host", "local", "peers",
		"models", "power_control",
		"cluster_cost_eur_per_hour", "cluster_cost_today_eur",
	})
}

func TestClusterPeerInfoWireFields(t *testing.T) {
	assertFields(t, ClusterPeerInfo{}, []string{
		"gpus", "addr", "hostname", "status", "node_id", "models", "backends",
		"total_in_flight", "healthy_backends", "prompt_history",
		"power_watts", "power_available", "power_source",
		"energy_kwh_24h", "energy_kwh_30d",
		"host_mem_total_mb", "host_mem_used_mb",
		"cost_available", "cost_eur_per_hour", "cost_today_eur",
		"origin",
	})
}

func TestEventWireFields(t *testing.T) {
	assertFields(t, Event{}, []string{"t", "type", "message", "gpu_id", "rid", "task_id", "replay"})
}

// MeshEvent embeds Event, so it must serialise as the union of both. A
// consumer reads node_id off the same object it reads message from.
func TestMeshEventWireFields(t *testing.T) {
	assertFields(t, MeshEvent{}, []string{
		"t", "type", "message", "gpu_id", "rid", "task_id", "replay",
		"node_id", "hostname", "addr",
	})
}

func TestPromptEntryWireFields(t *testing.T) {
	assertFields(t, PromptEntry{}, []string{"rid", "t", "model", "prompt", "output", "elapsed_ms"})
}

// Every field added after v1.0 must be omitempty, or a node running an older
// build cannot be told apart from one reporting a real zero. This is the rule
// that lets a mixed-version fleet render at all.
func TestAdditiveFieldsAreOmitempty(t *testing.T) {
	cases := []struct {
		val    any
		fields []string
	}{
		{StatusResponse{}, []string{
			"hostname", "listen_addr", "power_source", "gpus", "prompt_history",
			"host_mem_total_mb", "host_mem_used_mb", "energy_kwh_24h", "energy_kwh_30d",
			"cost_eur_per_hour", "cost_today_eur", "cost_breakdown",
		}},
		{BackendInfo{}, []string{
			"gpu_ids", "rss_mb", "slot_ctx", "slot_count", "slot_active",
			"tok_decoded", "tok_remain",
		}},
		{ClusterPeerInfo{}, []string{
			"gpus", "hostname", "node_id", "models", "backends", "prompt_history",
			"power_source", "energy_kwh_24h", "energy_kwh_30d",
			"host_mem_total_mb", "host_mem_used_mb",
		}},
	}
	for _, c := range cases {
		_, omit := jsonTags(t, c.val)
		for _, f := range c.fields {
			if !omit[f] {
				t.Errorf("%T.%s must be omitempty: without it a node that cannot measure this field publishes a zero indistinguishable from a real reading", c.val, f)
			}
		}
	}
}

// A peer that cannot measure something must omit it, so the JSON of an
// unreachable or minimal node stays small and unambiguous. This pins the
// "absent is not zero" half of the compatibility rule at the encoder.
func TestMinimalStatusOmitsUnmeasured(t *testing.T) {
	b, err := json.Marshal(StatusResponse{NodeID: "n1", Models: []string{}, Backends: []BackendInfo{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{
		"prompt_history", "host_mem_total_mb", "energy_kwh_24h",
		"gpus", "power_source", "hostname",
	} {
		if strings.Contains(string(b), absent) {
			t.Errorf("unmeasured field %q was emitted: %s", absent, b)
		}
	}
	// The always-present fields are the ones a consumer may rely on.
	for _, present := range []string{"node_id", "models", "backends", "total_in_flight", "power_available"} {
		if !strings.Contains(string(b), present) {
			t.Errorf("required field %q missing: %s", present, b)
		}
	}
}

func TestClusterPeerInfoOriginIsOmitEmpty(t *testing.T) {
	b, err := json.Marshal(ClusterPeerInfo{Addr: "100.64.0.11:9100", Status: "reachable"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "origin") {
		t.Fatalf("origin must be omitted when empty, got %s", b)
	}
}
