package process

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/balancer"
)

func TestBackendBuildArgs(t *testing.T) {
	b := &Backend{
		GPUID: 2, ModelPath: "/models/test.gguf", Port: 9003,
		ContextSize: 13337, NGPULayers: -1, Parallel: 1, Binary: "llama-server",
		ExtraArgs: []string{"--threads", "4"},
	}
	args := b.buildArgs()
	expected := []string{"--model", "/models/test.gguf", "--port", "9003", "--ctx-size", "13337", "--n-gpu-layers", "-1", "--parallel", "1", "--slots", "--log-disable", "--threads", "4"}
	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, arg := range args {
		if arg != expected[i] { t.Errorf("arg[%d]: expected %q, got %q", i, expected[i], arg) }
	}
}

func TestBackendEnv(t *testing.T) {
	b := &Backend{GPUID: 3}
	env := b.buildEnv()
	found := false
	for _, e := range env {
		if e == "ROCR_VISIBLE_DEVICES=3" { found = true }
	}
	if !found { t.Error("expected ROCR_VISIBLE_DEVICES=3 in env") }
}

func TestHealthCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" { w.WriteHeader(200); return }
		w.WriteHeader(404)
	}))
	defer srv.Close()
	b := &Backend{State: balancer.NewBackendState(0, srv.Listener.Addr().String()), HealthTimeout: 3 * time.Second}
	if !b.CheckHealth(context.Background()) { t.Error("expected healthy") }
}

func TestHealthCheckFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	b := &Backend{State: balancer.NewBackendState(0, srv.Listener.Addr().String()), HealthTimeout: 3 * time.Second}
	if b.CheckHealth(context.Background()) { t.Error("expected unhealthy") }
}

func TestBackendPowerLimitField(t *testing.T) {
	b := &Backend{GPUID: 3, PowerLimitWatts: 180}
	if b.PowerLimitWatts != 180 { t.Errorf("expected 180, got %d", b.PowerLimitWatts) }
}

func TestBackendBuildArgsTensorSplitDefault(t *testing.T) {
	b := &Backend{
		GPUID: -1, GPUIDs: []int{4, 5, 6, 7}, TensorSplit: true,
		ModelPath: "/models/big.gguf", Port: 9001,
		ContextSize: 4096, NGPULayers: -1, Parallel: 1, Binary: "llama-server",
	}
	args := b.buildArgs()
	// Must contain --split-mode layer (default) and --tensor-split with even
	// weights "1,1,1,1". --main-gpu must NOT be present for layer mode.
	joined := joinArgs(args)
	if !contains(args, "--split-mode") || !contains(args, "layer") {
		t.Errorf("expected --split-mode layer in args: %v", args)
	}
	if !contains(args, "--tensor-split") {
		t.Errorf("expected --tensor-split in args: %v", args)
	}
	if !contains(args, "1,1,1,1") {
		t.Errorf("expected even weights '1,1,1,1' in args: %v", args)
	}
	if contains(args, "--main-gpu") {
		t.Errorf("did not expect --main-gpu for split-mode=layer: %v", args)
	}
	if joined == "" {
		t.Error("empty arg list")
	}
}

func TestBackendBuildArgsTensorSplitRowMain(t *testing.T) {
	b := &Backend{
		GPUID: -1, GPUIDs: []int{4, 5, 6}, TensorSplit: true,
		SplitMode: "row", MainGPU: 1,
		SplitWeights: []float64{0.5, 1.0, 1.5},
		ModelPath: "/models/big.gguf", Port: 9001,
		ContextSize: 4096, NGPULayers: -1, Parallel: 1, Binary: "llama-server",
	}
	args := b.buildArgs()
	if !contains(args, "row") {
		t.Errorf("expected --split-mode row: %v", args)
	}
	if !contains(args, "--main-gpu") || !contains(args, "1") {
		t.Errorf("expected --main-gpu 1 for split-mode=row: %v", args)
	}
	if !contains(args, "0.5,1,1.5") {
		t.Errorf("expected explicit weights '0.5,1,1.5': %v", args)
	}
}

func TestBackendEnvTensorSplit(t *testing.T) {
	b := &Backend{GPUID: -1, GPUIDs: []int{4, 5, 8, 9}, TensorSplit: true}
	env := b.buildEnv()
	found := false
	for _, e := range env {
		if e == "ROCR_VISIBLE_DEVICES=4,5,8,9" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected ROCR_VISIBLE_DEVICES=4,5,8,9 in env, got %v", env)
	}
}

func TestBackendLabel(t *testing.T) {
	r := &Backend{GPUID: 4}
	if r.label() != "gpu-4" {
		t.Errorf("expected gpu-4, got %s", r.label())
	}
	ts := &Backend{GPUID: -1, GPUIDs: []int{4, 5, 6, 7}, TensorSplit: true}
	if ts.label() != "ts-4,5,6,7" {
		t.Errorf("expected ts-4,5,6,7, got %s", ts.label())
	}
}

// helpers
func contains(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

func joinArgs(args []string) string {
	out := ""
	for _, a := range args {
		out += a + " "
	}
	return out
}
