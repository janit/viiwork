package power

import (
	"context"
	"log"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

type cmdFunc func(ctx context.Context) ([]byte, error)

type Sampler struct {
	mu         sync.RWMutex
	lastWatts  float64
	available  atomic.Bool
	logger     *log.Logger
	cmdFactory cmdFunc
}

func NewSampler() *Sampler {
	s := &Sampler{
		logger: log.New(os.Stdout, "[power] ", log.LstdFlags),
		cmdFactory: func(ctx context.Context) ([]byte, error) {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			return exec.CommandContext(ctx, "ipmitool", "sdr", "type", "Power Supply").Output()
		},
	}

	// Test call to check availability
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := s.cmdFactory(ctx)
	if err != nil {
		s.available.Store(false)
		s.logger.Printf("IPMI unavailable: %v (power monitoring disabled)", err)
	} else {
		s.available.Store(true)
		s.logger.Println("IPMI available, power monitoring enabled")
		s.Sample(context.Background())
	}

	return s
}

func (s *Sampler) logf(format string, v ...any) {
	if s.logger != nil {
		s.logger.Printf(format, v...)
	}
}

func (s *Sampler) Sample(ctx context.Context) {
	if !s.available.Load() {
		return
	}
	out, err := s.cmdFactory(ctx)
	if err != nil {
		s.logf("ipmitool failed: %v (keeping last value: %.0fW)", err, s.Watts())
		return
	}
	watts := ParseWatts(string(out))
	s.mu.Lock()
	s.lastWatts = watts
	s.mu.Unlock()
}

func (s *Sampler) Watts() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastWatts
}

func (s *Sampler) Available() bool {
	return s.available.Load()
}
