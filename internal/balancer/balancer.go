package balancer

import (
	"errors"
	"log"
	"os"
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
	var healthy []*BackendState
	for _, be := range b.backends {
		if be.Status() == StatusHealthy {
			healthy = append(healthy, be)
		}
	}
	if len(healthy) == 0 {
		return nil, ErrNoHealthyBackend
	}
	if len(healthy) == 1 {
		b.logger.Printf("WARNING: only 1 healthy backend remaining (gpu-%d)", healthy[0].GPUID)
	}
	allAtMax := true
	for _, be := range healthy {
		if be.InFlight() < b.maxInFlightPerGPU {
			allAtMax = false
			break
		}
	}
	if allAtMax {
		return nil, ErrBackpressure
	}
	busyCount := 0
	var idle []*BackendState
	for _, be := range healthy {
		if be.InFlight() > 0 {
			busyCount++
		} else {
			idle = append(idle, be)
		}
	}
	if busyCount < b.highLoadThreshold && len(idle) > 0 {
		return pickLowestLatency(idle), nil
	}
	return pickLeastLoaded(healthy), nil
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

func (b *Balancer) MaxInFlightPerGPU() int { return int(b.maxInFlightPerGPU) }

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
