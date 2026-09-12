package gpu

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

const (
	nvidiaStatsPowerCmd   = "nvidia-smi --query-gpu=index,utilization.gpu,memory.used,memory.total,power.draw --format=csv,noheader,nounits"
	nvidiaStatsNoPowerCmd = "nvidia-smi --query-gpu=index,utilization.gpu,memory.used,memory.total --format=csv,noheader,nounits"

	// node-a-shaped, 2026-09-10, with one garbage row.
	nvidiaStatsThreeRows = "0, 37, 11148, 16376, 71.30\n1, 0, 13528, 16376, [N/A]\nnot a row\n2, [N/A], 13918, 16376, 18.95\n"
	nvidiaStatsTwoNoPow  = "0, 37, 11148, 16376\n1, 5, 13528, 16376\n"
)

func TestParseNVIDIAStats(t *testing.T) {
	got := parseNVIDIAStats([]byte(nvidiaStatsThreeRows), 1789000000)
	want := []GPUSample{
		{GPUID: 0, Utilization: 37, VRAMUsedMB: 11148, VRAMTotalMB: 16376, PowerW: 71.3, Timestamp: 1789000000},
		{GPUID: 1, Utilization: 0, VRAMUsedMB: 13528, VRAMTotalMB: 16376, PowerW: 0, Timestamp: 1789000000},
		{GPUID: 2, Utilization: 0, VRAMUsedMB: 13918, VRAMTotalMB: 16376, PowerW: 18.95, Timestamp: 1789000000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseNVIDIAStats =\n %+v\nwant\n %+v", got, want)
	}

	skipped := parseNVIDIAStats([]byte("x, 1, 2, 3\n0, 1, 2\n0, 1, 2, [N/A]\n"), 1)
	if len(skipped) != 0 {
		t.Errorf("rows with a bad index, too few fields or no memory.total must be skipped, got %+v", skipped)
	}
}

func TestNVIDIACollectorPowerRejected(t *testing.T) {
	h := NewHistory(10)
	c := NewNVIDIACollector(h, NewBroadcaster(), fakeRunner(map[string]string{nvidiaStatsNoPowerCmd: nvidiaStatsTwoNoPow}))
	if !c.Available() || c.PowerAvailable() {
		t.Fatalf("available=%v powerAvailable=%v, want true/false", c.Available(), c.PowerAvailable())
	}
	c.Sample(context.Background())
	if n := len(h.Latest()); n != 2 {
		t.Errorf("history holds %d GPUs, want 2", n)
	}
}

func TestNVIDIACollectorPowerReported(t *testing.T) {
	c := NewNVIDIACollector(NewHistory(10), NewBroadcaster(), fakeRunner(map[string]string{nvidiaStatsPowerCmd: nvidiaStatsThreeRows}))
	if !c.Available() || !c.PowerAvailable() {
		t.Errorf("available=%v powerAvailable=%v, want true/true", c.Available(), c.PowerAvailable())
	}
}

func TestNVIDIACollectorPowerAllNA(t *testing.T) {
	out := "0, 37, 11148, 16376, [N/A]\n1, 5, 13528, 16376, [N/A]\n"
	c := NewNVIDIACollector(NewHistory(10), NewBroadcaster(), fakeRunner(map[string]string{nvidiaStatsPowerCmd: out}))
	if !c.Available() || c.PowerAvailable() {
		t.Errorf("available=%v powerAvailable=%v, want true/false: power.draw accepted but every card [N/A]", c.Available(), c.PowerAvailable())
	}
}

func TestNVIDIACollectorUnavailable(t *testing.T) {
	h := NewHistory(10)
	c := NewNVIDIACollector(h, NewBroadcaster(), fakeRunner(nil))
	if c.Available() {
		t.Error("with nvidia-smi failing the collector must be unavailable")
	}
	c.Sample(context.Background())
	if n := len(h.Latest()); n != 0 {
		t.Errorf("an unavailable collector recorded %d GPUs", n)
	}
}

func TestNVIDIACollectorBroadcast(t *testing.T) {
	b := NewBroadcaster()
	c := NewNVIDIACollector(NewHistory(10), b, fakeRunner(map[string]string{nvidiaStatsNoPowerCmd: nvidiaStatsTwoNoPow}))
	// Subscribe after construction: construction samples once, like the ROCm
	// collector, and this test is about one Sample call.
	ch := b.Subscribe()
	c.Sample(context.Background())
	if n := len(ch); n != 1 {
		t.Fatalf("got %d messages, want exactly 1", n)
	}
	var event struct {
		T    int64 `json:"t"`
		GPUs map[string]struct {
			Util       float64 `json:"util"`
			VRAMUsedMB float64 `json:"vram_used_mb"`
		} `json:"gpus"`
	}
	if err := json.Unmarshal(<-ch, &event); err != nil {
		t.Fatal(err)
	}
	if event.T == 0 || len(event.GPUs) != 2 || event.GPUs["0"].Util != 37 || event.GPUs["1"].VRAMUsedMB != 13528 {
		t.Errorf("event = %+v, want the ROCm collector's shape with GPUs 0 and 1", event)
	}
}

func TestInventory(t *testing.T) {
	ctx := context.Background()
	list := "GPU 0: NVIDIA RTX A4000 (UUID: GPU-9ba7c87c-b150-9024-77d4-b1ab37cafc85)\n" +
		"GPU 1: NVIDIA RTX A4000 (UUID: GPU-46fd522a-c783-a5a9-47bb-f55690ad9168)\n" +
		"GPU 2: NVIDIA RTX A4000 (UUID: GPU-3efd28c0-dc4a-2660-edee-c7616d740da2)\n"
	got, err := Inventory(ctx, VendorNVIDIA, fakeRunner(map[string]string{"nvidia-smi -L": list}))
	if err != nil {
		t.Fatal(err)
	}
	want := []Identity{
		{Index: 0, UUID: "GPU-9ba7c87c-b150-9024-77d4-b1ab37cafc85", Name: "NVIDIA RTX A4000"},
		{Index: 1, UUID: "GPU-46fd522a-c783-a5a9-47bb-f55690ad9168", Name: "NVIDIA RTX A4000"},
		{Index: 2, UUID: "GPU-3efd28c0-dc4a-2660-edee-c7616d740da2", Name: "NVIDIA RTX A4000"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Inventory = %+v, want %+v", got, want)
	}

	for _, v := range []Vendor{VendorAMD, VendorNone} {
		if got, err := Inventory(ctx, v, failingRunner(t)); got != nil || err != nil {
			t.Errorf("Inventory(%s) = %v, %v; want nil, nil", v, got, err)
		}
	}
	if _, err := Inventory(ctx, VendorNVIDIA, fakeRunner(nil)); err == nil {
		t.Error("a failing nvidia-smi -L must be an error")
	}
}

func TestNewCollectorNone(t *testing.T) {
	if c := NewCollector(VendorNone, NewHistory(1), NewBroadcaster()); c != nil {
		t.Errorf("NewCollector(none) = %v, want nil", c)
	}
}
