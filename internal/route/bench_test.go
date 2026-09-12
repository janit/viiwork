package route

import (
	"fmt"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

// BenchmarkPick is the per-request routing cost: 3 local models of 4 backends
// and 10 peer reports of 3 models each, Pick then Release.
func BenchmarkPick(b *testing.B) {
	local := newFakeLocal()
	for m := 0; m < 3; m++ {
		name := fmt.Sprintf("m%d", m)
		for i := 0; i < 4; i++ {
			local.add(name, newFakeBackend(fmt.Sprintf("%s/%d", name, i), 2))
		}
	}
	reports := &fakeReports{}
	now := time.Now()
	for n := 0; n < 10; n++ {
		var models []meshapi.ModelCapacity
		for m := 0; m < 3; m++ {
			models = append(models, meshapi.ModelCapacity{Name: fmt.Sprintf("m%d", m), Slots: 4, Busy: 1, HealthyBackends: 2})
		}
		reports.reports = append(reports.reports, report(fmt.Sprintf("peer%d", n), now, time.Millisecond, models...))
	}
	r := New(Config{Self: "self", Local: local, Remote: reports, StaleAfter: time.Hour, QueueMax: 64, QueueTimeout: time.Second})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l, err := r.Pick(Request{Model: "m1"})
		if err != nil {
			b.Fatal(err)
		}
		l.Release()
	}
}
