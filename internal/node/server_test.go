package node

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/activity"
	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/meshapi"
	"github.com/janit/viiwork/v2/web"
)

type serverFakes struct {
	inference atomic.Int64
	aliases   atomic.Int64
	health    struct{ healthy, total, models int }
	members   []mesh.Member
	cancel    context.CancelFunc
}

// fakeServerDeps is NewServer's dependencies with fakes; tests adjust them.
func fakeServerDeps(t *testing.T) ServerDeps {
	d, _ := fakeServer(t)
	return d
}

func fakeServer(t *testing.T) (ServerDeps, *serverFakes) {
	t.Helper()
	f := &serverFakes{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.cancel = cancel
	d := ServerDeps{
		Self:      "gb1",
		Version:   "v2.0.0-test",
		Started:   time.Now().Add(-time.Minute),
		StreamCtx: ctx,
		Inference: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.inference.Add(1)
			if r.Header.Get("X-Test-Panic") != "" {
				panic("boom")
			}
			if r.URL.Path == meshapi.PathModels && r.Method != http.MethodGet {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, `{"inference":true}`)
		}),
		Aliases: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.aliases.Add(1)
			_, _ = io.WriteString(w, `{"aliases":true}`)
		}),
		AliasInfo: func() meshapi.AliasesResponse { return meshapi.AliasesResponse{Aliases: []meshapi.AliasInfo{}} },
		Status:    func() meshapi.NodeStatus { return meshapi.NodeStatus{Node: "gb1", Ver: "v2.0.0-test"} },
		Cluster:   func() meshapi.ClusterResponse { return meshapi.ClusterResponse{View: "gb1", Mesh: "open"} },
		Members:   func() []mesh.Member { return f.members },
		Activity:  activity.NewLog(),
		Health:    func() (int, int, int) { return f.health.healthy, f.health.total, f.health.models },
	}
	return d, f
}

func get(h http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestServerHealth(t *testing.T) {
	cases := []struct {
		id                     string
		healthy, total, models int
		code                   int
		status                 string
	}{
		{"R1", 0, 2, 1, 503, "unhealthy"},
		{"R2", 1, 2, 1, 200, "ok"},
		{"R3", 0, 0, 0, 200, "ok"},
	}
	for _, c := range cases {
		d, f := fakeServer(t)
		f.health.healthy, f.health.total, f.health.models = c.healthy, c.total, c.models
		rec := get(NewServer(d), "/health")
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != c.code || body["status"] != c.status || body["node"] != "gb1" || body["version"] != "v2.0.0-test" ||
			body["backends_healthy"] != float64(c.healthy) || body["backends_total"] != float64(c.total) || body["uptime_seconds"] == nil {
			t.Errorf("%s: %d %s", c.id, rec.Code, rec.Body.String())
		}
	}
}

func TestServerRoutes(t *testing.T) {
	d, f := fakeServer(t)
	h := NewServer(d)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")))
	if f.inference.Load() != 1 || rec.Body.String() != `{"inference":true}` {
		t.Errorf("R4: %d %q", f.inference.Load(), rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/aliases/x", strings.NewReader("{}")))
	if f.aliases.Load() != 1 {
		t.Errorf("R5: alias handler not reached: %q", rec.Body.String())
	}
	if rec := get(h, meshapi.PathStatus); !strings.Contains(rec.Body.String(), `"node":"gb1"`) || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("R6 status: %q", rec.Body.String())
	}
	if rec := get(h, meshapi.PathCluster); !strings.Contains(rec.Body.String(), `"view":"gb1"`) {
		t.Errorf("R6 cluster: %q", rec.Body.String())
	}
	for path, page := range map[string][]byte{"/mesh": web.MeshHTML, "/": web.DashboardHTML, "/chat": web.ChatHTML, "/prompt": web.PromptHTML} {
		if rec := get(h, path); rec.Code != 200 || rec.Header().Get("Content-Type") != "text/html" || rec.Body.String() != string(page) {
			t.Errorf("R7 %s: %d %q", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	if rec := get(h, "/nope"); rec.Code != 404 {
		t.Errorf("R14: %d", rec.Code)
	}
	if rec := get(h, "/v1/metrics"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"available":false`) {
		t.Errorf("metrics without a collector: %d %q", rec.Code, rec.Body.String())
	}
}

func TestServerPreflightNeverReachesInference(t *testing.T) {
	d, f := fakeServer(t)
	d.CORS = testCORS()
	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	NewServer(d).ServeHTTP(rec, req)
	if rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Origin") == "" || f.inference.Load() != 0 {
		t.Errorf("R8: %d, inference calls %d", rec.Code, f.inference.Load())
	}
}

func TestServerRecovers(t *testing.T) {
	d, _ := fakeServer(t)
	h := NewServer(d)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	req.Header.Set("X-Test-Panic", "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Errorf("R9: %d", rec.Code)
	}
	if rec := get(h, "/health"); rec.Code != 200 {
		t.Errorf("R9: not serving after a panic: %d", rec.Code)
	}
}

func TestServerActivityStream(t *testing.T) {
	d, f := fakeServer(t)
	d.Activity.Emit("system", -1, "backlog event")
	srv := httptest.NewServer(NewServer(d))
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + meshapi.PathActivityStream)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") {
				lines <- sc.Text()
			}
		}
		close(lines)
	}()
	next := func() string {
		select {
		case l := <-lines:
			return l
		case <-time.After(2 * time.Second):
			return ""
		}
	}
	if l := next(); !strings.Contains(l, "backlog event") {
		t.Fatalf("R10: first event %q", l)
	}
	d.Activity.Emit("system", -1, "live event")
	if l := next(); !strings.Contains(l, "live event") {
		t.Fatalf("R10: live event %q", l)
	}
	f.cancel()
	deadline := time.After(time.Second)
	for {
		select {
		case _, open := <-lines:
			if !open {
				return
			}
		case <-deadline:
			t.Fatal("R10: the stream did not end within 1s of StreamCtx ending")
		}
	}
}

func TestServerMeshPrompt(t *testing.T) {
	var gotPath atomic.Value
	member := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.RequestURI())
		_, _ = io.WriteString(w, `{"rid":7,"model":"m","prompt":"from the member"}`)
	}))
	t.Cleanup(member.Close)

	d, f := fakeServer(t)
	f.members = []mesh.Member{memberAt(t, "gb2", member, meshapi.MemberAlive, meshapi.RoleNode, false)}
	h := NewServer(d)

	addr := f.members[0].APIAddr()
	if rec := get(h, meshapi.PathMeshPrompt+"?addr="+addr+"&rid=7"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "from the member") || gotPath.Load() != "/v1/prompts?rid=7" {
		t.Errorf("R11: %d %q path %v", rec.Code, rec.Body.String(), gotPath.Load())
	}
	if rec := get(h, meshapi.PathMeshPrompt+"?addr=10.0.0.9:8086&rid=7"); rec.Code != 400 || !strings.Contains(rec.Body.String(), "unknown peer") {
		t.Errorf("R12: %d %q", rec.Code, rec.Body.String())
	}

	rid := activity.NewRequestID()
	d.Activity.StorePrompt(rid, "m", "local prompt")
	rec := get(h, meshapi.PathMeshPrompt+"?rid="+jsonInt(rid))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "local prompt") {
		t.Errorf("R13: %d %q", rec.Code, rec.Body.String())
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
