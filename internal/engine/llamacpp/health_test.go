package llamacpp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/engine"
)

// serve starts a fake llama-server and returns its host:port.
func serve(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestProbe(t *testing.T) {
	e := New()
	ctx := context.Background()

	p, err := e.Probe(ctx, serve(t, answer(200, `{"status":"ok"}`)))
	if err != nil || !p.Ready || p.Phase != "" || p.Progress != -1 || p.Reason != "" {
		t.Errorf("200: %+v, %v", p, err)
	}

	p, err = e.Probe(ctx, serve(t, answer(503, `{"error":{"code":503,"message":"Loading model","type":"unavailable_error"}}`)))
	if err != nil || p.Ready || p.Phase != "loading" || p.Progress != -1 {
		t.Errorf("503: %+v, %v", p, err)
	}

	p, err = e.Probe(ctx, serve(t, answer(500, "")))
	if err != nil || p.Ready || p.Reason != "health returned HTTP 500" {
		t.Errorf("500: %+v, %v", p, err)
	}

	closed := httptest.NewServer(answer(200, "ok"))
	addr := closed.Listener.Addr().String()
	closed.Close()
	if _, err := e.Probe(ctx, addr); err == nil {
		t.Error("a closed server must be a transport error")
	}

	slow := serve(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(time.Second):
		case <-r.Context().Done():
		}
	})
	deadline, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := e.Probe(deadline, slow); err == nil {
		t.Error("a probe past its deadline must be an error")
	}
}

func TestLoadIdle(t *testing.T) {
	l, err := New().Load(context.Background(), serve(t, answer(200, fixture(t, "slots-idle.json"))))
	if err != nil {
		t.Fatal(err)
	}
	if l != (engine.Load{Slots: 2, Busy: 0, Waiting: 0, CtxPerSlot: 49152}) {
		t.Errorf("Load = %+v", l)
	}
}

func TestLoadProgressBusy(t *testing.T) {
	l, decoded, remain, err := New().LoadProgress(context.Background(), serve(t, answer(200, fixture(t, "slots-busy.json"))))
	if err != nil {
		t.Fatal(err)
	}
	if l.Slots != 2 || l.Busy != 1 || l.CtxPerSlot != 49152 || decoded != 120 || remain != 380 {
		t.Errorf("LoadProgress = %+v decoded=%d remain=%d", l, decoded, remain)
	}
}

func TestLoadProgressIgnoresStaleTokenOnIdleSlot(t *testing.T) {
	body := `[{"id":0,"n_ctx":4096,"is_processing":false,"next_token":[{"n_decoded":999,"n_remain":5}]},{"id":1,"n_ctx":4096,"is_processing":false}]`
	_, decoded, remain, err := New().LoadProgress(context.Background(), serve(t, answer(200, body)))
	if err != nil || decoded != 0 || remain != 0 {
		t.Errorf("decoded=%d remain=%d err=%v, want 0, 0, nil", decoded, remain, err)
	}
}

func TestLoadErrors(t *testing.T) {
	e := New()
	ctx := context.Background()
	if _, err := e.Load(ctx, serve(t, answer(501, `{"error":"slots endpoint disabled"}`))); err == nil || !strings.Contains(err.Error(), "501") {
		t.Errorf("501: err = %v, want it to name the status", err)
	}
	if _, err := e.Load(ctx, serve(t, answer(200, "not json"))); err == nil {
		t.Error("an undecodable body must be an error")
	}
	if l, err := e.Load(ctx, serve(t, answer(200, "[]"))); err != nil || l.Slots != 0 {
		t.Errorf("empty array: %+v, %v", l, err)
	}
}

func TestRegistered(t *testing.T) {
	e, ok := engine.Lookup(config.EngineLlamaCpp)
	if !ok || e.Name() != config.EngineLlamaCpp {
		t.Fatalf("engine.Lookup(llamacpp) = %v, %v", e, ok)
	}
}
