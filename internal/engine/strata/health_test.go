package strata

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
)

// backend serves the given bodies by path; a path with no entry is a 404.
// A body of "HANG" never answers, to stand for the stalls seen on gb3.
func backend(t *testing.T, bodies map[string]string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if body == "HANG" {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func file(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	return c
}

func TestProbe(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		ready      bool
		reason     string
	}{
		{"gb3 ready", file(t, "health-ready.json"), true, ""},
		// Hand-written from the server's source: --lazy and idle unload.
		{"not loaded", file(t, "health-unloaded.json"), false, "not loaded"},
		{"loaded absent", `{"status": "ok", "service": "strata"}`, false, "not loaded"},
		{"another service", `{"status": "ok", "loaded": true, "service": "llama"}`, false, "not a Strata"},
		{"llama.cpp's health", `{"status": "ok"}`, false, "not a Strata"},
		{"status not ok", `{"status": "error", "loaded": true, "service": "strata"}`, false, "status"},
		{"not json", "<html>", false, "not a Strata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := New().Probe(ctx(t), backend(t, map[string]string{"/health": tc.body}))
			if err != nil {
				t.Fatalf("an answer is never a transport error: %v", err)
			}
			if p.Ready != tc.ready {
				t.Errorf("Ready = %v, want %v", p.Ready, tc.ready)
			}
			if !strings.Contains(p.Reason, tc.reason) {
				t.Errorf("Reason = %q, want it to contain %q", p.Reason, tc.reason)
			}
			if p.Progress != -1 || p.Phase != "" {
				t.Errorf("Strata publishes no phase or progress; got %q, %v", p.Phase, p.Progress)
			}
		})
	}
}

func TestProbeNon200IsNotReady(t *testing.T) {
	p, err := New().Probe(ctx(t), backend(t, map[string]string{}))
	if err != nil || p.Ready {
		t.Fatalf("a 404 must be Ready false with no error; got %+v, %v", p, err)
	}
}

func TestLoad(t *testing.T) {
	one := func(busy int) engine.Load {
		return engine.Load{Slots: 1, Busy: busy, Waiting: 0, CtxPerSlot: 131072}
	}
	for _, tc := range []struct {
		name            string
		slots, status   string
		want            engine.Load
		decoded, remain int64
	}{
		// /slots from gb3 (HIP), /status from teddy (CUDA), both v0.1.39.
		// An idle /status still carries the LAST request's counts
		// (generated 294 of 300): they are not work in flight.
		{"idle", file(t, "slots-idle.json"), file(t, "status-idle.json"), one(0), 0, 0},
		{"generating", file(t, "slots-busy.json"), file(t, "status-generating.json"), one(1), 125, 175},
		// Hand-written from here on.
		{"a queue", file(t, "slots-busy.json"), `{"busy": true, "queued": 2, "generated": null, "max_tokens": null}`,
			engine.Load{Slots: 1, Busy: 1, Waiting: 2, CtxPerSlot: 131072}, 0, 0},
		{"numbers written as floats", file(t, "slots-busy.json"), `{"busy": true, "queued": 1.0, "generated": 10.0, "max_tokens": 50.0}`,
			engine.Load{Slots: 1, Busy: 1, Waiting: 1, CtxPerSlot: 131072}, 10, 40},
		{"status without the fields", file(t, "slots-busy.json"), `{"phase": "answering"}`, one(1), 0, 0},
		{"status not json", file(t, "slots-busy.json"), `oops`, one(1), 0, 0},
		// /metrics did this on gb3 during a request; /status is the lighter
		// endpoint, and a stall there must cost the tick no more.
		{"status hangs", file(t, "slots-busy.json"), "HANG", one(1), 0, 0},
		// v0.1.41 with "parallel": 2, hand-written from the server's source:
		// one entry per batch slot. A request alone runs outside the slots,
		// so both read idle while /status says busy; the node's own count of
		// requests in flight covers that one (supervisor: max of the two).
		{"two slots idle", twoSlots(false, false), file(t, "status-idle.json"),
			engine.Load{Slots: 2, CtxPerSlot: 262144}, 0, 0},
		{"two slots, one busy", twoSlots(true, false), `{"busy": true, "queued": 0, "generated": 10, "max_tokens": 50}`,
			engine.Load{Slots: 2, Busy: 1, CtxPerSlot: 262144}, 10, 40},
		{"two slots, a request alone", twoSlots(false, false), `{"busy": true, "queued": 0, "generated": 10, "max_tokens": 50}`,
			engine.Load{Slots: 2, CtxPerSlot: 262144}, 10, 40},
		// /status counts one request; with two running it describes neither.
		{"two slots, both busy", twoSlots(true, true), `{"busy": true, "queued": 1, "generated": 10, "max_tokens": 50}`,
			engine.Load{Slots: 2, Busy: 2, Waiting: 1, CtxPerSlot: 262144}, 0, 0},
		{"generated above max_tokens", file(t, "slots-busy.json"), `{"busy": true, "queued": 0, "generated": 300, "max_tokens": 200}`, one(1), 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := backend(t, map[string]string{"/slots": tc.slots, "/status": tc.status})
			start := time.Now()
			got, decoded, remain, err := New().LoadProgress(ctx(t), engine.Spec{}, addr)
			if err != nil {
				t.Fatalf("LoadProgress: %v", err)
			}
			// The load tick is 1 s. A /status that never answers must give
			// up well inside it, not ride the caller's deadline.
			if d := time.Since(start); d > 700*time.Millisecond {
				t.Errorf("LoadProgress took %v: a stalled /status must not use up the load tick", d)
			}
			if got != tc.want || decoded != tc.decoded || remain != tc.remain {
				t.Errorf("got %+v decoded %d remain %d\nwant %+v decoded %d remain %d",
					got, decoded, remain, tc.want, tc.decoded, tc.remain)
			}
		})
	}
}

func twoSlots(first, second bool) string {
	return fmt.Sprintf(`[{"id": 0, "n_ctx": 262144, "is_processing": %t, "n_prompt_tokens": 0},
		{"id": 1, "n_ctx": 262144, "is_processing": %t, "n_prompt_tokens": 120}]`, first, second)
}

func TestLoadIsLoadProgressWithoutTheProgress(t *testing.T) {
	addr := backend(t, map[string]string{"/slots": file(t, "slots-busy.json"), "/status": file(t, "status-generating.json")})
	want := engine.Load{Slots: 1, Busy: 1, CtxPerSlot: 131072}
	if l, err := New().Load(ctx(t), engine.Spec{}, addr); err != nil || l != want {
		t.Errorf("Load = %+v, %v; want %+v", l, err, want)
	}
}

// Absent is not zero: none of these may come back as a Load.
func TestLoadErrors(t *testing.T) {
	for name, slots := range map[string]string{
		"empty list":     `[]`,
		"not a list":     `{}`,
		"not json":       `oops`,
		"n_ctx missing":  `[{"id": 0, "is_processing": false}]`,
		"n_ctx negative": `[{"id": 0, "n_ctx": -1, "is_processing": false}]`,
		// A missing flag is not "idle".
		"is_processing missing": `[{"id": 0, "n_ctx": 4096}]`,
		"is_processing null":    `[{"id": 0, "n_ctx": 4096, "is_processing": null}]`,
	} {
		t.Run(name, func(t *testing.T) {
			addr := backend(t, map[string]string{"/slots": slots, "/status": file(t, "status-idle.json")})
			if l, err := New().Load(ctx(t), engine.Spec{Parallel: 1, Context: 4096}, addr); err == nil {
				t.Errorf("Load = %+v with no error", l)
			}
		})
	}
	t.Run("slots 404", func(t *testing.T) {
		addr := backend(t, map[string]string{"/status": file(t, "status-idle.json")})
		if _, err := New().Load(ctx(t), engine.Spec{}, addr); err == nil {
			t.Error("a /slots that is not a 200 must be an error")
		}
	})
}
