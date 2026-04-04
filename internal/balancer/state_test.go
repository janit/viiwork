package balancer

import (
	"testing"
	"time"
)

func TestNewBackendState(t *testing.T) {
	s := NewBackendState(0, "localhost:9001")
	if s.Status() != StatusStarting {
		t.Errorf("expected starting, got %v", s.Status())
	}
	if s.InFlight() != 0 {
		t.Errorf("expected 0 in-flight, got %d", s.InFlight())
	}
}

func TestInFlightTracking(t *testing.T) {
	s := NewBackendState(0, "localhost:9001")
	s.SetStatus(StatusHealthy)
	s.IncrInFlight()
	s.IncrInFlight()
	if s.InFlight() != 2 {
		t.Errorf("expected 2 in-flight, got %d", s.InFlight())
	}
	s.DecrInFlight()
	if s.InFlight() != 1 {
		t.Errorf("expected 1 in-flight, got %d", s.InFlight())
	}
}

func TestInFlightNeverNegative(t *testing.T) {
	s := NewBackendState(0, "localhost:9001")
	s.DecrInFlight()
	if s.InFlight() != 0 {
		t.Errorf("expected 0, got %d", s.InFlight())
	}
}

func TestRecordLatency(t *testing.T) {
	s := NewBackendState(0, "localhost:9001")
	s.RecordLatency(100*time.Millisecond, 30*time.Second)
	s.RecordLatency(200*time.Millisecond, 30*time.Second)
	avg := s.LatencyAvg()
	if avg < 100*time.Millisecond || avg > 200*time.Millisecond {
		t.Errorf("expected avg between 100ms and 200ms, got %v", avg)
	}
}

func TestStatusTransitions(t *testing.T) {
	s := NewBackendState(0, "localhost:9001")
	s.SetStatus(StatusHealthy)
	if s.Status() != StatusHealthy { t.Errorf("expected healthy") }
	s.SetStatus(StatusUnhealthy)
	if s.Status() != StatusUnhealthy { t.Errorf("expected unhealthy") }
	s.SetStatus(StatusDead)
	if s.Status() != StatusDead { t.Errorf("expected dead") }
}
