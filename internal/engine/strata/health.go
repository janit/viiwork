package strata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
)

// healthDoc is Strata's /health. Loaded is a pointer so that a document
// without the field is "did not say", which is not ready.
type healthDoc struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Loaded  *bool  `json:"loaded"`
}

// Probe reads /health. Ready needs all three of service "strata", status "ok"
// and loaded true: a server started with --lazy, or one that unloaded itself
// when idle, answers 200 with status "ok" and would 503 or stall the first
// request routed to it.
//
// Phase and Progress are never set. While the model loads the port is closed,
// so the node sees transport errors, which is the expected case while starting;
// the load stages only go to the server's stdout.
func (e *Engine) Probe(ctx context.Context, addr string) (engine.Probe, error) {
	resp, err := e.get(ctx, addr, "/health")
	if err != nil {
		return engine.Probe{}, err // transport failure only
	}
	defer drain(resp)
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return engine.Probe{}, err
	}
	notReady := func(reason string) (engine.Probe, error) {
		return engine.Probe{Ready: false, Progress: -1, Reason: reason}, nil
	}
	var h healthDoc
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &h) != nil || h.Service != name {
		return notReady("not a Strata /health document (" + resp.Status + ")")
	}
	if h.Status != "ok" {
		return notReady("status " + h.Status)
	}
	if h.Loaded == nil || !*h.Loaded {
		return notReady("model not loaded")
	}
	return engine.Probe{Ready: true, Progress: -1}, nil
}

// slotDoc is one entry of /slots. IsProcessing is a pointer so that an entry
// without the flag is an error, not a measured "idle".
type slotDoc struct {
	NCtx         int64 `json:"n_ctx"`
	IsProcessing *bool `json:"is_processing"`
}

// statusDoc is the part of /status this engine reads: the server's own queue
// and the running request's token counts. The numbers are float64 so that a
// build writing 1.0 for 1 still decodes; once idle the document keeps the
// last request's counts, so they are read only while Busy.
type statusDoc struct {
	Busy      bool     `json:"busy"`
	Queued    *float64 `json:"queued"`
	Generated *float64 `json:"generated"`
	MaxTokens *float64 `json:"max_tokens"`
}

// Load is LoadProgress without the progress.
func (e *Engine) Load(ctx context.Context, s engine.Spec, addr string) (engine.Load, error) {
	l, _, _, err := e.LoadProgress(ctx, s, addr)
	return l, err
}

// LoadProgress is engine.TokenProgressReader.
//
// /slots is the part routing depends on. Anything wrong with it is an error,
// never a zero. From v0.1.41 it lists one entry per batch slot, each with the
// engine's whole --max-context, and that count is what the node publishes: the
// engine runs fewer slots than "parallel" asks for when they do not fit in
// VRAM, and says so only in its log. Up to v0.1.40.1 it listed exactly one
// slot whatever "parallel" was, so on such a build a model with parallel
// above 1 publishes one slot.
//
// A request that runs alone is decoded outside the batch slots, so every
// entry reads idle while it runs. Busy is then 0, and the node's own count of
// requests in flight is what marks the slot taken (the supervisor publishes
// the larger of the two).
//
// /status adds the server's own queue and the running request's token counts.
// It is best effort: the heavier /metrics sometimes did not answer on gb3
// while a request was running, and a Load that failed with its second request
// would throw away a good /slots reading. Without it Waiting is 0, which the
// contract defines as "publishes none", and progress is 0.
func (e *Engine) LoadProgress(ctx context.Context, _ engine.Spec, addr string) (load engine.Load, decoded, remain int64, err error) {
	slots, err := e.slots(ctx, addr)
	if err != nil {
		return engine.Load{}, 0, 0, err
	}
	load.Slots = len(slots)
	for i, s := range slots {
		if s.NCtx <= 0 || s.IsProcessing == nil {
			return engine.Load{}, 0, 0, errors.New("strata: /slots reports a slot without n_ctx or is_processing")
		}
		if i == 0 || s.NCtx < load.CtxPerSlot {
			load.CtxPerSlot = s.NCtx
		}
		if *s.IsProcessing {
			load.Busy++
		}
	}

	st, ok := e.status(ctx, addr)
	if !ok {
		return load, 0, 0, nil
	}
	if st.Queued != nil && *st.Queued > 0 {
		load.Waiting = int(*st.Queued)
	}
	// /status counts one request. With more than one slot at work it
	// describes none of them, and no progress is the honest answer.
	if g, m := st.Generated, st.MaxTokens; load.Busy <= 1 && st.Busy && g != nil && m != nil && *g >= 0 && *m >= *g {
		decoded, remain = int64(*g), int64(*m-*g)
	}
	return load, decoded, remain, nil
}

func (e *Engine) slots(ctx context.Context, addr string) ([]slotDoc, error) {
	resp, err := e.get(ctx, addr, "/slots")
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("strata: /slots answered %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, err
	}
	var slots []slotDoc
	if err := json.Unmarshal(body, &slots); err != nil {
		return nil, fmt.Errorf("strata: decoding /slots: %w", err)
	}
	if len(slots) == 0 {
		return nil, errors.New("strata: /slots lists no slot")
	}
	return slots, nil
}

// statusTimeout keeps a stalled /status from using up the whole load tick
// (1 s) that /slots has already answered inside.
const statusTimeout = 400 * time.Millisecond

func (e *Engine) status(ctx context.Context, addr string) (statusDoc, bool) {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	resp, err := e.get(ctx, addr, "/status")
	if err != nil {
		return statusDoc{}, false
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return statusDoc{}, false
	}
	var st statusDoc
	if json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&st) != nil {
		return statusDoc{}, false
	}
	return st, true
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
