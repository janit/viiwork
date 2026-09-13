package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/internal/api"
	"github.com/janit/viiwork/v2/meshapi"
)

// testDialect is a deliberately foreign request shape, registered on a path of
// its own. It exists to prove the seam carries a non-native dialect end to end
// — a request in another JSON form reaches the engine as OpenAI — and it is
// NOT shipped: nothing outside this test file registers it.
type testDialect struct{}

func (testDialect) Name() string    { return "test-foreign" }
func (testDialect) Paths() []string { return []string{"/test/v1/messages"} }

func (testDialect) Decode(r *http.Request, body []byte) (api.Request, bool, error) {
	var in struct {
		ModelID string `json:"model_id"`
		Prompt  string `json:"prompt"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return api.Request{}, false, err
	}
	out, err := json.Marshal(map[string]any{
		"model":    in.ModelID,
		"messages": []map[string]string{{"role": "user", "content": in.Prompt}},
	})
	if err != nil {
		return api.Request{}, false, err
	}
	return api.Request{Model: in.ModelID, Body: out}, false, nil
}

func init() { api.Register(testDialect{}) }

// The seam is real: a foreign shape on a foreign path is routed like any other
// request, and what the engine receives is OpenAI. Nothing past decode knows a
// dialect was involved.
func TestForeignDialectReachesTheEngineAsOpenAI(t *testing.T) {
	f := newHandlerFx(t)
	eng := newRecEngine(t, nil)
	f.local.add("m", eng.addr())
	f.build()

	rec := f.do(http.MethodPost, "/test/v1/messages", `{"model_id":"m","prompt":"hello"}`)
	if rec.Code != 200 || rec.Header().Get(meshapi.HeaderModel) != "m" {
		t.Fatalf("code=%d headers=%v body=%q", rec.Code, rec.Header(), rec.Body.String())
	}
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if len(eng.bodies) != 1 {
		t.Fatalf("engine saw %d requests, want 1", len(eng.bodies))
	}
	got := eng.bodies[0]
	if !strings.Contains(got, `"messages"`) || !strings.Contains(got, `"hello"`) {
		t.Errorf("the engine must receive an OpenAI body, got %q", got)
	}
	if strings.Contains(got, "model_id") {
		t.Errorf("the engine must never see the dialect's own shape, got %q", got)
	}
}

// The native dialect is pass-through: the bytes the client sent are the bytes
// the engine gets. This is the property the whole seam is built around, and it
// is what the allocation benchmarks guard.
func TestNativeDialectIsPassThrough(t *testing.T) {
	f := newHandlerFx(t)
	eng := newRecEngine(t, nil)
	f.local.add("m", eng.addr())
	f.build()

	if rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq); rec.Code != 200 {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if eng.bodies[0] != chatReq {
		t.Errorf("the native path rewrote the body:\n sent: %s\n got:  %s", chatReq, eng.bodies[0])
	}
}

func TestNativeDialectReportsItself(t *testing.T) {
	d, ok := api.Lookup(meshapi.PathChatCompletions)
	if !ok {
		t.Fatal("no dialect owns /v1/chat/completions")
	}
	if d.Name() != "openai" {
		t.Errorf("dialect = %q, want the native one", d.Name())
	}
	req, native, err := d.Decode(httpRequest(t), []byte(chatReq))
	if err != nil || !native {
		t.Fatalf("Decode: native=%v err=%v", native, err)
	}
	if req.Model != "m" {
		t.Errorf("Model = %q", req.Model)
	}
	if string(req.Body) != chatReq {
		t.Errorf("a native decode must not rewrite the body")
	}
}

// The task tag is stripped from the body and carried as a header, so no engine
// in the fleet ever sees the field.
func TestNativeDialectStripsTheTaskField(t *testing.T) {
	d, _ := api.Lookup(meshapi.PathChatCompletions)
	req, _, err := d.Decode(httpRequest(t), []byte(`{"model":"m","task":"nightly","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Task != "nightly" {
		t.Errorf("Task = %q", req.Task)
	}
	if strings.Contains(string(req.Body), "task") {
		t.Errorf("the task field must be stripped from the body, got %s", req.Body)
	}
}

func httpRequest(t *testing.T) *http.Request {
	t.Helper()
	return &http.Request{Header: http.Header{}}
}
