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
		ContextSize: 8192, NGPULayers: -1, Parallel: 1, Binary: "llama-server",
		ExtraArgs: []string{"--threads", "4"},
	}
	args := b.buildArgs()
	expected := []string{"--model", "/models/test.gguf", "--port", "9003", "--ctx-size", "8192", "--n-gpu-layers", "-1", "--parallel", "1", "--slots", "--log-disable", "--threads", "4"}
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
