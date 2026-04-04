package process

import (
	"testing"

	"github.com/janit/viiwork/internal/config"
)

func TestManagerCreatesBackends(t *testing.T) {
	cfg := config.Defaults()
	cfg.GPUs.Count = 3
	cfg.Model.Path = "/models/test.gguf"
	m := NewManager(&cfg, nil, nil, nil, nil)
	if len(m.Backends) != 3 { t.Errorf("expected 3 backends, got %d", len(m.Backends)) }
	for i, b := range m.Backends {
		if b.GPUID != i { t.Errorf("backend %d: expected GPUID %d, got %d", i, i, b.GPUID) }
		if b.Port != cfg.GPUs.BasePort+i { t.Errorf("backend %d: expected port %d, got %d", i, cfg.GPUs.BasePort+i, b.Port) }
	}
}

func TestManagerStates(t *testing.T) {
	cfg := config.Defaults()
	cfg.GPUs.Count = 2
	cfg.Model.Path = "/models/test.gguf"
	m := NewManager(&cfg, nil, nil, nil, nil)
	states := m.States()
	if len(states) != 2 { t.Errorf("expected 2 states, got %d", len(states)) }
}

func TestManagerAcceptsNilSampler(t *testing.T) {
	cfg := config.Defaults()
	cfg.GPUs.Count = 1
	cfg.Model.Path = "/models/test.gguf"
	m := NewManager(&cfg, nil, nil, nil, nil)
	if m == nil {
		t.Fatal("expected non-nil manager")
	}
}

func TestManagerAcceptsNilCostTracker(t *testing.T) {
	cfg := config.Defaults()
	cfg.GPUs.Count = 1
	cfg.Model.Path = "/models/test.gguf"
	m := NewManager(&cfg, nil, nil, nil, nil)
	if m == nil { t.Fatal("expected non-nil manager") }
}
