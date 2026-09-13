package node

// v2.2 Task 4: both new engines end to end through a real node, with the real
// llamacpp engine alongside them. The engines build the command lines, the
// fakes in fakenewengines_test.go assert those command lines and answer the
// engines' own endpoints, and the node does the rest unchanged.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/mesh/meshtest"
	"github.com/janit/viiwork/v2/meshapi"
)

// threeEngineModels is one model per engine: llamacpp on CPU, vLLM across two
// cards, FreeToken on one.
func threeEngineModels() []string {
	return []string{
		fakeModel{name: "lc", parallel: 2}.yaml(),
		gpuModel{name: "vl", engine: "vllm", helper: "vllm",
			gpus: []int{0, 1}, ctx: 16384, para: 8}.yaml(),
		gpuModel{name: "ft", engine: "freetoken", helper: "ft",
			gpus: []int{2}, ctx: 32768, para: 4}.yaml(),
	}
}

// capacityOf fetches /v1/capacity and indexes it by model name.
func capacityOf(t *testing.T, tn *testNode) map[string]meshapi.ModelCapacity {
	t.Helper()
	resp, err := http.Get(tn.url(meshapi.PathCapacity))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var cap meshapi.CapacityResponse
	if err := json.NewDecoder(resp.Body).Decode(&cap); err != nil {
		t.Fatal(err)
	}
	out := map[string]meshapi.ModelCapacity{}
	for _, m := range cap.Models {
		out[m.Name] = m
	}
	return out
}

// waitForModels waits until /v1/capacity reports every name with slots > 0,
// which is the node's own statement that the backend reached healthy.
func waitForModels(t *testing.T, tn *testNode, names ...string) map[string]meshapi.ModelCapacity {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var got map[string]meshapi.ModelCapacity
	for time.Now().Before(deadline) {
		got = capacityOf(t, tn)
		ready := 0
		for _, n := range names {
			if m, ok := got[n]; ok && m.Slots > 0 {
				ready++
			}
		}
		if ready == len(names) {
			return got
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("models %v not all healthy within 30s; capacity: %+v\nlog:\n%s", names, got, tn.log.String())
	return nil
}

// One node, three engines, all healthy, and the numbers the node publishes are
// the ones the operator configured.
func TestThreeEnginesOnOneNode(t *testing.T) {
	tn := startNodeYAML(t, meshtest.NewNetwork(), "n1", threeEngineModels(), "", nil)

	cap := waitForModels(t, tn, "lc", "vl", "ft")

	for _, want := range []struct {
		name       string
		slots      int
		ctxPerSlot int64
	}{
		// llamacpp reports its own /slots, which the fake serves at n_ctx 512.
		{name: "lc", slots: 2, ctxPerSlot: 512},
		// vLLM reports neither, so both come from the Spec: parallel 8 and
		// context 16384, per backend. Two cards, gpus_per_backend 2, so one
		// backend.
		{name: "vl", slots: 8, ctxPerSlot: 16384},
		{name: "ft", slots: 4, ctxPerSlot: 32768},
	} {
		got, ok := cap[want.name]
		if !ok {
			t.Errorf("%s missing from /v1/capacity", want.name)
			continue
		}
		if got.Slots != want.slots {
			t.Errorf("%s slots = %d, want %d", want.name, got.Slots, want.slots)
		}
		if got.Ctx != want.ctxPerSlot {
			t.Errorf("%s ctx = %d, want %d", want.name, got.Ctx, want.ctxPerSlot)
		}
	}

	// /v1/models lists all three, whatever engine serves them.
	resp, err := http.Get(tn.url("/v1/models"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range models.Data {
		seen[m.ID] = true
	}
	for _, n := range []string{"lc", "vl", "ft"} {
		if !seen[n] {
			t.Errorf("/v1/models does not list %q; got %+v", n, models.Data)
		}
	}
}

// A request reaches the engine that serves it and comes back as a completion.
func TestEachEngineServesARequest(t *testing.T) {
	tn := startNodeYAML(t, meshtest.NewNetwork(), "n1", threeEngineModels(), "", nil)
	waitForModels(t, tn, "lc", "vl", "ft")

	for _, name := range []string{"lc", "vl", "ft"} {
		t.Run(name, func(t *testing.T) {
			body := `{"model":"` + name + `","messages":[{"role":"user","content":"hi"}]}`
			resp, err := http.Post(tn.url("/v1/chat/completions"), "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %s", resp.Status)
			}
			var out struct {
				Choices []struct {
					Message struct {
						Content string `json:"content"`
					} `json:"message"`
				} `json:"choices"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
			if len(out.Choices) == 0 || out.Choices[0].Message.Content == "" {
				t.Errorf("no completion content: %+v", out)
			}
		})
	}
}

// A FreeToken backend that stays "loading" is not routable, and the phase the
// engine reported is what /v1/status shows — the distinction the engine's
// Probe exists to make, since FreeToken answers 200 while loading.
func TestFreeTokenLoadingIsNotRoutableAndShowsItsPhase(t *testing.T) {
	models := []string{
		gpuModel{name: "ft", engine: "freetoken", helper: "ft", gpus: []int{0},
			env: map[string]string{"FAKE_STATUS": "loading", "FAKE_PHASE": "weights"}}.yaml(),
	}
	tn := startNodeYAML(t, meshtest.NewNetwork(), "n1", models, "", nil)

	// Wait for the ENGINE's phase. The supervisor reports its own phases on the
	// same field — a backend waiting on the node-wide load gate is "queued" —
	// so this waits for the one the engine's /health supplied rather than for
	// the first non-empty value.
	deadline := time.Now().Add(15 * time.Second)
	var phase string
	for time.Now().Before(deadline) {
		resp, err := http.Get(tn.url(meshapi.PathStatus))
		if err != nil {
			t.Fatal(err)
		}
		var st meshapi.NodeStatus
		err = json.NewDecoder(resp.Body).Decode(&st)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range st.Models {
			for _, b := range m.Backends {
				if b.Phase != "" {
					phase = b.Phase
				}
			}
		}
		if phase == "weights" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if phase != "weights" {
		t.Errorf("phase = %q, want %q: a loading FreeToken backend must show what it is doing\nlog:\n%s", phase, "weights", tn.log.String())
	}

	// Not routable: the request queues and times out rather than being sent to
	// a backend that would 503 it.
	if cap := capacityOf(t, tn); cap["ft"].Slots != 0 {
		t.Errorf("a loading backend must publish no slots, got %d", cap["ft"].Slots)
	}
	body := `{"model":"ft","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(tn.url("/v1/chat/completions"), "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("a loading backend must not serve a request, got 200")
	}
}

// A BoundGPU mismatch is REPORTED and changes nothing: the backend is serving
// correctly, and taking it out of the mesh would trade a wrong label for a
// lost GPU.
func TestFreeTokenGPUMismatchLogsAndChangesNothing(t *testing.T) {
	models := []string{
		gpuModel{name: "ft", engine: "freetoken", helper: "ft", gpus: []int{0}, para: 4,
			env: map[string]string{"FAKE_GPU_UUID": "GPU-not-the-one-the-node-thinks"}}.yaml(),
	}
	tn := startNodeYAML(t, meshtest.NewNetwork(), "n1", models, "", nil)

	cap := waitForModels(t, tn, "ft")
	if cap["ft"].Slots != 4 {
		t.Errorf("slots = %d, want 4: a GPU-label mismatch must not change capacity", cap["ft"].Slots)
	}

	// And it still serves.
	body := `{"model":"ft","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(tn.url("/v1/chat/completions"), "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %s: a GPU-label mismatch must not stop the backend serving", resp.Status)
	}
}

// vLLM's Load returns an error rather than a zero when the build publishes no
// running-sequence gauge (Decision 3). The node keeps serving on its own
// in-flight count; what it must not do is freeze "idle" into the mesh.
func TestVLLMWithoutRunningGaugeStillServes(t *testing.T) {
	models := []string{
		gpuModel{name: "vl", engine: "vllm", helper: "vllm", gpus: []int{0}, para: 3,
			env: map[string]string{"FAKE_NO_RUNNING_GAUGE": "1"}}.yaml(),
	}
	tn := startNodeYAML(t, meshtest.NewNetwork(), "n1", models, "", nil)
	waitForModels(t, tn, "vl")

	body := `{"model":"vl","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(tn.url("/v1/chat/completions"), "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %s", resp.Status)
	}
}
