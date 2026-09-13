package llamacpp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/janit/viiwork/v2/internal/engine"
)

// maxBody caps what is read from /health and /slots. /slots repeats each busy
// slot's sampling params, so it grows with parallel, but never near this.
const maxBody = 4 << 20

// get fetches http://<addr><path>. The caller's context is the only deadline.
// The body is read through maxBody, then the rest drained and closed, so the
// connection goes back to the pool: the load poll runs every second per
// backend.
func (e *Engine) get(ctx context.Context, addr, path string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return 0, nil, err
	}
	client := e.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	_, _ = io.Copy(io.Discard, resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// Probe asks /health. 200 is ready. 503 is llama-server loading the model
// (its body says "Loading model"; the body is not interpreted). Any other
// status is not ready with a reason. Only a transport failure is an error,
// which the supervisor treats as a hard failure (spec C2).
func (e *Engine) Probe(ctx context.Context, addr string) (engine.Probe, error) {
	status, _, err := e.get(ctx, addr, "/health")
	if err != nil {
		return engine.Probe{}, err
	}
	switch status {
	case http.StatusOK:
		return engine.Probe{Ready: true, Progress: -1}, nil
	case http.StatusServiceUnavailable:
		return engine.Probe{Phase: "loading", Progress: -1}, nil
	default:
		return engine.Probe{Progress: -1, Reason: fmt.Sprintf("health returned HTTP %d", status)}, nil
	}
}

// slotEntry is the part of a /slots entry this engine reads. An idle entry
// carries only id, n_ctx, is_processing and speculative (llama-server build
// 10853); a busy one adds next_token with the decode progress.
type slotEntry struct {
	NCtx         int64 `json:"n_ctx"`
	IsProcessing bool  `json:"is_processing"`
	NextToken    []struct {
		NDecoded int64 `json:"n_decoded"`
		NRemain  int64 `json:"n_remain"`
	} `json:"next_token"`
}

// Load reads occupancy from /slots.
func (e *Engine) Load(ctx context.Context, s engine.Spec, addr string) (engine.Load, error) {
	l, _, _, err := e.LoadProgress(ctx, s, addr)
	return l, err
}

// LoadProgress reads occupancy and decode progress from one /slots request.
// Progress sums next_token[0] over processing slots only: an idle slot can
// still carry the next_token of its last request, which is not work in flight
// (v1 ReadSlots). /slots reports no queue, so Waiting is 0.
// llama.cpp reports both Slots and CtxPerSlot itself, so the Spec is unused
// here: what /slots says is what the backend will serve, and a disagreement
// with the configured parallel or context is exactly what the node should
// publish rather than hide.
func (e *Engine) LoadProgress(ctx context.Context, _ engine.Spec, addr string) (load engine.Load, decoded, remain int64, err error) {
	status, body, err := e.get(ctx, addr, "/slots")
	if err != nil {
		return engine.Load{}, 0, 0, err
	}
	if status != http.StatusOK {
		// 501 when llama-server was started without --slots.
		return engine.Load{}, 0, 0, fmt.Errorf("slots returned HTTP %d", status)
	}
	var slots []slotEntry
	if err := json.Unmarshal(body, &slots); err != nil {
		return engine.Load{}, 0, 0, fmt.Errorf("decoding /slots: %w", err)
	}
	load.Slots = len(slots)
	for _, s := range slots {
		load.CtxPerSlot = max(load.CtxPerSlot, s.NCtx)
		if !s.IsProcessing {
			continue
		}
		load.Busy++
		if len(s.NextToken) > 0 {
			decoded += s.NextToken[0].NDecoded
			remain += s.NextToken[0].NRemain
		}
	}
	return load, decoded, remain, nil
}
