package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/peer"
)

// inferenceServer stands in for one node's llama-server or a peer viiwork: it
// answers every POST with a fixed reply and remembers what it was asked.
type inferenceServer struct {
	*httptest.Server
	hits      atomic.Int32
	lastQuery atomic.Value // string
}

func newInferenceServer(t *testing.T, reply string) *inferenceServer {
	s := &inferenceServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.lastQuery.Store(r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"` + reply + `"}}]}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *inferenceServer) query() string {
	v, _ := s.lastQuery.Load().(string)
	return v
}

// pinnedMesh is a three-host mesh as gb1 sees it: gb1 serves "m" locally and
// idle, gb2 is an idle peer, gb3 is a peer three requests deep. Unpinned
// routing therefore has exactly one answer — local — which is what lets the
// pin tests prove they changed the outcome rather than agreed with it.
type pinnedMesh struct {
	h               *Handler
	local, gb2, gb3 *inferenceServer
}

func newPinnedMesh(t *testing.T, extra ...*peer.PeerState) *pinnedMesh {
	m := &pinnedMesh{
		local: newInferenceServer(t, "from-gb1"),
		gb2:   newInferenceServer(t, "from-gb2"),
		gb3:   newInferenceServer(t, "from-gb3"),
	}
	state := balancer.NewBackendState(0, m.local.Listener.Addr().String())
	state.SetStatus(balancer.StatusHealthy)
	backends := []*balancer.BackendState{state}

	p2 := peer.NewPeerState(m.gb2.Listener.Addr().String())
	p2.Update(peer.StatusResponse{NodeID: "viiwork-gb2", Hostname: "gb2", Models: []string{"m"}, HealthyBackends: 1, TotalBackends: 1})
	p3 := peer.NewPeerState(m.gb3.Listener.Addr().String())
	p3.Update(peer.StatusResponse{NodeID: "viiwork-gb3", Hostname: "gb3", Models: []string{"m"}, HealthyBackends: 1, TotalBackends: 1, TotalInFlight: 3})
	peers := append([]*peer.PeerState{p2, p3}, extra...)

	reg := peer.NewRegistry("viiwork-gb1", "m", backends, peers, 3*time.Second)
	reg.SetLocation("gb1", "gb1:8080")
	m.h = NewMeshHandler(balancer.New(backends, 7, 4), reg, 30*time.Second)
	return m
}

func (m *pinnedMesh) post(t *testing.T, url, model string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", url, strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	m.h.ServeHTTP(w, req)
	return w
}

// hits is local/gb2/gb3.
func (m *pinnedMesh) hits() [3]int32 {
	return [3]int32{m.local.hits.Load(), m.gb2.hits.Load(), m.gb3.hits.Load()}
}

func bodyOf(w *httptest.ResponseRecorder) string {
	b, _ := io.ReadAll(w.Body)
	return string(b)
}

func TestHostPinAbsentRoutesAsBefore(t *testing.T) {
	m := newPinnedMesh(t)
	w := m.post(t, "/v1/chat/completions", "m")
	if w.Code != 200 || !strings.Contains(bodyOf(w), "from-gb1") {
		t.Fatalf("unpinned: code %d body %s", w.Code, bodyOf(w))
	}
	if got := m.hits(); got != [3]int32{1, 0, 0} {
		t.Errorf("unpinned request should stay local; hits local/gb2/gb3 = %v", got)
	}
	if w.Header().Get("X-Viiwork-Origin") != "" {
		t.Error("a locally served request must not carry X-Viiwork-Origin")
	}
}

func TestHostPinMeshIsTheDefault(t *testing.T) {
	m := newPinnedMesh(t)
	for _, q := range []string{"?host=mesh", "?host=MESH", "?host="} {
		w := m.post(t, "/v1/chat/completions"+q, "m")
		if w.Code != 200 || !strings.Contains(bodyOf(w), "from-gb1") {
			t.Fatalf("%s: code %d body %s", q, w.Code, bodyOf(w))
		}
	}
	if got := m.hits(); got != [3]int32{3, 0, 0} {
		t.Errorf("host=mesh must route exactly like no host; hits = %v", got)
	}
}

func TestHostPinToLocalHostServesLocally(t *testing.T) {
	m := newPinnedMesh(t)
	for _, pin := range []string{"gb1", "GB1"} {
		w := m.post(t, "/v1/chat/completions?host="+pin, "m")
		if w.Code != 200 || !strings.Contains(bodyOf(w), "from-gb1") {
			t.Fatalf("host=%s: code %d body %s", pin, w.Code, bodyOf(w))
		}
	}
	if got := m.hits(); got != [3]int32{2, 0, 0} {
		t.Errorf("pinned to the local host, no peer may be called; hits = %v", got)
	}
}

func TestHostPinBeatsTheBalancer(t *testing.T) {
	m := newPinnedMesh(t)
	// The counterfactual first: with the local backend and gb2 both idle and
	// gb3 three deep, the balancer never picks gb3 on its own.
	if w := m.post(t, "/v1/chat/completions", "m"); !strings.Contains(bodyOf(w), "from-gb1") {
		t.Fatalf("precondition: unpinned routing should have gone local, got %s", bodyOf(w))
	}

	w := m.post(t, "/v1/chat/completions?host=gb3", "m")
	if w.Code != 200 {
		t.Fatalf("pinned to gb3: code %d body %s", w.Code, bodyOf(w))
	}
	if !strings.Contains(bodyOf(w), "from-gb3") {
		t.Fatalf("pinned to gb3 but answered by %s", bodyOf(w))
	}
	if got := m.hits(); got != [3]int32{1, 0, 1} {
		t.Errorf("hits local/gb2/gb3 = %v, want the pinned request on gb3 only", got)
	}
	if origin, want := w.Header().Get("X-Viiwork-Origin"), m.gb3.Listener.Addr().String(); origin != want {
		t.Errorf("X-Viiwork-Origin = %q, want gb3's address %q", origin, want)
	}
	// The pin travels with the forward, so the pinned node could apply it too.
	if q := m.gb3.query(); q != "host=gb3" {
		t.Errorf("gb3 saw query %q, want host=gb3", q)
	}
}

func TestHostPinAppliesToEveryInferencePath(t *testing.T) {
	m := newPinnedMesh(t)
	for _, path := range []string{"/v1/chat/completions", "/v1/completions", "/v1/embeddings"} {
		w := m.post(t, path+"?host=gb3", "m")
		if w.Code != 200 || !strings.Contains(bodyOf(w), "from-gb3") {
			t.Errorf("%s: code %d body %s", path, w.Code, bodyOf(w))
		}
	}
	if got := m.hits(); got != [3]int32{0, 0, 3} {
		t.Errorf("hits local/gb2/gb3 = %v", got)
	}
}

func TestHostPinUnknownHostIs404NotFallback(t *testing.T) {
	m := newPinnedMesh(t)
	w := m.post(t, "/v1/chat/completions?host=gb9", "m")
	if w.Code != 404 {
		t.Fatalf("code %d, want 404; body %s", w.Code, bodyOf(w))
	}
	var resp struct {
		Error struct{ Message, Type string } `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("404 body is not JSON: %v", err)
	}
	if !strings.Contains(resp.Error.Message, `"gb9"`) || !strings.Contains(resp.Error.Message, `"m"`) {
		t.Errorf("404 should name host and model, got %q", resp.Error.Message)
	}
	if got := m.hits(); got != [3]int32{0, 0, 0} {
		t.Errorf("a pin that does not hold must not fall back to the mesh; hits = %v", got)
	}
}

func TestHostPinUnknownModelStaysGeneric404(t *testing.T) {
	// "No such model anywhere" is reported as before, not blamed on the host.
	m := newPinnedMesh(t)
	w := m.post(t, "/v1/chat/completions?host=gb2", "nope")
	if w.Code != 404 {
		t.Fatalf("code %d, want 404", w.Code)
	}
	if strings.Contains(bodyOf(w), "gb2") {
		t.Errorf("an unknown model should not read as a host problem: %s", bodyOf(w))
	}
}

func TestHostPinUnreachableHostIs404WithoutDialling(t *testing.T) {
	// gb4 was serving m and then went away. MarkUnreachable is what the poll
	// loop calls; it keeps the hostname and drops the models.
	gb4 := newInferenceServer(t, "from-gb4")
	p4 := peer.NewPeerState(gb4.Listener.Addr().String())
	p4.Update(peer.StatusResponse{NodeID: "viiwork-gb4", Hostname: "gb4", Models: []string{"m"}, HealthyBackends: 1, TotalBackends: 1})
	p4.MarkUnreachable()
	m := newPinnedMesh(t, p4)

	w := m.post(t, "/v1/chat/completions?host=gb4", "m")
	if w.Code != 404 {
		t.Fatalf("code %d, want 404; body %s", w.Code, bodyOf(w))
	}
	if gb4.hits.Load() != 0 {
		t.Error("an unreachable pinned host must not be dialled")
	}
	if got := m.hits(); got != [3]int32{0, 0, 0} {
		t.Errorf("no fallback: hits local/gb2/gb3 = %v", got)
	}
}

func TestHostPinIgnoredOnForwardedRequest(t *testing.T) {
	// A request that already hopped once arrives from a known peer and is
	// served locally whatever it says: the pinned host is by definition the
	// node it arrived at, so the parameter is redundant here and harmless.
	m := newPinnedMesh(t)
	req := httptest.NewRequest("POST", "/v1/chat/completions?host=gb3",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderForwarded, "viiwork-gb2")
	w := httptest.NewRecorder()
	m.h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(bodyOf(w), "from-gb1") {
		t.Fatalf("forwarded: code %d body %s", w.Code, bodyOf(w))
	}
	if got := m.hits(); got != [3]int32{1, 0, 0} {
		t.Errorf("a forwarded request must stay local; hits = %v", got)
	}
}

func TestHostPinMalformedIs400(t *testing.T) {
	m := newPinnedMesh(t)
	for _, bad := range []string{"gb2/x", "gb2%20x", "gb2%3Bx", strings.Repeat("h", 300)} {
		w := m.post(t, "/v1/chat/completions?host="+bad, "m")
		if w.Code != 400 {
			t.Errorf("host=%q: code %d, want 400", bad, w.Code)
		}
	}
	if got := m.hits(); got != [3]int32{0, 0, 0} {
		t.Errorf("a malformed pin must not route anywhere; hits = %v", got)
	}
}

func TestSanitizeHost(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{"   ", "", true},
		{"mesh", "", true},
		{"MESH", "", true},
		{"gb2", "gb2", true},
		{" gb2\n", "gb2", true},
		{"GB2", "GB2", true},
		{"192.168.1.42", "192.168.1.42", true},
		{"[fd7a:115c:a1e0::1]", "[fd7a:115c:a1e0::1]", true},
		{"node_1.tail.ts.net", "node_1.tail.ts.net", true},
		{"gb2/x", "", false},
		{"gb2 x", "", false},
		{"gb2;x", "", false},
		{"gb2?x=1", "", false},
		{strings.Repeat("h", maxHostLen+1), "", false},
	}
	for _, tc := range cases {
		got, ok := sanitizeHost(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("sanitizeHost(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
