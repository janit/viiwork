package balancer

import (
	"errors"
	"log"
	"os"
	"sync/atomic"

	"github.com/janit/viiwork/internal/logging"
)

var (
	ErrBackpressure     = errors.New("all backends at capacity")
	ErrNoHealthyBackend = errors.New("no healthy backend available")
)

type Balancer struct {
	backends          []*BackendState
	highLoadThreshold int
	maxInFlightPerGPU int64
	logger            *log.Logger
	// lastHealthy makes the single-backend warning edge-triggered.
	lastHealthy atomic.Int32
}

func New(backends []*BackendState, highLoadThreshold int, maxInFlightPerGPU int) *Balancer {
	return &Balancer{
		backends:          backends,
		highLoadThreshold: highLoadThreshold,
		maxInFlightPerGPU: int64(maxInFlightPerGPU),
		logger:            log.New(os.Stdout, "[balancer] ", log.LstdFlags),
	}
}

func (b *Balancer) Pick() (*BackendState, error) {
	healthy := make([]*BackendState, 0, len(b.backends))
	for _, be := range b.backends {
		if be.Status() == StatusHealthy {
			healthy = append(healthy, be)
		}
	}
	if len(healthy) == 0 {
		if logging.DebugEnabled() {
			b.logger.Printf("[debug] Pick: no healthy backends (total=%d)", len(b.backends))
			for _, be := range b.backends {
				b.logger.Printf("[debug]   %s status=%s in_flight=%d", be.Label(), be.Status(), be.InFlight())
			}
		}
		return nil, ErrNoHealthyBackend
	}
	// Edge-triggered, not level-triggered. This used to fire on EVERY request
	// while only one backend was healthy — which is the steady state for any
	// single-backend deployment, so it logged once per request forever and
	// buried real events. Now it reports the transition only.
	if prev := b.lastHealthy.Swap(int32(len(healthy))); prev != int32(len(healthy)) && len(healthy) == 1 {
		b.logger.Printf("WARNING: only 1 healthy backend remaining (%s)", healthy[0].Label())
	}
	allAtMax := true
	for _, be := range healthy {
		if be.InFlight() < b.maxInFlightPerGPU {
			allAtMax = false
			break
		}
	}
	if allAtMax {
		if logging.DebugEnabled() {
			b.logger.Printf("[debug] Pick: backpressure — all %d healthy backends at max_in_flight=%d", len(healthy), b.maxInFlightPerGPU)
			for _, be := range healthy {
				b.logger.Printf("[debug]   %s in_flight=%d", be.Label(), be.InFlight())
			}
		}
		return nil, ErrBackpressure
	}
	busyCount := 0
	idle := make([]*BackendState, 0, len(healthy))
	for _, be := range healthy {
		if be.InFlight() > 0 {
			busyCount++
		} else {
			idle = append(idle, be)
		}
	}
	if busyCount < b.highLoadThreshold && len(idle) > 0 {
		picked := pickLowestLatency(idle)
		if logging.DebugEnabled() {
			b.logger.Printf("[debug] Pick: low-load path, picked %s (idle=%d busy=%d)", picked.Label(), len(idle), busyCount)
		}
		return picked, nil
	}
	picked := pickLeastLoaded(healthy)
	if logging.DebugEnabled() {
		b.logger.Printf("[debug] Pick: high-load path, picked %s (in_flight=%d healthy=%d busy=%d)", picked.Label(), picked.InFlight(), len(healthy), busyCount)
	}
	return picked, nil
}

func pickLowestLatency(backends []*BackendState) *BackendState {
	best := backends[0]
	for _, be := range backends[1:] {
		if be.LatencyAvg() < best.LatencyAvg() {
			best = be
		}
	}
	return best
}

func (b *Balancer) MaxInFlightPerGPU() int    { return int(b.maxInFlightPerGPU) }
func (b *Balancer) Backends() []*BackendState { return b.backends }

func pickLeastLoaded(backends []*BackendState) *BackendState {
	best := backends[0]
	for _, be := range backends[1:] {
		if be.InFlight() < best.InFlight() {
			best = be
		} else if be.InFlight() == best.InFlight() && be.LatencyAvg() < best.LatencyAvg() {
			best = be
		}
	}
	return best
}
