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
	GPUIDs  []int // populated in tensor-split mode; empty otherwise
	Addr    string
	inFlight atomic.Int64
	status   atomic.Int32
	rssMB       atomic.Int64
	slotCtx     atomic.Int64
	slotCount   atomic.Int32
	slotActive  atomic.Int32
	tokDecoded  atomic.Int64
	tokRemain   atomic.Int64
	mu          sync.Mutex
	latencies   []time.Duration
	latHead     int
	latCount    int
	latMax      int
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
	if s.latMax != maxSamples {
		s.latencies = make([]time.Duration, maxSamples)
		s.latHead = 0
		s.latCount = 0
		s.latencySum = 0
		s.latMax = maxSamples
	}
	if s.latCount == s.latMax {
		s.latencySum -= s.latencies[s.latHead]
	}
	s.latencies[s.latHead] = d
	s.latencySum += d
	s.latHead = (s.latHead + 1) % s.latMax
	if s.latCount < s.latMax {
		s.latCount++
	}
}

func (s *BackendState) LatencyAvg() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latCount == 0 {
		return 0
	}
	return s.latencySum / time.Duration(s.latCount)
}

func (s *BackendState) RSSMB() int64        { return s.rssMB.Load() }
func (s *BackendState) SetRSSMB(mb int64)    { s.rssMB.Store(mb) }
func (s *BackendState) SlotCtx() int64       { return s.slotCtx.Load() }
func (s *BackendState) SlotCount() int       { return int(s.slotCount.Load()) }
func (s *BackendState) SlotActive() int      { return int(s.slotActive.Load()) }
func (s *BackendState) TokDecoded() int64    { return s.tokDecoded.Load() }
func (s *BackendState) TokRemain() int64     { return s.tokRemain.Load() }
func (s *BackendState) SetSlots(nctx int64, count, active int, decoded, remain int64) {
	s.slotCtx.Store(nctx)
	s.slotCount.Store(int32(count))
	s.slotActive.Store(int32(active))
	s.tokDecoded.Store(decoded)
	s.tokRemain.Store(remain)
}
