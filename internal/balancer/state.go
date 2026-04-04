package balancer

import (
	"sync"
	"sync/atomic"
	"time"
)

type BackendStatus int

const (
	StatusStarting BackendStatus = iota
	StatusHealthy
	StatusUnhealthy
	StatusDead
)

func (s BackendStatus) String() string {
	switch s {
	case StatusStarting:
		return "starting"
	case StatusHealthy:
		return "healthy"
	case StatusUnhealthy:
		return "unhealthy"
	case StatusDead:
		return "dead"
	default:
		return "unknown"
	}
}

type BackendState struct {
	GPUID   int
	Addr    string
	inFlight atomic.Int64
	status   atomic.Int32
	mu          sync.Mutex
	latencies   []time.Duration
	latencySum  time.Duration
}

func NewBackendState(gpuID int, addr string) *BackendState {
	return &BackendState{GPUID: gpuID, Addr: addr}
}

func (s *BackendState) Status() BackendStatus { return BackendStatus(s.status.Load()) }
func (s *BackendState) SetStatus(status BackendStatus) { s.status.Store(int32(status)) }
func (s *BackendState) InFlight() int64 { return s.inFlight.Load() }
func (s *BackendState) IncrInFlight() { s.inFlight.Add(1) }

func (s *BackendState) DecrInFlight() {
	for {
		old := s.inFlight.Load()
		if old <= 0 {
			return
		}
		if s.inFlight.CompareAndSwap(old, old-1) {
			return
		}
	}
}

func (s *BackendState) RecordLatency(d time.Duration, window time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	maxSamples := int(window.Seconds()) * 2
	if maxSamples < 10 {
		maxSamples = 10
	}
	s.latencies = append(s.latencies, d)
	s.latencySum += d
	for len(s.latencies) > maxSamples {
		s.latencySum -= s.latencies[0]
		s.latencies = s.latencies[1:]
	}
}

func (s *BackendState) LatencyAvg() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.latencies) == 0 {
		return 0
	}
	return s.latencySum / time.Duration(len(s.latencies))
}
