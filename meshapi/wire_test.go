package meshapi

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
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
		t.Errorf("%T wire fields drifted.\n got: %v\nwant: %v\n\nThese names are frozen from v2.0.0-alpha.1. Add an omitempty field instead of renaming or removing one.", v, got, want)
	}
}

// The lists below are the v2 contract. A failure is not a test to update.
func TestV2WireFields(t *testing.T) {
	assertFields(t, NodeStatus{}, []string{
		"node", "node_id", "ver", "addr", "api_port", "uptime_s",
		"host_mem_total_mb", "host_mem_used_mb", "gpus", "models", "power",
		"energy_kwh_24h", "energy_kwh_30d", "cost", "prompt_history",
	})
	assertFields(t, GPUInfo{}, []string{"index", "uuid", "name", "vendor", "util", "vram_used_mb", "vram_total_mb", "power_w"})
	assertFields(t, ModelStatus{}, []string{"name", "engine", "slots", "busy", "queued", "ctx", "requests_total", "tokens_total", "backends"})
	assertFields(t, BackendStatus{}, []string{
		"id", "gpus", "status", "phase", "pid", "rss_mb", "slots", "busy",
		"tok_decoded", "tok_remain", "respawns", "uptime_s",
	})
	assertFields(t, PowerInfo{}, []string{"watts", "available", "source"})
	assertFields(t, CostInfo{}, []string{"available", "eur_per_hour", "today_eur", "breakdown"})
	assertFields(t, CostBreakdown{}, []string{"spot_cents_kwh", "transfer_cents_kwh", "tax_cents_kwh", "vat_percent", "total_cents_kwh"})
	assertFields(t, CapacityResponse{}, []string{"node", "ver", "models"})
	assertFields(t, ModelCapacity{}, []string{"name", "engine", "slots", "busy", "queued", "ctx", "backends", "healthy_backends"})
	assertFields(t, ClusterResponse{}, []string{"view", "mesh", "members", "models", "power_control", "cluster_cost_eur_per_hour", "cluster_cost_today_eur"})
	assertFields(t, Member{}, []string{"node", "addr", "role", "state", "status"})
	assertFields(t, PowerControlInfo{}, []string{"hosts", "out_of_band"})
	assertFields(t, ModelEntry{}, []string{"id", "object", "owned_by", "target"})
	assertFields(t, ModelsResponse{}, []string{"object", "data"})
	assertFields(t, ErrorResponse{}, []string{"error"})
	assertFields(t, ErrorBody{}, []string{"message", "type"})
	assertFields(t, Event{}, []string{"t", "type", "message", "gpu_id", "rid", "task_id", "replay"})
	assertFields(t, MeshEvent{}, []string{"t", "type", "message", "gpu_id", "rid", "task_id", "replay", "node_id", "hostname", "addr"})
	assertFields(t, PromptEntry{}, []string{"rid", "t", "model", "prompt", "output", "elapsed_ms"})
}

// Fields a node may be unable to measure must be omitempty, so "absent" never
// reads as a measured zero.
func TestV2UnmeasurableFieldsAreOmitempty(t *testing.T) {
	cases := []struct {
		val    any
		fields []string
	}{
		{NodeStatus{}, []string{"host_mem_total_mb", "host_mem_used_mb", "gpus", "energy_kwh_24h", "energy_kwh_30d", "prompt_history"}},
		{GPUInfo{}, []string{"uuid", "name", "vendor", "power_w"}},
		{ModelStatus{}, []string{"requests_total", "tokens_total"}},
		{BackendStatus{}, []string{"gpus", "phase", "pid", "rss_mb", "tok_decoded", "tok_remain"}},
		{PowerInfo{}, []string{"source"}},
		{CostInfo{}, []string{"eur_per_hour", "today_eur", "breakdown"}},
		{ClusterResponse{}, []string{"power_control", "cluster_cost_eur_per_hour", "cluster_cost_today_eur"}},
		{ModelEntry{}, []string{"target"}},
	}
	for _, c := range cases {
		_, omit := jsonTags(t, c.val)
		for _, f := range c.fields {
			if !omit[f] {
				t.Errorf("%T.%s must be omitempty", c.val, f)
			}
		}
	}
}

func TestMemberStatusIsNullWhenNotAlive(t *testing.T) {
	b, err := json.Marshal(Member{Node: "gb2", Addr: "100.64.0.30", Role: RoleNode, State: MemberDead})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"status":null`) {
		t.Errorf("a member that is not alive must carry status null, got %s", b)
	}
}

func TestModelCapacityFree(t *testing.T) {
	if f := (ModelCapacity{Slots: 4, Busy: 1}).Free(); f != 3 {
		t.Errorf("Free() = %d, want 3", f)
	}
	if f := (ModelCapacity{Slots: 2, Busy: 5}).Free(); f != 0 {
		t.Errorf("Free() = %d, want 0 (never negative)", f)
	}
}

func TestAliasPathsAndBackendID(t *testing.T) {
	if p := AliasPath("stable-coder"); p != "/v1/aliases/stable-coder" {
		t.Errorf("AliasPath = %q", p)
	}
	if p := AliasRevertPath("stable-coder"); p != "/v1/aliases/stable-coder/revert" {
		t.Errorf("AliasRevertPath = %q", p)
	}
	if p := AliasPath("a b"); p != "/v1/aliases/a%20b" {
		t.Errorf("AliasPath must escape, got %q", p)
	}
	if id := BackendID("Qwen3.8-27B", 0); id != "Qwen3.8-27B/0" {
		t.Errorf("BackendID = %q", id)
	}
}

// meshapi is imported across module boundaries (gateway), so it must never
// pull in internal/ or third-party code.
func TestImportsStdlibOnly(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(strings.SplitN(path, "/", 2)[0], ".") {
				t.Errorf("%s imports %s: meshapi must stay stdlib-only", name, path)
			}
		}
	}
}

// Paths are contract in the same way field names are: a node routes on them and
// a consumer in another repository dials them or compares against them — the
// gateway matches PathPower and PathMeshPower rather than literals. Changing one
// silently breaks something that cannot be updated in the same commit.
//
// Only what nodes say to each other is listed. The browser pages (/, /mesh,
// /chat, /prompt) and /v1/metrics, which only the dashboard fetches, are
// deliberately absent: freezing a UI route here would make an HTML URL a
// permanent commitment in a package with no migration path, and this contract
// is machine-to-machine.
func TestPathsAreFrozen(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{PathStatus, "/v1/status"},
		{PathCluster, "/v1/cluster"},
		{PathCapacity, "/v1/capacity"},
		{PathModels, "/v1/models"},
		{PathChatCompletions, "/v1/chat/completions"},
		{PathCompletions, "/v1/completions"},
		{PathEmbeddings, "/v1/embeddings"},
		{PathHealth, "/health"},
		{PathActivity, "/v1/activity"},
		{PathActivityStream, "/v1/activity/stream"},
		{PathMeshStream, "/v1/mesh/stream"},
		{PathPrompts, "/v1/prompts"},
		{PathMeshPrompt, "/v1/mesh/prompt"},
		{PathPower, "/v1/power"},
		{PathMeshPower, "/v1/mesh/power"},
		{PathAliases, "/v1/aliases"},
		{AliasRevertSuffix, "/revert"},
	} {
		if c.got != c.want {
			t.Errorf("path changed: %q, want %q", c.got, c.want)
		}
	}
}
