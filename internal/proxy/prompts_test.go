package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/activity"
	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/peer"
)

// findRequestRID pulls the RequestID off the first "request" event in the
// log — tests can't predict the value since activity.NewRequestID() is a
// process-wide counter shared across the whole test binary.
func findRequestRID(t *testing.T, log *activity.Log) int64 {
	t.Helper()
	for _, ev := range log.Recent() {
		if ev.Type == "request" && ev.RequestID != 0 {
			return ev.RequestID
		}
	}
	t.Fatal("no request event found")
	return 0
}

func TestPromptStoredAndFetchableLocally(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	}))
	defer backend.Close()

	// Prompt capture sits on the mesh routing path (handleProxy's rid mint
	// point) — a standalone NewHandler with no registry returns early via
	// handleLocalProxy and never reaches it, so this needs NewMeshHandler
	// even though the request never actually leaves this node.
	state := balancer.NewBackendState(0, backend.Listener.Addr().String())
	state.SetStatus(balancer.StatusHealthy)
	backends := []*balancer.BackendState{state}
	reg := peer.NewRegistry("viiwork-local", "test", backends, nil, 3*time.Second)
	bal := balancer.New(backends, 7, 4)
	h := NewMeshHandler(bal, reg, 30*time.Second)
	log := activity.NewLog()
	h.SetActivity(log)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"what is 2+2?"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	rid := findRequestRID(t, log)

	// Direct lookup.
	lookupReq := httptest.NewRequest("GET", "/v1/prompts?rid="+strconv.FormatInt(rid, 10), nil)
	lookupW := httptest.NewRecorder()
	h.ServeHTTP(lookupW, lookupReq)
	if lookupW.Code != 200 {
		t.Fatalf("/v1/prompts: expected 200, got %d", lookupW.Code)
	}
	var entry activity.PromptEntry
	if err := json.Unmarshal(lookupW.Body.Bytes(), &entry); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if entry.Prompt != "what is 2+2?" {
		t.Errorf("prompt = %q, want %q", entry.Prompt, "what is 2+2?")
	}
	if entry.Model != "test" {
		t.Errorf("model = %q, want %q", entry.Model, "test")
	}

	// Mesh fan-out entry point with an empty addr must resolve locally too.
	meshReq := httptest.NewRequest("GET", "/v1/mesh/prompt?rid="+strconv.FormatInt(rid, 10)+"&addr=", nil)
	meshW := httptest.NewRecorder()
	h.ServeHTTP(meshW, meshReq)
	if meshW.Code != 200 {
		t.Fatalf("/v1/mesh/prompt (local): expected 200, got %d", meshW.Code)
	}
	if !strings.Contains(meshW.Body.String(), "what is 2+2?") {
		t.Errorf("mesh prompt body missing prompt text: %s", meshW.Body.String())
	}
}

func TestPromptLookupNotFound(t *testing.T) {
	bal := balancer.New(nil, 7, 4)
	h := NewHandler(bal, "/models/test.gguf", 30*time.Second)
	h.SetActivity(activity.NewLog())

	req := httptest.NewRequest("GET", "/v1/prompts?rid=999999999", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown rid, got %d", w.Code)
	}
}

func TestMeshPromptProxiesToPeer(t *testing.T) {
	peerBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/prompts" && r.URL.Query().Get("rid") == "42" {
			json.NewEncoder(w).Encode(activity.PromptEntry{RequestID: 42, Model: "peer-model", Prompt: "prompt from peer"})
			return
		}
		http.NotFound(w, r)
	}))
	defer peerBackend.Close()

	localState := balancer.NewBackendState(0, "localhost:9001")
	localState.SetStatus(balancer.StatusHealthy)
	backends := []*balancer.BackendState{localState}
	peers := []*peer.PeerState{peer.NewPeerState(peerBackend.Listener.Addr().String())}
	reg := peer.NewRegistry("viiwork-local", "local-model", backends, peers, 3*time.Second)
	reg.PollOnce(context.Background())

	bal := balancer.New(backends, 7, 4)
	h := NewMeshHandler(bal, reg, 30*time.Second)
	h.SetActivity(activity.NewLog())

	req := httptest.NewRequest("GET", "/v1/mesh/prompt?rid=42&addr="+peerBackend.Listener.Addr().String(), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), "prompt from peer") {
		t.Errorf("expected proxied peer prompt, got %s", body)
	}
}

func TestPipelineRequestPromptStored(t *testing.T) {
	log := activity.NewLog()
	log.EmitRequestTask(activity.NewRequestID(), -1, "", "[pipeline] %s started", "translate-fi")
	rid := findRequestRID(t, log)
	log.StorePrompt(rid, "translate-fi", "hyvää huomenta")
	entry, ok := log.GetPrompt(rid)
	if !ok {
		t.Fatal("expected prompt to be stored")
	}
	if entry.Prompt != "hyvää huomenta" {
		t.Errorf("prompt = %q", entry.Prompt)
	}
}

// TestPromptHistoryEvictsAtCap pins the "in memory only, oldest-first eviction"
// rule so a long-running node cannot accumulate prompt text without bound.
//
// Written against a configured cap rather than the default: the default is a
// tunable number, and a test that hardcodes it fails for the wrong reason the
// next time it changes.
func TestPromptHistoryEvictsAtCap(t *testing.T) {
	const cap = 100
	log := activity.NewLogWithPromptHistory(cap)
	const n = cap + 30
	for i := 1; i <= n; i++ {
		log.StorePrompt(int64(i), "m", "prompt-"+strconv.Itoa(i))
	}

	// Everything past the cap must be gone.
	for _, rid := range []int64{1, 15, n - cap} {
		if _, ok := log.GetPrompt(rid); ok {
			t.Errorf("rid %d should have been evicted past the %d-entry cap", rid, cap)
		}
	}
	// The most recent cap entries must survive.
	for _, rid := range []int64{n - cap + 1, n - 1, n} {
		entry, ok := log.GetPrompt(rid)
		if !ok {
			t.Fatalf("rid %d should still be present", rid)
		}
		if entry.Prompt != "prompt-"+strconv.FormatInt(rid, 10) {
			t.Errorf("rid %d has wrong prompt %q", rid, entry.Prompt)
		}
	}
}

// The default must be the documented one — README, CHANGELOG and the example
// config all quote it, and the dashboard falls back to it for peers too old to
// report their own.
func TestDefaultPromptHistory(t *testing.T) {
	if activity.DefaultPromptHistory != 1000 {
		t.Errorf("DefaultPromptHistory = %d, want 1000 (documented)", activity.DefaultPromptHistory)
	}
	if got := activity.NewLog().PromptHistoryMax(); got != 1000 {
		t.Errorf("NewLog() max = %d, want 1000", got)
	}
}

// TestEmptyPromptNotStored guards the dashboard: a blank entry would still
// render a prompt link that opens an empty modal.
func TestEmptyPromptNotStored(t *testing.T) {
	log := activity.NewLog()
	log.StorePrompt(1, "m", "")
	if _, ok := log.GetPrompt(1); ok {
		t.Error("empty prompt should not be stored")
	}
}

// TestMeshPromptRejectsUnknownAddr is a security regression test. handleMeshPrompt
// fetches the addr it is given and echoes the response back, so accepting an
// arbitrary addr would make every node an SSRF probe for its own LAN — which
// on these hosts sits alongside IPMI and other management interfaces.
func TestMeshPromptRejectsUnknownAddr(t *testing.T) {
	var probed bool
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probed = true
		w.Write([]byte(`{"secret":"internal-service-data"}`))
	}))
	defer victim.Close()

	localState := balancer.NewBackendState(0, "localhost:9001")
	localState.SetStatus(balancer.StatusHealthy)
	backends := []*balancer.BackendState{localState}
	// Registry with NO peers: nothing is a legitimate forwarding target.
	reg := peer.NewRegistry("viiwork-local", "local-model", backends, nil, 3*time.Second)
	h := NewMeshHandler(balancer.New(backends, 7, 4), reg, 30*time.Second)
	h.SetActivity(activity.NewLog())

	req := httptest.NewRequest("GET", "/v1/mesh/prompt?rid=1&addr="+victim.Listener.Addr().String(), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if probed {
		t.Error("SSRF: node fetched an address that is not a configured peer")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for a non-peer addr, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "internal-service-data") {
		t.Error("SSRF: response from a non-peer host was echoed to the caller")
	}
}

// TestMeshPromptRidNotInjectable ensures rid is re-serialised from the parsed
// integer, so a crafted value cannot smuggle extra query parameters into the
// request this node makes against a peer.
func TestMeshPromptRidNotInjectable(t *testing.T) {
	var gotQuery string
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		json.NewEncoder(w).Encode(activity.PromptEntry{RequestID: 1, Prompt: "ok"})
	}))
	defer peerSrv.Close()

	localState := balancer.NewBackendState(0, "localhost:9001")
	localState.SetStatus(balancer.StatusHealthy)
	backends := []*balancer.BackendState{localState}
	peers := []*peer.PeerState{peer.NewPeerState(peerSrv.Listener.Addr().String())}
	reg := peer.NewRegistry("viiwork-local", "local-model", backends, peers, 3*time.Second)
	reg.PollOnce(context.Background())
	h := NewMeshHandler(balancer.New(backends, 7, 4), reg, 30*time.Second)
	h.SetActivity(activity.NewLog())

	// A rid that is not a plain integer must be rejected outright, never
	// concatenated into the peer URL.
	req := httptest.NewRequest("GET",
		"/v1/mesh/prompt?rid=1%26evil%3D1&addr="+peerSrv.Listener.Addr().String(), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for a non-integer rid, got %d", w.Code)
	}
	if strings.Contains(gotQuery, "evil") {
		t.Errorf("injected parameter reached the peer request: %q", gotQuery)
	}
}
