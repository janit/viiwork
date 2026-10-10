package node

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/mesh/meshtest"
	"github.com/janit/viiwork/v2/meshapi"
)

func TestEngineVersionsKnownNeverWaits(t *testing.T) {
	release := make(chan struct{})
	var reads atomic.Int32
	ev := &engineVersions{read: func() map[string]string {
		reads.Add(1)
		<-release
		return map[string]string{"llamacpp": "b6123"}
	}}
	if got := ev.Known(); got != nil {
		t.Fatalf("Known before any read = %v, want nil", got)
	}
	done := make(chan map[string]string, 2)
	go func() { done <- ev.Wait() }()
	go func() { done <- ev.Wait() }()
	// The read is running and blocked: Known still answers, with nothing.
	deadline := time.Now().Add(5 * time.Second)
	for reads.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the read never started")
		}
		time.Sleep(time.Millisecond)
	}
	if got := ev.Known(); got != nil {
		t.Fatalf("Known during the read = %v, want nil", got)
	}
	close(release)
	for range 2 {
		if got := <-done; got["llamacpp"] != "b6123" {
			t.Errorf("Wait = %v", got)
		}
	}
	if got := ev.Known(); got["llamacpp"] != "b6123" {
		t.Errorf("Known after the read = %v", got)
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("the engines were read %d times, want once", n)
	}
}

// An engine whose version cannot be read leaves an empty map, which is
// "read, and nothing to say", and is not read again.
func TestEngineVersionsEmptyReadIsFinal(t *testing.T) {
	var reads atomic.Int32
	ev := &engineVersions{read: func() map[string]string { reads.Add(1); return nil }}
	if got := ev.Wait(); len(got) != 0 {
		t.Errorf("Wait = %v", got)
	}
	ev.Wait()
	if got := ev.Known(); got == nil || len(got) != 0 {
		t.Errorf("Known = %v, want an empty map", got)
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("read %d times, want once", n)
	}
}

// A running node reads its engine's version by itself, and the cluster view
// carries it beside the engine's name with nobody asking /v1/update first.
func TestClusterCarriesTheEngineVersion(t *testing.T) {
	// llama-server as the node runs it, except that --version answers like a
	// real build instead of starting the fake.
	wrapper := filepath.Join(t.TempDir(), "llama-server")
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'version: 6123 (abc1234)'; exit 0; fi\nexec %q \"$@\"\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	model := strings.Replace(fakeModel{name: "lc"}.yaml(), fmt.Sprintf("binary: %q", os.Args[0]), fmt.Sprintf("binary: %q", wrapper), 1)
	if !strings.Contains(model, wrapper) {
		t.Fatal("the model does not run the wrapper")
	}
	tn := startNodeYAML(t, meshtest.NewNetwork(), "n1", []string{model}, "", nil)
	waitForModels(t, tn, "lc")

	var last meshapi.ClusterResponse
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var c meshapi.ClusterResponse
		if getJSON(t, tn.url("/v1/cluster"), &c) == 200 && len(c.Members) == 1 && c.Members[0].Status != nil && len(c.Members[0].Status.Models) == 1 {
			last = c
			m := c.Members[0].Status.Models[0]
			if m.Engine != "llamacpp" || m.EngineName != "llama.cpp" {
				t.Fatalf("engine %q, engine_name %q; want llamacpp, llama.cpp", m.Engine, m.EngineName)
			}
			if m.EngineVersion == "b6123" {
				return
			}
			if m.EngineVersion != "" {
				t.Fatalf("engine_version = %q, want b6123", m.EngineVersion)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("/v1/cluster never carried engine_version b6123: %+v", last)
}
