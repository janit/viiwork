package vllm

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/janit/viiwork/v2/internal/engine"
)

var errNoGPUs = errors.New("vllm: a backend needs at least one GPU (vllm does not run on CPU)")

// Probe reads /health. vLLM answers only once it is ready — it does not answer
// 200 while loading, the way FreeToken does — so the status code is the whole
// readiness rule and there is no body to inspect.
//
// Progress is -1, never 0: vLLM reports no load progress, and 0 would render
// as "0% loaded" on a dashboard rather than as "unknown".
func (e *Engine) Probe(ctx context.Context, addr string) (engine.Probe, error) {
	resp, err := e.get(ctx, addr, "/health")
	if err != nil {
		// Transport failure only: refused, EOF, timeout, cancelled.
		return engine.Probe{}, err
	}
	defer drain(resp)

	if resp.StatusCode == http.StatusOK {
		return engine.Probe{Ready: true, Progress: -1}, nil
	}
	// An answer that is not 200 is a successful probe reporting "not ready".
	// Returning an error here would stop the node telling "loading" from
	// "gone".
	return engine.Probe{Ready: false, Progress: -1, Reason: resp.Status}, nil
}

// Load scrapes /metrics.
//
// Slots and CtxPerSlot restate what the node asked for (Decisions 2 and 6), so
// a backend whose engine silently clamped a value shows up as a disagreement
// rather than vanishing. vLLM reports neither, so both come from the Spec the
// node handed in.
func (e *Engine) Load(ctx context.Context, s engine.Spec, addr string) (engine.Load, error) {
	resp, err := e.get(ctx, addr, "/metrics")
	if err != nil {
		return engine.Load{}, err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return engine.Load{}, fmt.Errorf("vllm: /metrics answered %s", resp.Status)
	}
	// Bounded read: /metrics grows with histogram cardinality and this runs
	// every second.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return engine.Load{}, err
	}

	st := parseStats(body)
	if !st.found["running"] {
		// Decision 3. A zero would freeze "idle" into the mesh for a busy
		// backend; an error degrades to the node's in-flight count, visibly.
		return engine.Load{}, errors.New("vllm: no running-sequence gauge in this build's /metrics")
	}
	return engine.Load{
		Slots:      s.Parallel,
		Busy:       int(st.numRunning),
		Waiting:    int(st.numWaiting),
		CtxPerSlot: int64(s.Context),
	}, nil
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

// stats is the subset of vLLM's Prometheus output this engine acts on.
type stats struct {
	numRunning int64
	numWaiting int64
	// kvCachePct is fractional KV cache occupancy, 0..1. It is the single most
	// useful number vLLM reports — a backend at 0.95 is about to start
	// preempting, which shows up as latency long before it shows up as an
	// error — and meshapi has no field for it, so it is parsed, kept accurate
	// and not published. That is the obvious next additive wire field.
	kvCachePct float64
	// found records which gauges actually appeared. vLLM renames metrics
	// between engine versions, and a consumer must be able to tell "zero
	// requests running" from "this build does not publish that gauge".
	found map[string]bool
}

// Metric name aliases, newest first. The V1 engine publishes
// kv_cache_usage_perc where V0 published gpu_cache_usage_perc, and a node is
// expected to work against whichever vLLM the operator installed.
var (
	runningNames = []string{"vllm:num_requests_running"}
	waitingNames = []string{"vllm:num_requests_waiting"}
	kvCacheNames = []string{"vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc"}
)

// parseStats reads Prometheus text format.
//
// A vLLM server may expose several series for one metric — one per engine
// under data parallelism, each with its own label set. Counts are summed
// because the backend's load is the total, while KV occupancy takes the
// maximum: it is a pressure signal, and the engine closest to preemption is
// the one that will make the backend slow.
func parseStats(body []byte) stats {
	st := stats{found: map[string]bool{}}
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		metric, val, ok := parseSample(line)
		if !ok {
			continue
		}
		switch {
		case matches(metric, runningNames):
			st.numRunning += int64(val)
			st.found["running"] = true
		case matches(metric, waitingNames):
			st.numWaiting += int64(val)
			st.found["waiting"] = true
		case matches(metric, kvCacheNames):
			if val > st.kvCachePct {
				st.kvCachePct = val
			}
			st.found["kv_cache"] = true
		}
	}
	return st
}

func matches(metric string, candidates []string) bool {
	for _, c := range candidates {
		if metric == c {
			return true
		}
	}
	return false
}

// parseSample splits one Prometheus sample line into its metric name and
// value, discarding labels. A histogram's _bucket/_sum/_count suffixes are
// left attached, so they simply fail to match any name we look for.
func parseSample(line string) (metric string, value float64, ok bool) {
	// name{labels} value  |  name value
	if i := strings.IndexByte(line, '{'); i >= 0 {
		metric = line[:i]
		j := strings.LastIndexByte(line, '}')
		if j < i {
			return "", 0, false
		}
		line = strings.TrimSpace(line[j+1:])
	} else {
		i := strings.IndexAny(line, " \t")
		if i < 0 {
			return "", 0, false
		}
		metric = line[:i]
		line = strings.TrimSpace(line[i:])
	}
	// A sample may carry a trailing timestamp; the value is the first field.
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		line = line[:i]
	}
	v, err := strconv.ParseFloat(line, 64)
	if err != nil {
		return "", 0, false
	}
	return metric, v, true
}
