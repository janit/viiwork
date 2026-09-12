package gpu

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

const (
	rocmPIDGPUsCmd     = "rocm-smi --showpidgpus"
	nvidiaQueryGPUCmd  = "nvidia-smi --query-gpu=index,uuid --format=csv,noheader"
	nvidiaComputeAppsC = "nvidia-smi --query-compute-apps=pid,gpu_uuid --format=csv,noheader"
)

func TestProcessGPUsROCm(t *testing.T) {
	ctx := context.Background()
	fixture := mustFixture(t, "rocm-showpidgpus.txt")

	got, err := ProcessGPUs(ctx, VendorAMD, fakeRunner(map[string]string{rocmPIDGPUsCmd: fixture}))
	if err != nil {
		t.Fatal(err)
	}
	want := map[int][]int{2903704: {3, 4}, 2904488: {7, 8}, 2903370: {2}, 2898270: {0, 1}, 2904155: {5, 6}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("gb1 fixture = %v, want %v", got, want)
	}

	// A PID line followed straight by the footer holds no indices.
	footer := "=========================================================================================="
	truncated := strings.Replace(fixture, footer, "PID 42 is using 1 DRM device(s):\n"+footer, 1)
	got, err = ProcessGPUs(ctx, VendorAMD, fakeRunner(map[string]string{rocmPIDGPUsCmd: truncated}))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[42]; ok {
		t.Errorf("PID 42 with no index line must be absent, got %v", got[42])
	}
	if !reflect.DeepEqual(got[2904155], []int{5, 6}) || len(got) != 5 {
		t.Errorf("the other blocks must still parse, got %v", got)
	}

	if _, err := ProcessGPUs(ctx, VendorAMD, fakeRunner(nil)); err == nil || !strings.Contains(err.Error(), rocmPIDGPUsCmd) {
		t.Errorf("failing rocm-smi: err = %v, want it to name the command", err)
	}
}

func TestProcessGPUsNVIDIA(t *testing.T) {
	ctx := context.Background()
	outputs := map[string]string{
		nvidiaQueryGPUCmd:  mustFixture(t, "nvidia-query-gpu.csv"),
		nvidiaComputeAppsC: mustFixture(t, "nvidia-compute-apps.csv"),
	}
	got, err := ProcessGPUs(ctx, VendorNVIDIA, fakeRunner(outputs))
	if err != nil {
		t.Fatal(err)
	}
	want := map[int][]int{2968184: {0, 1}, 2968078: {2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("nvidia fixtures = %v, want %v", got, want)
	}

	outputs[nvidiaComputeAppsC] += "77, GPU-deadbeef\n"
	got, err = ProcessGPUs(ctx, VendorNVIDIA, fakeRunner(outputs))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[77]; ok || !reflect.DeepEqual(got, want) {
		t.Errorf("a row with an unknown UUID must add nothing, got %v", got)
	}
}

func TestProcessGPUsNone(t *testing.T) {
	got, err := ProcessGPUs(context.Background(), VendorNone, failingRunner(t))
	if got != nil || err != nil {
		t.Errorf("VendorNone = %v, %v; want nil, nil", got, err)
	}
}

func TestCheckAssignment(t *testing.T) {
	cases := []struct {
		name        string
		pidGPUs     map[int][]int
		nodePIDs    []int
		backendPIDs []int
		want        []int
		verdict     Verdict
	}{
		{"on its cards, any order", map[int][]int{10: {0, 1}}, []int{10}, []int{10}, []int{1, 0}, VerdictOK},
		{"child process holds the card", map[int][]int{11: {0}}, []int{10, 11}, []int{10, 11}, []int{0}, VerdictOK},
		{"wrong card", map[int][]int{10: {2}}, []int{10}, []int{10}, []int{0}, VerdictWrong},
		{"extra card", map[int][]int{10: {0, 1}}, []int{10}, []int{10}, []int{0}, VerdictWrong},
		{"no card while namespaces match", map[int][]int{10: {0}}, []int{10, 12}, []int{12}, []int{1}, VerdictWrong},
		{"private PID namespace (gb1 container)", map[int][]int{2903370: {2}}, []int{25}, []int{25}, []int{2}, VerdictUnknown},
		{"tool reported nothing", nil, []int{10}, []int{10}, []int{0}, VerdictUnknown},
	}
	for _, tc := range cases {
		if got := CheckAssignment(tc.pidGPUs, tc.nodePIDs, tc.backendPIDs, tc.want); got != tc.verdict {
			t.Errorf("%s: CheckAssignment = %v, want %v", tc.name, got, tc.verdict)
		}
	}
}

func TestVerdictString(t *testing.T) {
	for v, want := range map[Verdict]string{VerdictUnknown: "unknown", VerdictOK: "ok", VerdictWrong: "wrong"} {
		if got := v.String(); got != want {
			t.Errorf("Verdict(%d).String() = %q, want %q", int(v), got, want)
		}
	}
}
