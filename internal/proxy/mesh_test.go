package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/gpu"
	"github.com/janit/viiwork/internal/peer"
)

// newTestBackendState builds a single tensor-split backend in the shape the
// mesh view has to render: gpu_id -1 with a gpu_ids pair.
func newTestBackendState(t *testing.T) []*balancer.BackendState {
	t.Helper()
	b := balancer.NewBackendState(-1, "127.0.0.1:9851")
	b.GPUIDs = []int{0, 1}
	b.SetStatus(balancer.StatusHealthy)
	b.SetRSSMB(1424)
	b.SetSlots(131072, 1, 0, 73, 0)
	return []*balancer.BackendState{b}
}

// fakeGPUHistory supplies fixed samples in place of a live rocm-smi collector.
type fakeGPUHistory struct{ samples []gpu.GPUSample }

func (f *fakeGPUHistory) Latest() []gpu.GPUSample { return f.samples }

// TestStatusPublishesGPUsAndBackendDetail is the crux of the mesh view: a peer
// learns everything it can show about this node from /v1/status, so anything
// missing here is missing from every other node's /mesh page.
func TestStatusPublishesGPUsAndBackendDetail(t *testing.T) {
	prev := statusGPUSource
	defer func() { statusGPUSource = prev }()
	SetStatusGPUSource(&fakeGPUHistory{samples: []gpu.GPUSample{
		{GPUID: 0, Utilization: 91, VRAMUsedMB: 13851, VRAMTotalMB: 16368},
		{GPUID: 1, Utilization: 4, VRAMUsedMB: 13200, VRAMTotalMB: 16368},
	}})

	bs := newTestBackendState(t)
	h := NewStatusHandler("node-a", "test-model", bs, nil, nil, StatusLocation{Hostname: "hostA"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	var resp peer.StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.GPUs) != 2 {
		t.Fatalf("got %d GPUs, want 2 — peers cannot render GPU load without these", len(resp.GPUs))
	}
	if resp.GPUs[0].Util != 91 || resp.GPUs[0].VRAMTotalMB != 16368 {
		t.Errorf("GPU 0 = %+v, want util 91 and 16368 MB total", resp.GPUs[0])
	}
	if len(resp.Backends) == 0 {
		t.Fatal("no backends published")
	}
	b := resp.Backends[0]
	if b.Model != "test-model" {
		t.Errorf("backend model = %q, want test-model — the mesh view groups on this", b.Model)
	}
	if len(b.GPUIDs) == 0 {
		t.Error("tensor-split backend published no gpu_ids; peers would render gpu--1")
	}
}

// TestClusterCarriesPeerBackendDetail proves the widened BackendInfo actually
// survives the hop: a peer's RSS and context figures must arrive on this node's
// /v1/cluster or the right-hand table is blank for every host but this one.
func TestClusterCarriesPeerBackendDetail(t *testing.T) {
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(peer.StatusResponse{
			NodeID: "node-b", Hostname: "hostB", Models: []string{"peer-model"},
			HealthyBackends: 1, TotalBackends: 1, TotalInFlight: 2,
			Backends: []peer.BackendInfo{{
				GPUID: -1, GPUIDs: []int{4, 5}, Model: "peer-model", Status: "healthy",
				InFlight: 2, RSSMB: 9100, SlotCtx: 4096, SlotCount: 1, TokDecoded: 135,
			}},
			GPUs: []peer.GPUInfo{
				{GPUID: 4, Util: 99, VRAMUsedMB: 11600, VRAMTotalMB: 16368},
				{GPUID: 5, Util: 0, VRAMUsedMB: 11700, VRAMTotalMB: 16368},
			},
		})
	}))
	defer peerSrv.Close()

	ps := peer.NewPeerState(strings.TrimPrefix(peerSrv.URL, "http://"))
	reg := peer.NewRegistry("node-a", "local-model", newTestBackendState(t),
		[]*peer.PeerState{ps}, 2*time.Second)
	reg.SetLocation("hostA", "hostA:8098")
	reg.PollOnce(context.Background())

	state := reg.ClusterState()
	if len(state.Peers) != 1 {
		t.Fatalf("got %d peers, want 1", len(state.Peers))
	}
	p := state.Peers[0]
	if p.Status != "reachable" {
		t.Fatalf("peer status = %q", p.Status)
	}
	if len(p.Backends) != 1 {
		t.Fatalf("got %d peer backends, want 1", len(p.Backends))
	}
	pb := p.Backends[0]
	if pb.RSSMB != 9100 {
		t.Errorf("peer backend RSS = %d, want 9100 — detail was dropped in transit", pb.RSSMB)
	}
	if pb.SlotCtx != 4096 || pb.TokDecoded != 135 {
		t.Errorf("peer context use = %d/%d, want 135/4096", pb.TokDecoded, pb.SlotCtx)
	}
	if pb.Model != "peer-model" {
		t.Errorf("peer backend model = %q, want peer-model", pb.Model)
	}
	if len(p.GPUs) != 2 || p.GPUs[0].Util != 99 {
		t.Errorf("peer GPUs = %+v, want 2 entries with GPU 4 at 99%%", p.GPUs)
	}
}

// TestMeshPageServed checks the route exists and is self-contained.
func TestMeshPageServed(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/mesh", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /mesh = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"viiwork mesh", "/v1/mesh/stream", "/v1/cluster"} {
		if !strings.Contains(body, want) {
			t.Errorf("mesh page missing %q", want)
		}
	}
	// The page must not reach off-host: a strict-CSP or offline viewer has to
	// work, and the whole point is that one node serves the global view.
	if strings.Contains(body, "http://") && !strings.Contains(body, "http://localhost") {
		t.Error("mesh page references an absolute http:// URL; it must be same-origin only")
	}
}
