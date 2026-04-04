package balancer

import (
	"fmt"
	"testing"
	"time"
)

func makeBackends(n int) []*BackendState {
	backends := make([]*BackendState, n)
	for i := range n {
		backends[i] = NewBackendState(i, fmt.Sprintf("localhost:%d", 9001+i))
		backends[i].SetStatus(StatusHealthy)
	}
	return backends
}

func TestPickIdleGPU(t *testing.T) {
	backends := makeBackends(3)
	backends[1].RecordLatency(50*time.Millisecond, 30*time.Second)
	backends[0].RecordLatency(100*time.Millisecond, 30*time.Second)
	backends[2].RecordLatency(200*time.Millisecond, 30*time.Second)
	b := New(backends, 7, 4)
	picked, err := b.Pick()
	if err != nil { t.Fatal(err) }
	if picked.GPUID != 1 {
		t.Errorf("expected GPU 1 (lowest latency idle), got GPU %d", picked.GPUID)
	}
}

func TestFallsToLeastInFlight(t *testing.T) {
	backends := makeBackends(3)
	backends[0].IncrInFlight(); backends[0].IncrInFlight()
	backends[1].IncrInFlight()
	backends[2].IncrInFlight(); backends[2].IncrInFlight(); backends[2].IncrInFlight()
	b := New(backends, 7, 4)
	picked, err := b.Pick()
	if err != nil { t.Fatal(err) }
	if picked.GPUID != 1 {
		t.Errorf("expected GPU 1 (least in-flight), got GPU %d", picked.GPUID)
	}
}

func TestHeavyLoadMode(t *testing.T) {
	backends := makeBackends(10)
	for i := range 7 {
		backends[i].IncrInFlight(); backends[i].IncrInFlight()
	}
	b := New(backends, 7, 4)
	picked, err := b.Pick()
	if err != nil { t.Fatal(err) }
	if picked.InFlight() != 0 {
		t.Errorf("expected idle GPU, got GPU %d with %d in-flight", picked.GPUID, picked.InFlight())
	}
}

func TestBackpressure429(t *testing.T) {
	backends := makeBackends(2)
	for range 4 { backends[0].IncrInFlight(); backends[1].IncrInFlight() }
	b := New(backends, 7, 4)
	_, err := b.Pick()
	if err != ErrBackpressure {
		t.Errorf("expected ErrBackpressure, got %v", err)
	}
}

func TestAllUnhealthy503(t *testing.T) {
	backends := makeBackends(2)
	backends[0].SetStatus(StatusUnhealthy)
	backends[1].SetStatus(StatusDead)
	b := New(backends, 7, 4)
	_, err := b.Pick()
	if err != ErrNoHealthyBackend {
		t.Errorf("expected ErrNoHealthyBackend, got %v", err)
	}
}

func TestSkipsUnhealthyBackends(t *testing.T) {
	backends := makeBackends(3)
	backends[0].SetStatus(StatusUnhealthy)
	backends[1].SetStatus(StatusDead)
	b := New(backends, 7, 4)
	picked, err := b.Pick()
	if err != nil { t.Fatal(err) }
	if picked.GPUID != 2 {
		t.Errorf("expected GPU 2 (only healthy), got GPU %d", picked.GPUID)
	}
}
