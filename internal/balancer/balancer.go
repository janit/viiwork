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
		b.logger.Printf("[debug] Pick: no healthy backends (total=%d)", len(b.backends))
		for _, be := range b.backends {
			b.logger.Printf("[debug]   gpu-%d status=%s in_flight=%d", be.GPUID, be.Status(), be.InFlight())
		}
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
		b.logger.Printf("[debug] Pick: backpressure — all %d healthy backends at max_in_flight=%d", len(healthy), b.maxInFlightPerGPU)
		for _, be := range healthy {
			b.logger.Printf("[debug]   gpu-%d in_flight=%d", be.GPUID, be.InFlight())
		}
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
		picked := pickLowestLatency(idle)
		b.logger.Printf("[debug] Pick: low-load path, picked gpu-%d (idle=%d busy=%d)", picked.GPUID, len(idle), busyCount)
		return picked, nil
	}
	picked := pickLeastLoaded(healthy)
	b.logger.Printf("[debug] Pick: high-load path, picked gpu-%d (in_flight=%d healthy=%d busy=%d)", picked.GPUID, picked.InFlight(), len(healthy), busyCount)
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

func (b *Balancer) MaxInFlightPerGPU() int         { return int(b.maxInFlightPerGPU) }
func (b *Balancer) Backends() []*BackendState       { return b.backends }

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
