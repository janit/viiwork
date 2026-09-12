//go:build webfixture

package node

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/activity"
	"github.com/janit/viiwork/v2/internal/power"
	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/meshapi"
)

// TestWebFixture serves the dashboards on 127.0.0.1:18086 with canned meshapi
// v2 data, so the pages can be checked in a real browser:
//
//	go test -tags webfixture -run TestWebFixture ./internal/node/
//
// It serves until interrupted, or for WEBFIXTURE_FOR (a duration) when set.
func TestWebFixture(t *testing.T) {
	log := activity.NewLog()
	log.EmitRequestTask(1, -1, "batch-7", "%s", meshapi.RequestStarted("m", "m/0"))
	log.StorePrompt(1, "m", "Translate 'good morning' into Finnish.")
	log.EmitRequest(2, -1, "%s", meshapi.RequestStarted("n", "n/0"))
	log.EmitRequest(2, -1, "%s", meshapi.RequestDone("n", "n/0", 1840*time.Millisecond))
	log.StorePrompt(2, "n", "Summarise the release notes.")
	log.StoreOutput(2, "n", "Viiwork 2 runs one node per machine.", 1840)
	log.Emit("backend", -1, "n/0: healthy")

	started := time.Now().Add(-3 * time.Hour)
	nodeA := fixtureStatusA(started)
	nodeB := meshapi.NodeStatus{
		Node: "node-b", NodeID: "viiwork-00000000000000b0", Ver: "v2.0.0-fixture", Addr: "192.0.2.2", APIPort: 8086,
		UptimeS: 5400, HostMemTotalMB: 128000, HostMemUsedMB: 40000,
		GPUs: []meshapi.GPUInfo{{Index: 0, Vendor: "amd", Util: 0, VRAMUsedMB: 300, VRAMTotalMB: 16368, PowerW: 20}},
		Models: []meshapi.ModelStatus{{
			Name: "x", Engine: "llamacpp", Slots: 0, Busy: 0, Ctx: 8192,
			Backends: []meshapi.BackendStatus{{ID: "x/0", GPUs: []int{0}, Status: meshapi.StatusUnhealthy, Phase: "respawn grace", Slots: 1, Respawns: 2, UptimeS: 40}},
		}},
		Power: meshapi.PowerInfo{Watts: 180, Available: true, Source: "rocm-smi"},
		Cost:  meshapi.CostInfo{Available: false},
	}
	cluster := func() meshapi.ClusterResponse {
		a, b := nodeA, nodeB
		return meshapi.ClusterResponse{
			View: "node-a", Mesh: meshapi.MeshOpen,
			Members: []meshapi.Member{
				{Node: "node-a", Addr: "192.0.2.1", Role: meshapi.RoleNode, State: meshapi.MemberAlive, Status: &a},
				{Node: "node-b", Addr: "192.0.2.2", Role: meshapi.RoleNode, State: meshapi.MemberAlive, Status: &b},
				{Node: "node-c", Addr: "192.0.2.3", Role: meshapi.RoleNode, State: meshapi.MemberDead},
			},
			Models:                []string{"m", "n", "x"},
			PowerControl:          &meshapi.PowerControlInfo{Hosts: []string{"node-a", "node-b", "node-c"}, OutOfBand: []string{"node-c"}},
			ClusterCostEURPerHour: nodeA.Cost.EURPerHour,
			ClusterCostTodayEUR:   nodeA.Cost.TodayEUR,
		}
	}
	resolvedM, resolvedN := "m", "n"
	aliases := meshapi.AliasesResponse{Aliases: []meshapi.AliasInfo{
		{Name: "legacy", Target: "retired-model", Fallbacks: []string{"n"}, UpdatedAt: "2026-09-11T08:00:00.000Z", UpdatedBy: "node-b", Resolved: &resolvedN, State: meshapi.AliasStateFallback},
		{Name: "stable", Target: "m", Fallbacks: []string{}, UpdatedAt: "2026-09-11T09:46:40.123Z", UpdatedBy: "node-a", Resolved: &resolvedM, State: meshapi.AliasStateOK},
	}}

	inference := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case meshapi.PathModels:
			writeJSON(w, meshapi.ModelsResponse{Object: "list", Data: []meshapi.ModelEntry{
				{ID: "m", Object: "model", OwnedBy: meshapi.OwnedByLocal},
				{ID: "n", Object: "model", OwnedBy: meshapi.OwnedByLocal},
				{ID: "stable", Object: "model", OwnedBy: meshapi.OwnedByAlias, Target: "m"},
				{ID: "x", Object: "model", OwnedBy: meshapi.OwnedByPeer},
			}})
		case meshapi.PathChatCompletions:
			body, _ := io.ReadAll(r.Body)
			w.Header().Set(meshapi.HeaderNode, "node-a")
			w.Header().Set(meshapi.HeaderGPUBackend, "m/0")
			w.Header().Set(meshapi.HeaderModel, "m")
			if strings.Contains(string(body), `"model":"stable"`) {
				w.Header().Set(meshapi.HeaderAlias, "stable")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hyvää huomenta\"}}]}\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	})

	h := NewServer(ServerDeps{
		Self: "node-a", Version: "v2.0.0-fixture", Started: started, StreamCtx: context.Background(),
		Inference: inference,
		Aliases:   http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, aliases) }),
		AliasInfo: func() meshapi.AliasesResponse { return aliases },
		Status:    func() meshapi.NodeStatus { return nodeA },
		Cluster:   cluster,
		Members:   func() []mesh.Member { return nil },
		Activity:  log,
		PowerControl: power.NewController(power.ControlConfig{Enabled: true, Hosts: []string{"node-a", "node-b", "node-c"},
			BMCs: map[string]power.BMC{"node-c": {Addr: "192.0.2.13", Username: "admin", Password: "fixture"}}}, "node-a"),
		Health: func() (int, int, int) { return 3, 3, 2 },
	})

	ln, err := net.Listen("tcp", "127.0.0.1:18086")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Logf("web fixture on http://%s/mesh", ln.Addr())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	var deadline <-chan time.Time
	if d, err := time.ParseDuration(os.Getenv("WEBFIXTURE_FOR")); err == nil {
		deadline = time.After(d)
	}
	select {
	case <-stop:
	case <-deadline:
	}
	srv.Close()
}

func fixtureStatusA(started time.Time) meshapi.NodeStatus {
	st := meshapi.NodeStatus{
		Node: "node-a", NodeID: "viiwork-00000000000000a0", Ver: "v2.0.0-fixture", Addr: "192.0.2.1", APIPort: 8086,
		UptimeS: int64(time.Since(started) / time.Second), HostMemTotalMB: 64000, HostMemUsedMB: 21000,
		GPUs: []meshapi.GPUInfo{
			{Index: 0, Vendor: "amd", Util: 87, VRAMUsedMB: 14200, VRAMTotalMB: 16368, PowerW: 160},
			{Index: 1, Vendor: "amd", Util: 83, VRAMUsedMB: 14100, VRAMTotalMB: 16368, PowerW: 150},
			{Index: 2, Vendor: "amd", Util: 0, VRAMUsedMB: 9800, VRAMTotalMB: 16368, PowerW: 22},
		},
		Models: []meshapi.ModelStatus{
			{Name: "m", Engine: "llamacpp", Slots: 2, Busy: 1, Queued: 0, Ctx: 16384, RequestsTotal: 42, TokensTotal: 18000,
				Backends: []meshapi.BackendStatus{
					{ID: "m/0", GPUs: []int{0, 1}, Status: meshapi.StatusHealthy, PID: 4101, RSSMB: 2100, Slots: 2, Busy: 1, TokDecoded: 120, TokRemain: 380, UptimeS: 9000},
				}},
			{Name: "n", Engine: "llamacpp", Slots: 1, Busy: 0, Ctx: 8192, RequestsTotal: 7, TokensTotal: 2400,
				Backends: []meshapi.BackendStatus{
					{ID: "n/0", GPUs: []int{2}, Status: meshapi.StatusHealthy, PID: 4102, RSSMB: 900, Slots: 1, UptimeS: 9000},
				}},
		},
		Power:         meshapi.PowerInfo{Watts: 412, Available: true, Source: "dcmi"},
		EnergyKWh24h:  9.8,
		EnergyKWh30d:  301.5,
		Cost:          meshapi.CostInfo{Available: true, EURPerHour: 0.07, TodayEUR: 1.21, Breakdown: &meshapi.CostBreakdown{SpotCentsKWh: 4.1, TransferCentsKWh: 3.2, TaxCentsKWh: 2.8, VATPercent: 25.5, TotalCentsKWh: 12.7}},
		PromptHistory: 1000,
	}
	return st
}
