package balancer

import (
	"strconv"
	"strings"
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
	GPUID    int
	GPUIDs   []int // populated in tensor-split mode; empty otherwise
	Addr     string
	inFlight atomic.Int64
	status   atomic.Int32
	// hardFailure latches when the inference path observes an unambiguous
	// socket-level failure (EOF / connection refused on a proxied request).
	// The manager's health loop reads it on the next tick and short-circuits
	// the failure-count ladder so respawn fires after one failed probe instead
	// of MaxFailures. Cleared when a probe succeeds.
	hardFailure atomic.Bool
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

// Label returns a human-readable identifier for this backend: "gpu-4" for a
// single-GPU replica, or "ts-4,5" for a tensor-split group.
//
// Tensor-split backends carry GPUID = -1 because no single device owns them,
// so anything formatting GPUID directly renders the useless "gpu--1". That is
// harmless in a log line but not in the X-GPU-Backend response header, where
// it made every backend in a multi-group fleet indistinguishable — a 5x TS=2
// deployment returned "gpu--1" for all five, leaving no way to attribute a
// request to a backend from the client side.
//
// This mirrors the convention in process.Backend.label(). The two are separate
// because that one describes a supervised process and this one a routable
// backend, and process.Backend populates its own GPUIDs before State's are
// set. Keep the output format identical if either changes.
func (s *BackendState) Label() string {
	if len(s.GPUIDs) > 0 {
		parts := make([]string, len(s.GPUIDs))
		for i, id := range s.GPUIDs {
			parts[i] = strconv.Itoa(id)
		}
		return "ts-" + strings.Join(parts, ",")
	}
	return "gpu-" + strconv.Itoa(s.GPUID)
}

func (s *BackendState) Status() BackendStatus          { return BackendStatus(s.status.Load()) }
func (s *BackendState) SetStatus(status BackendStatus) { s.status.Store(int32(status)) }
func (s *BackendState) InFlight() int64                { return s.inFlight.Load() }
func (s *BackendState) IncrInFlight()                  { s.inFlight.Add(1) }

// NoteHardFailure marks the backend unhealthy and latches a flag the manager's
// health-tick will read. Called from the proxy when a request observes EOF or
// "connection refused" against the backend's listen port — kernel-level signals
// that the process is definitively gone. The status flip removes the backend
// from the picker immediately; the latched flag tells the manager to skip the
// 3-strike ladder and respawn after one failed probe.
func (s *BackendState) NoteHardFailure() {
	s.status.Store(int32(StatusUnhealthy))
	s.hardFailure.Store(true)
}

// HardFailureSeen reports whether NoteHardFailure has been called since the
// last successful health probe.
func (s *BackendState) HardFailureSeen() bool { return s.hardFailure.Load() }

// ClearHardFailure resets the latched flag. Called by the manager on a
// successful health probe so a transient false-positive doesn't persist into
// the next respawn cycle.
func (s *BackendState) ClearHardFailure() { s.hardFailure.Store(false) }

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

func (s *BackendState) RSSMB() int64      { return s.rssMB.Load() }
func (s *BackendState) SetRSSMB(mb int64) { s.rssMB.Store(mb) }
func (s *BackendState) SlotCtx() int64    { return s.slotCtx.Load() }
func (s *BackendState) SlotCount() int    { return int(s.slotCount.Load()) }
func (s *BackendState) SlotActive() int   { return int(s.slotActive.Load()) }
func (s *BackendState) TokDecoded() int64 { return s.tokDecoded.Load() }
func (s *BackendState) TokRemain() int64  { return s.tokRemain.Load() }
func (s *BackendState) SetSlots(nctx int64, count, active int, decoded, remain int64) {
	s.slotCtx.Store(nctx)
	s.slotCount.Store(int32(count))
	s.slotActive.Store(int32(active))
	s.tokDecoded.Store(decoded)
	s.tokRemain.Store(remain)
}
