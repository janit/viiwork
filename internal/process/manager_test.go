package process

import (
	"os"
	"testing"

	"github.com/janit/viiwork/internal/config"
)

func TestManagerCreatesBackends(t *testing.T) {
	cfg := config.Defaults()
	cfg.GPUs.Count = 3
	cfg.Model.Path = "/models/test.gguf"
	m := NewManager(&cfg, nil, nil, nil, nil, nil)
	if len(m.Backends) != 3 { t.Errorf("expected 3 backends, got %d", len(m.Backends)) }
	for i, b := range m.Backends {
		if b.GPUID != i { t.Errorf("backend %d: expected GPUID %d, got %d", i, i, b.GPUID) }
		if b.Port != cfg.GPUs.BasePort+i { t.Errorf("backend %d: expected port %d, got %d", i, cfg.GPUs.BasePort+i, b.Port) }
	}
}

func TestManagerStates(t *testing.T) {
	cfg := config.Defaults()
	cfg.GPUs.Count = 2
	cfg.Model.Path = "/models/test.gguf"
	m := NewManager(&cfg, nil, nil, nil, nil, nil)
	states := m.States()
	if len(states) != 2 { t.Errorf("expected 2 states, got %d", len(states)) }
}

func TestManagerAcceptsNilSampler(t *testing.T) {
	cfg := config.Defaults()
	cfg.GPUs.Count = 1
	cfg.Model.Path = "/models/test.gguf"
	m := NewManager(&cfg, nil, nil, nil, nil, nil)
	if m == nil {
		t.Fatal("expected non-nil manager")
	}
}

func TestManagerAcceptsNilCostTracker(t *testing.T) {
	cfg := config.Defaults()
	cfg.GPUs.Count = 1
	cfg.Model.Path = "/models/test.gguf"
	m := NewManager(&cfg, nil, nil, nil, nil, nil)
	if m == nil { t.Fatal("expected non-nil manager") }
}

func TestManagerTensorSplitSingleBackend(t *testing.T) {
	cfg := config.Defaults()
	cfg.Model.Path = "/models/big.gguf"
	cfg.GPUs.Devices = []int{4, 5, 6, 7}
	cfg.GPUs.BasePort = 9001
	cfg.GPUs.TensorSplit.Enabled = true
	cfg.GPUs.TensorSplit.Mode = "layer"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	m := NewManager(&cfg, nil, nil, nil, nil, nil)
	if len(m.Backends) != 1 {
		t.Fatalf("expected exactly 1 backend in tensor-split mode, got %d", len(m.Backends))
	}
	b := m.Backends[0]
	if !b.TensorSplit {
		t.Error("expected backend.TensorSplit=true")
	}
	if b.GPUID != -1 {
		t.Errorf("expected sentinel GPUID=-1, got %d", b.GPUID)
	}
	if len(b.GPUIDs) != 4 || b.GPUIDs[0] != 4 || b.GPUIDs[3] != 7 {
		t.Errorf("expected GPUIDs=[4,5,6,7], got %v", b.GPUIDs)
	}
	if b.SplitMode != "layer" {
		t.Errorf("expected SplitMode=layer, got %q", b.SplitMode)
	}
	if b.Port != 9001 {
		t.Errorf("expected port 9001 (single port in tensor-split mode), got %d", b.Port)
	}
	if b.Parallel != 1 {
		t.Errorf("expected parallel=1 (forced), got %d", b.Parallel)
	}
	if b.State == nil {
		t.Error("expected non-nil State")
	}
}

func TestManagerTensorSplitGroups(t *testing.T) {
	cfg := config.Defaults()
	cfg.Model.Path = "/models/big.gguf"
	cfg.GPUs.Devices = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	cfg.GPUs.BasePort = 9401
	cfg.GPUs.TensorSplit.Enabled = true
	cfg.GPUs.TensorSplit.GroupSize = 2
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	m := NewManager(&cfg, nil, nil, nil, nil, nil)
	if len(m.Backends) != 5 {
		t.Fatalf("expected 5 backends (10 devices / group_size 2), got %d", len(m.Backends))
	}
	for i, b := range m.Backends {
		if !b.TensorSplit {
			t.Errorf("backend %d: expected TensorSplit=true", i)
		}
		if b.GPUID != -1 {
			t.Errorf("backend %d: expected sentinel GPUID=-1, got %d", i, b.GPUID)
		}
		wantGPUs := []int{i * 2, i*2 + 1}
		if len(b.GPUIDs) != 2 || b.GPUIDs[0] != wantGPUs[0] || b.GPUIDs[1] != wantGPUs[1] {
			t.Errorf("backend %d: expected GPUIDs=%v, got %v", i, wantGPUs, b.GPUIDs)
		}
		if b.Port != 9401+i {
			t.Errorf("backend %d: expected port %d, got %d", i, 9401+i, b.Port)
		}
		if b.Parallel != 1 {
			t.Errorf("backend %d: expected parallel=1 (forced), got %d", i, b.Parallel)
		}
	}
}

func TestManagerTensorSplitGroupsValidation(t *testing.T) {
	cases := []struct {
		name      string
		devices   []int
		groupSize int
		wantErr   bool
	}{
		{"even_split_ok", []int{0, 1, 2, 3}, 2, false},
		{"group_size_zero_single_group", []int{0, 1, 2, 3}, 0, false},
		{"group_size_equals_devices", []int{0, 1}, 2, false},
		{"uneven_rejected", []int{0, 1, 2}, 2, true},
		{"group_size_one_rejected", []int{0, 1, 2, 3}, 1, true},
		{"group_size_too_big_rejected", []int{0, 1}, 4, true},
		{"group_size_negative_rejected", []int{0, 1}, -1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.Model.Path = "/models/x.gguf"
			cfg.GPUs.Devices = tc.devices
			cfg.GPUs.BasePort = 9001
			cfg.GPUs.TensorSplit.Enabled = true
			cfg.GPUs.TensorSplit.GroupSize = tc.groupSize
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestManagerReplicaModeUnchanged(t *testing.T) {
	// Sanity check: configs without tensor_split still produce N backends.
	cfg := config.Defaults()
	cfg.Model.Path = "/models/test.gguf"
	cfg.GPUs.Devices = []int{0, 1, 2}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	m := NewManager(&cfg, nil, nil, nil, nil, nil)
	if len(m.Backends) != 3 {
		t.Errorf("expected 3 replica backends, got %d", len(m.Backends))
	}
	for _, b := range m.Backends {
		if b.TensorSplit {
			t.Error("replica backend should have TensorSplit=false")
		}
		if b.GPUID < 0 {
			t.Errorf("replica backend should have GPUID>=0, got %d", b.GPUID)
		}
	}
}

func TestHasMmapArgHelpers(t *testing.T) {
	cases := []struct {
		args    []string
		hasNo   bool
		hasYes  bool
	}{
		{nil, false, false},
		{[]string{}, false, false},
		{[]string{"--no-warmup"}, false, false},
		{[]string{"--no-mmap"}, true, false},
		{[]string{"--mmap"}, false, true},
		{[]string{"--no-warmup", "--no-mmap"}, true, false},
		{[]string{"--mmap", "--no-warmup"}, false, true},
	}
	for _, tc := range cases {
		if got := hasNoMmapArg(tc.args); got != tc.hasNo {
			t.Errorf("hasNoMmapArg(%v) = %v, want %v", tc.args, got, tc.hasNo)
		}
		if got := hasExplicitMmapArg(tc.args); got != tc.hasYes {
			t.Errorf("hasExplicitMmapArg(%v) = %v, want %v", tc.args, got, tc.hasYes)
		}
	}
}

func TestApplyAutoNoMmapInjectsWhenModelTooBig(t *testing.T) {
	// Create a temp file pretending to be a 1 KiB "model"
	f, err := os.CreateTemp("", "fake-model-*.gguf")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(make([]byte, 1024)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	cfg := config.Defaults()
	cfg.Model.Path = f.Name()
	cfg.GPUs.Devices = []int{0, 1}
	cfg.GPUs.TensorSplit.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	m := NewManager(&cfg, nil, nil, nil, nil, nil)

	// Pass a fake "host RAM" of 1024 bytes — 80% threshold = 819 bytes,
	// so the 1024-byte file should trigger injection.
	m.applyAutoNoMmap(1024)

	if len(m.Backends) != 1 {
		t.Fatalf("expected 1 tensor-split backend, got %d", len(m.Backends))
	}
	if !hasNoMmapArg(m.Backends[0].ExtraArgs) {
		t.Errorf("expected --no-mmap auto-injected, got args=%v", m.Backends[0].ExtraArgs)
	}
}

func TestApplyAutoNoMmapSkipsWhenModelFits(t *testing.T) {
	f, err := os.CreateTemp("", "small-model-*.gguf")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.Write(make([]byte, 100)) // 100 bytes
	f.Close()

	cfg := config.Defaults()
	cfg.Model.Path = f.Name()
	cfg.GPUs.Devices = []int{0, 1}
	cfg.GPUs.TensorSplit.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	m := NewManager(&cfg, nil, nil, nil, nil, nil)

	// "Host RAM" = 10 KiB; 80% = 8192 bytes; 100 bytes is well under.
	m.applyAutoNoMmap(10240)

	if hasNoMmapArg(m.Backends[0].ExtraArgs) {
		t.Errorf("did not expect --no-mmap injection, got args=%v", m.Backends[0].ExtraArgs)
	}
}

func TestApplyAutoNoMmapRespectsExplicitNoMmap(t *testing.T) {
	f, err := os.CreateTemp("", "fake-model-*.gguf")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.Write(make([]byte, 1024))
	f.Close()

	cfg := config.Defaults()
	cfg.Model.Path = f.Name()
	cfg.GPUs.Devices = []int{0, 1}
	cfg.GPUs.TensorSplit.Enabled = true
	cfg.Backend.ExtraArgs = []string{"--no-mmap"} // already set
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	m := NewManager(&cfg, nil, nil, nil, nil, nil)
	before := len(m.Backends[0].ExtraArgs)

	m.applyAutoNoMmap(1024)

	// Should NOT have appended a second --no-mmap
	if len(m.Backends[0].ExtraArgs) != before {
		t.Errorf("expected no change to args (already had --no-mmap), got %v", m.Backends[0].ExtraArgs)
	}
	count := 0
	for _, a := range m.Backends[0].ExtraArgs {
		if a == "--no-mmap" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly one --no-mmap, found %d", count)
	}
}

func TestApplyAutoNoMmapRespectsExplicitMmap(t *testing.T) {
	f, err := os.CreateTemp("", "fake-model-*.gguf")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.Write(make([]byte, 1024))
	f.Close()

	cfg := config.Defaults()
	cfg.Model.Path = f.Name()
	cfg.GPUs.Devices = []int{0, 1}
	cfg.GPUs.TensorSplit.Enabled = true
	cfg.Backend.ExtraArgs = []string{"--mmap"} // user explicitly opted in
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	m := NewManager(&cfg, nil, nil, nil, nil, nil)

	m.applyAutoNoMmap(1024)

	// Should NOT auto-inject --no-mmap when --mmap is explicit
	if hasNoMmapArg(m.Backends[0].ExtraArgs) {
		t.Errorf("should respect explicit --mmap, but --no-mmap was injected: %v", m.Backends[0].ExtraArgs)
	}
}

func TestReadTotalRAMBytes(t *testing.T) {
	// On Linux test hosts this should return a positive value; on others it returns 0.
	got := readTotalRAMBytes()
	if got < 0 {
		t.Errorf("readTotalRAMBytes returned negative: %d", got)
	}
	// Can't assert non-zero portably, but log it for visibility.
	t.Logf("MemTotal seen by tests: %.1f GiB", float64(got)/(1<<30))
}

func TestModelTotalSizeSingleFile(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/model.gguf"
	if err := os.WriteFile(p, make([]byte, 12345), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := modelTotalSize(p)
	if err != nil {
		t.Fatalf("modelTotalSize: %v", err)
	}
	if got != 12345 {
		t.Errorf("expected 12345, got %d", got)
	}
}

func TestModelTotalSizeMultiPart(t *testing.T) {
	dir := t.TempDir()
	// Three parts of different sizes — total 600.
	parts := []struct {
		name string
		size int
	}{
		{"my-model-Q3_K_M-00001-of-00003.gguf", 200},
		{"my-model-Q3_K_M-00002-of-00003.gguf", 200},
		{"my-model-Q3_K_M-00003-of-00003.gguf", 200},
	}
	for _, p := range parts {
		if err := os.WriteFile(dir+"/"+p.name, make([]byte, p.size), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Pass part 1 as the model path; helper should sum all 3.
	got, err := modelTotalSize(dir + "/" + parts[0].name)
	if err != nil {
		t.Fatalf("modelTotalSize: %v", err)
	}
	if got != 600 {
		t.Errorf("expected 600 (sum of all parts), got %d", got)
	}
}

func TestModelTotalSizeMultiPartWithMissingPart(t *testing.T) {
	// If one part is missing from the directory, the helper should still
	// return the sum of what's there (degrades gracefully). This protects
	// us from a partial download where one part hasn't landed yet.
	dir := t.TempDir()
	for _, p := range []struct {
		name string
		size int
	}{
		{"x-00001-of-00003.gguf", 100},
		{"x-00003-of-00003.gguf", 100},
	} {
		if err := os.WriteFile(dir+"/"+p.name, make([]byte, p.size), 0644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := modelTotalSize(dir + "/x-00001-of-00003.gguf")
	if err != nil {
		t.Fatalf("modelTotalSize: %v", err)
	}
	if got != 200 {
		t.Errorf("expected 200 (only 2 parts present), got %d", got)
	}
}

func TestApplyAutoNoMmapInjectsWhenMultiPartTotalTooBig(t *testing.T) {
	// Regression test for the off-by-300-MiB miss on Qwen 3 235B Q3_K_M:
	// individual parts are smaller than the threshold, but the total isn't.
	dir := t.TempDir()
	parts := []string{
		"qwen-Q3_K_M-00001-of-00003.gguf",
		"qwen-Q3_K_M-00002-of-00003.gguf",
		"qwen-Q3_K_M-00003-of-00003.gguf",
	}
	// Each part is 4 KiB. Total = 12 KiB.
	for _, name := range parts {
		if err := os.WriteFile(dir+"/"+name, make([]byte, 4096), 0644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := config.Defaults()
	cfg.Model.Path = dir + "/" + parts[0]
	cfg.GPUs.Devices = []int{0, 1}
	cfg.GPUs.TensorSplit.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	m := NewManager(&cfg, nil, nil, nil, nil, nil)

	// Fake host RAM = 10 KiB. Threshold = 8192 bytes.
	// Single part 4096 < 8192 (would NOT trigger before this fix)
	// Total       12288 > 8192 (DOES trigger now)
	m.applyAutoNoMmap(10240)

	if !hasNoMmapArg(m.Backends[0].ExtraArgs) {
		t.Errorf("expected --no-mmap auto-injected for multi-part model "+
			"(part1=4096, total=12288, threshold=8192), got args=%v",
			m.Backends[0].ExtraArgs)
	}
}
