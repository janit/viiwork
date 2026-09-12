package proxy

import (
	"sync"
	"sync/atomic"
)

// ModelCounters is one model's cumulative requests and completion tokens since
// the node started (C4 requests_total, tokens_total).
type ModelCounters struct {
	Requests uint64
	Tokens   uint64
}

// Counters counts requests executed on this node, per real model. The origin
// of a forwarded request never counts it, so the fleet never double counts.
type Counters struct {
	mu     sync.RWMutex
	models map[string]*modelCounter
}

type modelCounter struct {
	requests atomic.Uint64
	tokens   atomic.Uint64
}

func NewCounters() *Counters { return &Counters{models: map[string]*modelCounter{}} }

func (c *Counters) counter(model string) *modelCounter {
	c.mu.RLock()
	mc, ok := c.models[model]
	c.mu.RUnlock()
	if ok {
		return mc
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if mc, ok = c.models[model]; !ok {
		mc = &modelCounter{}
		c.models[model] = mc
	}
	return mc
}

// Add counts one completed request and its completion tokens (0 when the
// response carried no usage). It allocates only the first time a model is seen.
func (c *Counters) Add(model string, completionTokens int64) {
	mc := c.counter(model)
	mc.requests.Add(1)
	if completionTokens > 0 {
		mc.tokens.Add(uint64(completionTokens))
	}
}

func (c *Counters) Get(model string) ModelCounters {
	c.mu.RLock()
	mc, ok := c.models[model]
	c.mu.RUnlock()
	if !ok {
		return ModelCounters{}
	}
	return ModelCounters{Requests: mc.requests.Load(), Tokens: mc.tokens.Load()}
}

func (c *Counters) Snapshot() map[string]ModelCounters {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]ModelCounters, len(c.models))
	for name, mc := range c.models {
		out[name] = ModelCounters{Requests: mc.requests.Load(), Tokens: mc.tokens.Load()}
	}
	return out
}
