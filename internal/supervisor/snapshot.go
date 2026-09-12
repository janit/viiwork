package supervisor

import (
	"slices"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/meshapi"
)

// backendView is a consistent snapshot of one backend. Taking it never reads
// /proc, calls an engine or runs a tool: every member polls capacity once a
// second, so snapshots use only what the loop has already cached.
type backendView struct {
	ID        string
	GPUs      []int
	State     State
	Phase     string
	PID       int
	RSSMB     int64
	Load      engine.Load
	LoadFresh bool
	Decoded   int64
	Remain    int64
	InFlight  int
	Respawns  int
	Uptime    time.Duration
}

func (b *Backend) view(now time.Time, staleAfter time.Duration) backendView {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := backendView{
		ID:       b.id,
		GPUs:     slices.Clone(b.gpus),
		State:    StateStarting,
		Phase:    b.phase,
		RSSMB:    b.rss,
		Load:     b.load,
		Decoded:  b.decoded,
		Remain:   b.remain,
		InFlight: b.InFlight(),
	}
	if b.ladder != nil {
		v.State = b.ladder.State()
		v.Respawns = b.ladder.Respawns()
	}
	if !b.loadAt.IsZero() && now.Sub(b.loadAt) < staleAfter {
		v.LoadFresh = true
	}
	if b.proc != nil && b.proc.Running() {
		v.PID = b.proc.PID()
		v.Uptime = now.Sub(b.launchedAt)
	}
	return v
}

// slots is the engine's slot count from a fresh load, else parallel
// (Decision 6: a healthy backend is routable before its first load poll, and a
// /slots that stops answering cannot freeze a stale count into the mesh).
func (v backendView) slots(cfg config.Model) int {
	if v.LoadFresh {
		return v.Load.Slots
	}
	return max(cfg.Parallel, 1)
}

// busy is max(engine busy from a fresh load, node in-flight): the engine sees
// requests the node does not (a stuck slot), the node sees requests the engine
// has not picked up yet.
func (v backendView) busy() int {
	engineBusy := 0
	if v.LoadFresh {
		engineBusy = v.Load.Busy
	}
	return max(engineBusy, v.InFlight)
}

func (v backendView) status(cfg config.Model) meshapi.BackendStatus {
	s := meshapi.BackendStatus{
		ID:       v.ID,
		GPUs:     slices.Clone(v.GPUs),
		Status:   v.State.String(),
		Phase:    v.Phase,
		PID:      v.PID,
		RSSMB:    v.RSSMB,
		Slots:    v.slots(cfg),
		Busy:     v.busy(),
		Respawns: v.Respawns,
		UptimeS:  int64(v.Uptime / time.Second),
	}
	if v.LoadFresh {
		s.TokDecoded, s.TokRemain = v.Decoded, v.Remain
	}
	return s
}

// totals sums slots and busy over healthy backends, and picks the context per
// slot: the largest the engine reports on a healthy fresh backend, else the
// configured one.
func totals(cfg config.Model, views []backendView) (slots, busy, healthy int, ctx int64) {
	for _, v := range views {
		if v.State != StateHealthy {
			continue
		}
		healthy++
		slots += v.slots(cfg)
		busy += v.busy()
		if v.LoadFresh {
			ctx = max(ctx, v.Load.CtxPerSlot)
		}
	}
	if ctx == 0 {
		ctx = int64(cfg.Context)
	}
	return slots, busy, healthy, ctx
}

// modelStatus leaves Queued and the counters at zero: the router owns the
// queue and the proxy the counters, and the node merges them in (P4, P6).
func modelStatus(cfg config.Model, views []backendView) meshapi.ModelStatus {
	slots, busy, _, ctx := totals(cfg, views)
	s := meshapi.ModelStatus{Name: cfg.Name, Engine: cfg.Engine, Slots: slots, Busy: busy, Ctx: ctx}
	s.Backends = make([]meshapi.BackendStatus, 0, len(views))
	for _, v := range views {
		s.Backends = append(s.Backends, v.status(cfg))
	}
	return s
}

func modelCapacity(cfg config.Model, views []backendView) meshapi.ModelCapacity {
	slots, busy, healthy, ctx := totals(cfg, views)
	return meshapi.ModelCapacity{
		Name: cfg.Name, Engine: cfg.Engine, Slots: slots, Busy: busy, Ctx: ctx,
		Backends: len(views), HealthyBackends: healthy,
	}
}
