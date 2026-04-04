package gpu

import "testing"

func TestHistoryRecord(t *testing.T) {
	h := NewHistory(5)
	h.Record(GPUSample{GPUID: 0, Utilization: 50, Timestamp: 100})
	h.Record(GPUSample{GPUID: 0, Utilization: 60, Timestamp: 105})
	samples := h.Samples(0)
	if len(samples) != 2 { t.Fatalf("expected 2, got %d", len(samples)) }
	if samples[0].Utilization != 50 { t.Errorf("expected 50, got %f", samples[0].Utilization) }
	if samples[1].Utilization != 60 { t.Errorf("expected 60, got %f", samples[1].Utilization) }
}

func TestHistoryRingOverwrite(t *testing.T) {
	h := NewHistory(3)
	for i := range 5 { h.Record(GPUSample{GPUID: 0, Utilization: float64(i * 10), Timestamp: int64(i)}) }
	samples := h.Samples(0)
	if len(samples) != 3 { t.Fatalf("expected 3, got %d", len(samples)) }
	if samples[0].Utilization != 20 { t.Errorf("expected 20 (oldest), got %f", samples[0].Utilization) }
	if samples[2].Utilization != 40 { t.Errorf("expected 40 (newest), got %f", samples[2].Utilization) }
}

func TestHistoryChronologicalOrder(t *testing.T) {
	h := NewHistory(3)
	h.Record(GPUSample{GPUID: 0, Timestamp: 100})
	h.Record(GPUSample{GPUID: 0, Timestamp: 105})
	h.Record(GPUSample{GPUID: 0, Timestamp: 110})
	h.Record(GPUSample{GPUID: 0, Timestamp: 115})
	samples := h.Samples(0)
	if samples[0].Timestamp != 105 { t.Errorf("expected 105, got %d", samples[0].Timestamp) }
	if samples[2].Timestamp != 115 { t.Errorf("expected 115, got %d", samples[2].Timestamp) }
}

func TestHistoryMultipleGPUs(t *testing.T) {
	h := NewHistory(10)
	h.Record(GPUSample{GPUID: 0, Utilization: 50})
	h.Record(GPUSample{GPUID: 1, Utilization: 70})
	all := h.AllGPUSamples()
	if len(all) != 2 { t.Fatalf("expected 2 GPUs, got %d", len(all)) }
	if len(all[0]) != 1 { t.Errorf("gpu0: expected 1, got %d", len(all[0])) }
	if len(all[1]) != 1 { t.Errorf("gpu1: expected 1, got %d", len(all[1])) }
}

func TestHistoryEmptyGPU(t *testing.T) {
	h := NewHistory(10)
	samples := h.Samples(99)
	if len(samples) != 0 { t.Errorf("expected 0, got %d", len(samples)) }
}

func TestHistorySamplesReturnsCopy(t *testing.T) {
	h := NewHistory(10)
	h.Record(GPUSample{GPUID: 0, Utilization: 50})
	s1 := h.Samples(0)
	s1[0].Utilization = 999
	s2 := h.Samples(0)
	if s2[0].Utilization != 50 { t.Errorf("expected 50 (original), got %f", s2[0].Utilization) }
}
