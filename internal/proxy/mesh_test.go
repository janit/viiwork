package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/activity"
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

// Host memory used to be stripped from the pushed snapshot because an exact
// figure moves every second and defeats change detection. It is coarsened
// instead now that the mesh view draws a per-host memory strip. These pin the
// property that made stripping necessary in the first place.
func TestHostMemDeadbandSuppressesJitter(t *testing.T) {
	// Measured on gb1: ~86 MB of movement per second on a 64 GB host. 21050
	// sits deliberately astride a bucket boundary -- plain rounding flips
	// there on every tick, which is the case a deadband exists for.
	const totalMB = 64 * 1024
	d := newHostMemDeadband()
	snap := func(localMB, peerMB int64) []byte {
		c := peer.ClusterResponse{
			Local: peer.ClusterLocalInfo{HostMemTotalMB: totalMB, HostMemUsedMB: localMB},
			Peers: []peer.ClusterPeerInfo{{Addr: "10.0.0.1:9101", HostMemTotalMB: totalMB, HostMemUsedMB: peerMB}},
		}
		d.apply(&c)
		b, _ := json.Marshal(c)
		return b
	}
	first := snap(38477, 21050)
	for _, j := range []struct{ l, p int64 }{{38477 + 86, 21050 - 60}, {38473, 21110}, {38636, 20990}, {38016, 21301}} {
		if b := snap(j.l, j.p); !bytes.Equal(first, b) {
			t.Errorf("second-to-second jitter must not produce a new snapshot:\n %s\n %s", first, b)
		}
	}
}

func TestHostMemDeadbandKeepsRealMovement(t *testing.T) {
	const totalMB = 64 * 1024
	d := newHostMemDeadband()
	step := int64(totalMB / hostMemBuckets)

	first := d.value("host", 20000, totalMB)
	// The published figure has to stay honest to within one bucket, or the
	// strip draws a level the host was never at.
	if off := absInt64(first - 20000); off > step/2 {
		t.Errorf("published value drifted %d MB from the reading (step %d)", off, step)
	}
	if moved := d.value("host", 28000, totalMB); moved == first {
		t.Error("an 8 GB swing must still change the snapshot")
	} else if off := absInt64(moved - 28000); off > step/2 {
		t.Errorf("published value drifted %d MB after moving (step %d)", off, step)
	}
}

// Each host is held independently: one host moving must not drag another's
// published level with it.
func TestHostMemDeadbandIsPerHost(t *testing.T) {
	const totalMB = 64 * 1024
	d := newHostMemDeadband()
	a := d.value("gb0", 20000, totalMB)
	d.value("gb1", 50000, totalMB)
	if got := d.value("gb0", 20050, totalMB); got != a {
		t.Errorf("gb0 should be held at %d, got %d", a, got)
	}
}

// A node that reports no total -- older peer, or /proc/meminfo unreadable --
// must pass through untouched rather than being snapped against a zero step.
func TestHostMemDeadbandUnknownTotal(t *testing.T) {
	c := peer.ClusterResponse{
		Local: peer.ClusterLocalInfo{HostMemUsedMB: 1234},
		Peers: []peer.ClusterPeerInfo{{Addr: "a", HostMemTotalMB: 0, HostMemUsedMB: 0}},
	}
	newHostMemDeadband().apply(&c)
	if c.Local.HostMemUsedMB != 1234 { t.Errorf("expected passthrough, got %d", c.Local.HostMemUsedMB) }
	if c.Peers[0].HostMemUsedMB != 0 { t.Errorf("expected 0, got %d", c.Peers[0].HostMemUsedMB) }
}

// readSSE runs a streaming handler with an already-cancelled context: the
// handler writes whatever it has for a client that just connected, then returns
// at its first select on ctx.Done. That is exactly the backlog.
func readSSE(t *testing.T, h *Handler, path string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil).WithContext(ctx))
	return rec.Body.String()
}

// Replaying the ring on connect is what makes a dropped connection
// recoverable. A consumer reconstructing in-flight requests from start/done
// pairs loses the pairing for anything that finishes while it is away — a
// slept laptop, a throttled background tab — and a start with no done strands
// a row that never leaves.
func TestActivityStreamReplaysBacklog(t *testing.T) {
	log := activity.NewLog()
	log.EmitRequest(1, 0, "model-a → gpu-0")
	log.EmitRequest(1, 0, "model-a → gpu-0 done (1.2s)")
	log.EmitRequest(2, 1, "model-b → gpu-1")

	h := &Handler{}
	h.SetActivity(log)
	body := readSSE(t, h, "/v1/activity/stream")

	var got []activity.Event
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") { continue }
		var ev activity.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		got = append(got, ev)
	}

	if len(got) != 3 {
		t.Fatalf("replayed %d events, want 3 — without the backlog a reconnect cannot repair its in-flight set", len(got))
	}
	for i, ev := range got {
		if !ev.Replay {
			t.Errorf("event %d not marked replay; a consumer cannot tell it from a live event and will double-render it", i)
		}
	}
	// Both halves of the finished request, so it cancels out on rebuild; only
	// the start of the one still running, so it survives.
	if got[0].RequestID != 1 || !strings.Contains(got[1].Message, "done") || got[2].RequestID != 2 {
		t.Errorf("backlog out of order or incomplete: %+v", got)
	}
}

func TestMeshStreamReplaysBacklog(t *testing.T) {
	log := activity.NewLog()
	log.EmitRequest(7, 0, "model-a → gpu-0")

	reg := peer.NewRegistry("node-a", "model-a", newTestBackendState(t), nil, time.Second)
	reg.SetLocation("hostA", "hostA:8080")
	h := NewMeshHandler(nil, reg, time.Second)
	h.SetActivity(log)

	body := readSSE(t, h, "/v1/mesh/stream")
	if !strings.Contains(body, "event: activity") {
		t.Fatalf("no replayed activity event on connect:\n%s", body)
	}
	var ev MeshEvent
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") { continue }
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil { continue }
		if ev.RequestID == 7 { break }
	}
	if ev.RequestID != 7 || !ev.Replay {
		t.Fatalf("expected replayed rid 7, got %+v", ev)
	}
	// Tagged with the node, or the consumer cannot attribute it to a host --
	// request ids are per-process, so an untagged replay is unusable.
	if ev.NodeID != "node-a" || ev.Hostname != "hostA" {
		t.Errorf("replayed event not tagged with its node: node_id=%q hostname=%q", ev.NodeID, ev.Hostname)
	}
}

// Recent() feeds /v1/activity, which is a plain history read and must not
// claim its events are replays.
func TestBacklogMarksOnlyItsOwnCopy(t *testing.T) {
	log := activity.NewLog()
	log.EmitRequest(1, 0, "model-a → gpu-0")
	if log.Backlog()[0].Replay != true {
		t.Error("Backlog must mark events as replay")
	}
	if log.Recent()[0].Replay != false {
		t.Error("Recent must not mark events as replay — it is a history read, not a stream rebuild")
	}
}
