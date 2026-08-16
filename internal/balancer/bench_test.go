package balancer

import (
	"fmt"
	"testing"
	"time"
)

func benchBalancer(n int) *Balancer {
	states := make([]*BackendState, n)
	for i := 0; i < n; i++ {
		s := NewBackendState(i, fmt.Sprintf("localhost:%d", 9851+i))
		s.SetStatus(StatusHealthy)
		s.RecordLatency(time.Duration(10+i)*time.Millisecond, 30*time.Second)
		states[i] = s
	}
	return New(states, 8, 2)
}

// BenchmarkPick covers the per-request routing decision. n=5 matches the
// current gb1 fleet; n=10 is the ceiling this host can reach.
func BenchmarkPick(b *testing.B) {
	for _, n := range []int{5, 10} {
		bal := benchBalancer(n)
		b.Run(fmt.Sprintf("backends=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				bal.Pick()
			}
		})
	}
}

// BenchmarkRecordLatency covers the mutex-guarded rolling window updated once
// per completed request.
func BenchmarkRecordLatency(b *testing.B) {
	s := NewBackendState(0, "localhost:9851")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.RecordLatency(12*time.Millisecond, 30*time.Second)
	}
}

// BenchmarkRecordLatencyParallel measures contention when several backends'
// requests complete concurrently on the same state.
func BenchmarkRecordLatencyParallel(b *testing.B) {
	s := NewBackendState(0, "localhost:9851")
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s.RecordLatency(12*time.Millisecond, 30*time.Second)
		}
	})
}
