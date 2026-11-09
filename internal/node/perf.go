package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/sessionshare"
)

// perfKeys hashes what makes a model's speed on this node: engine, weights,
// GPUs and args. A change drops the saved baseline instead of inheriting a
// number measured on other hardware or flags. Env is left out on purpose.
func perfKeys(models []config.Model) map[string]string {
	keys := make(map[string]string, len(models))
	for _, m := range models {
		h := sha256.New()
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%v\x00%d\x00%q", m.Engine, m.Path, m.Source, m.GPUs, m.GPUsPerBackend, m.Args)
		// What an engine says also makes its speed (engine.PerfKeyer): a
		// config file of its own that the entry only names.
		if e, ok := engine.Lookup(m.Engine); ok {
			if k, ok := e.(engine.PerfKeyer); ok {
				fmt.Fprintf(h, "\x00%s", k.PerfKey(config.ModelSpec(m)))
			}
		}
		keys[m.Name] = hex.EncodeToString(h.Sum(nil)[:8])
	}
	return keys
}

// sessionModels is every model whose engine shares sessions (kv_share) with
// the settings it reads for it.
func sessionModels(models []config.Model) []sessionshare.Model {
	var out []sessionshare.Model
	for _, m := range models {
		e, ok := engine.Lookup(m.Engine)
		if !ok {
			continue
		}
		spec := config.ModelSpec(m)
		switch se := e.(type) {
		case engine.SessionSharer:
			if cfg, ok := se.SessionShare(spec); ok {
				out = append(out, sessionshare.Model{Name: m.Name, Engine: se, Config: cfg})
			}
		case engine.SlotSharer:
			if cfg, ok := se.SessionShare(spec); ok {
				out = append(out, sessionshare.Model{Name: m.Name, Slots: se, Config: cfg})
			}
		}
	}
	return out
}

// usageReporting is how the engine serving model reports usage.
func (n *Node) usageReporting(model string) engine.UsageReporting {
	for _, m := range n.runningConfig().Models {
		if m.Name == model {
			if e, ok := engine.Lookup(m.Engine); ok {
				return engine.UsageOf(e)
			}
			break
		}
	}
	return engine.UsageReporting{}
}

// separatesReasoning reports whether the engine serving model streams
// reasoning apart from the answer.
func (n *Node) separatesReasoning(model string) bool {
	for _, m := range n.runningConfig().Models {
		if m.Name == model {
			e, ok := engine.Lookup(m.Engine)
			return ok && engine.SeparatesReasoning(e)
		}
	}
	return false
}

// perfSaveLoop writes changed baselines once a minute and once at stop. A
// failed write is logged: losing baselines costs minutes of learning, never
// service.
func (n *Node) perfSaveLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			n.savePerf()
			return
		case <-t.C:
			n.savePerf()
		}
	}
}

func (n *Node) savePerf() {
	dir := n.runningConfig().Node.StateDir
	if dir == "" {
		return
	}
	if err := n.perf.Save(dir); err != nil {
		n.logf("%v; performance baselines not saved, will retry", err) // err names perf.json
	}
}
