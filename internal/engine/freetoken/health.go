package freetoken

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/janit/viiwork/v2/internal/engine"
)

var errNoGPUs = errors.New("freetoken: a backend needs a GPU")

func errTooManyGPUs(n int) error {
	return fmt.Errorf("freetoken: %d GPUs in one backend; the engine takes exactly one card per process", n)
}

// Lifecycle states as FreeToken's /health reports them.
const (
	healthOK      = "ok"
	healthLoading = "loading"
	healthError   = "error"

	// maintServing is the only maintenance state that accepts work. The engine
	// answers /health with status "ok" while a runtime cache rebuild is in
	// progress and 503s every generation request for its duration, so status
	// alone is not readiness.
	maintServing = "serving"
)

// health is FreeToken's /health document. Three shapes share one struct
// because the engine returns all three from one endpoint with HTTP 200 in
// every case.
type health struct {
	Status      string `json:"status"`
	Maintenance string `json:"maintenance"`
	Message     string `json:"message"`
	Phase       string `json:"phase"`
	Progress    struct {
		DoneBytes  int64 `json:"done_bytes"`
		TotalBytes int64 `json:"total_bytes"`
	} `json:"progress"`
}

// ready reports whether the backend will actually serve a request. An older
// engine build may omit maintenance from the ready shape; status "ok" without
// it is the serving case.
func (h health) ready() bool {
	return h.Status == healthOK && (h.Maintenance == "" || h.Maintenance == maintServing)
}

// describe is a short reason a backend is not ready, for the activity log.
func (h health) describe() string {
	switch {
	case h.ready():
		return ""
	case h.Status == healthError:
		if h.Message != "" {
			return "failed: " + h.Message
		}
		return "failed"
	case h.Status == healthLoading:
		if h.Phase != "" {
			return "loading (" + h.Phase + ")"
		}
		return "loading"
	case h.Maintenance != "":
		return "maintenance: " + h.Maintenance
	}
	return "not ready: " + h.Status
}

// progress is 0..1, or -1 when the engine did not say.
func (h health) progress() float64 {
	if h.Progress.TotalBytes > 0 {
		return float64(h.Progress.DoneBytes) / float64(h.Progress.TotalBytes)
	}
	return -1
}

func parseHealth(body []byte) (health, bool) {
	var h health
	if err := json.Unmarshal(body, &h); err != nil {
		return health{}, false
	}
	if h.Status == "" {
		return health{}, false
	}
	return h, true
}

// Probe reads /health.
//
// THE RULE A REVIEWER SHOULD CHECK TWICE. FreeToken answers /health with 200
// in EVERY lifecycle state, including the minutes a frontier MoE model spends
// loading and any window a live cache rebuild takes it out of service. Both
// 503 every generation request. A port of vLLM's probe — which stops at the
// status code — would advertise the backend to the mesh and have every routed
// request come back 503.
//
// Decision 4: a 200 whose body is not the /health document is NOT ready, and
// is not a transport error either. Something else is listening on that port,
// which is a real failure, but counting it on the health ladder is the right
// outcome.
func (e *Engine) Probe(ctx context.Context, addr string) (engine.Probe, error) {
	resp, err := e.get(ctx, addr, "/health")
	if err != nil {
		return engine.Probe{}, err // transport failure only
	}
	defer drain(resp)

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return engine.Probe{}, err
	}
	h, ok := parseHealth(body)
	if !ok {
		return engine.Probe{
			Ready: false, Progress: -1,
			Reason: "not a FreeToken /health document (" + resp.Status + ")",
		}, nil
	}
	if !h.ready() {
		return engine.Probe{
			Ready: false, Phase: h.Phase, Progress: h.progress(), Reason: h.describe(),
		}, nil
	}
	return engine.Probe{Ready: true, Progress: -1}, nil
}

// pool is one of FreeToken's cache pools. The engine sends null for a pool a
// given model does not use, so a pointer is the difference between "empty" and
// "not applicable".
type pool struct {
	UsedPages  int64 `json:"used_pages"`
	TotalPages int64 `json:"total_pages"`
	UsedSlots  int64 `json:"used_slots"`
	TotalSlots int64 `json:"total_slots"`
}

func (p *pool) occupancy() (float64, bool) {
	if p == nil {
		return 0, false
	}
	switch {
	case p.TotalPages > 0:
		return float64(p.UsedPages) / float64(p.TotalPages), true
	case p.TotalSlots > 0:
		return float64(p.UsedSlots) / float64(p.TotalSlots), true
	}
	return 0, false
}

type statsDoc struct {
	KV    *pool `json:"kv"`
	Mamba *pool `json:"mamba"`
	SWA   *pool `json:"swa"`

	VRAMBytes int64 `json:"vram_bytes"`
	// GPUs is the engine's own account of the card it bound. Added upstream
	// after 0.1.2 and therefore absent on the released engine, which is why a
	// missing entry reads as "unknown" rather than as a mismatch.
	//
	// Index is deliberately not read: under one-card-per-process pinning
	// exactly one device is visible, so it is always 0 and carries no
	// information. UUID is the only field here that names a physical card.
	GPUs []struct {
		Name string `json:"name"`
		UUID string `json:"uuid"`
	} `json:"gpus"`
	Requests struct {
		Active int64 `json:"active"`
	} `json:"requests"`
}

// stats is the subset of /v1/stats this engine acts on.
type stats struct {
	numRunning int64
	// cachePct is the fullest pool, 0..1. A maximum across pools rather than
	// the KV pool alone because which pools exist depends on the model: a
	// dense-attention model reports kv, a sliding-window model reports swa and
	// a null kv, a linear-attention model reports mamba slots. Reading only kv
	// would report a permanent zero for two of the three.
	//
	// Not published: meshapi has no occupancy field. Kept accurate because a
	// number that is wrong while nobody looks stays wrong when somebody does.
	cachePct float64
	gpuUUID  string
	found    map[string]bool
}

func parseStats(body []byte) (stats, bool) {
	var doc statsDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return stats{}, false
	}
	st := stats{
		numRunning: doc.Requests.Active,
		found:      map[string]bool{"running": true},
	}
	for poolName, p := range map[string]*pool{"kv": doc.KV, "swa": doc.SWA, "mamba": doc.Mamba} {
		occ, ok := p.occupancy()
		if !ok {
			continue
		}
		st.found[poolName] = true
		st.found["cache"] = true
		if occ > st.cachePct {
			st.cachePct = occ
		}
	}
	// The first entry is the primary TP rank. There is only ever one: the
	// engine has --tensor-parallel-size but does not yet place more than one
	// card, and the list exists upstream so that it can later.
	if len(doc.GPUs) > 0 && doc.GPUs[0].UUID != "" {
		st.gpuUUID = doc.GPUs[0].UUID
		st.found["gpu_uuid"] = true
	}
	return st, true
}

// Load reads /v1/stats.
//
// Waiting is always 0 (Decision 5): FreeToken's scheduler runs
// --max-running-requests concurrently and holds the rest, but never says how
// many it is holding. There is no queue-depth gauge to read. The node's own
// origin queue is what `queued` means on the dashboard.
func (e *Engine) Load(ctx context.Context, s engine.Spec, addr string) (engine.Load, error) {
	st, err := e.stats(ctx, addr)
	if err != nil {
		return engine.Load{}, err
	}
	return engine.Load{
		Slots:      s.Parallel,
		Busy:       int(st.numRunning),
		Waiting:    0,
		CtxPerSlot: int64(s.Context),
	}, nil
}

// BoundGPU is engine.GPUBindingReader: the card the engine says it actually
// bound. ok is false on an engine at or before 0.1.2, which reports no UUID —
// that is "unknown", not a mismatch.
//
// Worth reporting because the failure it catches is otherwise silent. Pinning
// happens through CUDA_VISIBLE_DEVICES, whose index CUDA resolves in whatever
// order CUDA_DEVICE_ORDER selects; get that wrong and the backend loads,
// serves and answers every probe while the GPU panel attributes its load, its
// VRAM and its wattage to a neighbouring card — and on a host recording energy,
// the misattribution is written to disk.
func (e *Engine) BoundGPU(ctx context.Context, addr string) (string, bool, error) {
	st, err := e.stats(ctx, addr)
	if err != nil {
		return "", false, err
	}
	if !st.found["gpu_uuid"] {
		return "", false, nil
	}
	return st.gpuUUID, true, nil
}

func (e *Engine) stats(ctx context.Context, addr string) (stats, error) {
	resp, err := e.get(ctx, addr, "/v1/stats")
	if err != nil {
		return stats{}, err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return stats{}, fmt.Errorf("freetoken: /v1/stats answered %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return stats{}, err
	}
	st, ok := parseStats(body)
	if !ok {
		return stats{}, errors.New("freetoken: /v1/stats is not the stats document")
	}
	return st, nil
}

func (e *Engine) get(ctx context.Context, addr, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return nil, err
	}
	return e.client.Do(req)
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}
