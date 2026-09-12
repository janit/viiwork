package aliascli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/meshauth"
	"github.com/janit/viiwork/v2/meshapi"
)

const (
	writtenTS = 1789120000123
	writtenAt = "2026-09-11T09:46:40.123Z"
)

var testSecret = bytes.Repeat([]byte{9}, 32)

type request struct {
	method, path, body string
	header             http.Header
	signer             string // verified meshauth signer, "" if none verified
}

// fakeNode is a node's alias API and /v1/cluster, with three members whose
// /v1/aliases answers the test controls.
type fakeNode struct {
	srv          *httptest.Server
	mu           sync.Mutex
	requests     []request
	infos        []meshapi.AliasInfo
	table        meshapi.AliasTable
	writeStatus  map[string]int // alias name -> forced error status
	writeMessage string
	clusterFail  bool
	clusterCalls atomic.Int64
	members      []*fakeMember
	secret       []byte // verify signatures with this secret when set
}

type fakeMember struct {
	srv   *httptest.Server
	calls atomic.Int64
	// answer is the member's alias list on its nth call (1-based).
	answer func(n int64) []meshapi.AliasInfo
}

func newFakeMember(t *testing.T, answer func(n int64) []meshapi.AliasInfo) *fakeMember {
	m := &fakeMember{answer: answer}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := m.calls.Add(1)
		_ = json.NewEncoder(w).Encode(meshapi.AliasesResponse{Aliases: m.answer(n)})
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func converged(name string) []meshapi.AliasInfo {
	return []meshapi.AliasInfo{{Name: name, Target: "Qwen3.8-27B", Fallbacks: []string{}, UpdatedAt: writtenAt, UpdatedBy: "gb1", State: "ok"}}
}

func stale(name string) []meshapi.AliasInfo {
	return []meshapi.AliasInfo{{Name: name, Target: "old", Fallbacks: []string{}, UpdatedAt: "2026-09-01T00:00:00.000Z", UpdatedBy: "gb1", State: "ok"}}
}

func newFakeNode(t *testing.T, members ...*fakeMember) *fakeNode {
	n := &fakeNode{members: members, writeStatus: map[string]int{}, table: meshapi.AliasTable{V: 1, Aliases: map[string]meshapi.AliasEntry{}}}
	n.srv = httptest.NewServer(http.HandlerFunc(n.serve))
	t.Cleanup(n.srv.Close)
	return n
}

func (n *fakeNode) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rq := request{method: r.Method, path: r.URL.RequestURI(), body: string(body), header: r.Header.Clone()}
	if n.secret != nil {
		signer, _ := meshauth.NewSigner(n.secret, "gb1")
		if _, who, err := signer.VerifyRequest(r, body); err == nil {
			rq.signer = who
		}
	}
	n.mu.Lock()
	n.requests = append(n.requests, rq)
	n.mu.Unlock()

	path := r.URL.EscapedPath()
	switch {
	case path == meshapi.PathCluster:
		n.clusterCalls.Add(1)
		if n.clusterFail {
			http.Error(w, "boom", 500)
			return
		}
		resp := meshapi.ClusterResponse{View: "gb1"}
		for i, m := range n.members {
			_, port, _ := net.SplitHostPort(m.srv.Listener.Addr().String())
			var p int
			fmt.Sscan(port, &p)
			resp.Members = append(resp.Members, meshapi.Member{Node: fmt.Sprintf("m%d", i), Addr: "127.0.0.1", Role: meshapi.RoleNode, State: meshapi.MemberAlive, Status: &meshapi.NodeStatus{APIPort: p}})
		}
		resp.Members = append(resp.Members, meshapi.Member{Node: "dead", Addr: "127.0.0.1", Role: meshapi.RoleNode, State: meshapi.MemberDead})
		_ = json.NewEncoder(w).Encode(resp)
	case path == meshapi.PathAliases && r.URL.Query().Get("table") == "1":
		_ = json.NewEncoder(w).Encode(n.table)
	case path == meshapi.PathAliases:
		_ = json.NewEncoder(w).Encode(meshapi.AliasesResponse{Aliases: n.infos})
	case strings.HasPrefix(path, meshapi.PathAliases+"/"):
		name := strings.TrimSuffix(strings.TrimPrefix(path, meshapi.PathAliases+"/"), meshapi.AliasRevertSuffix)
		if status := n.writeStatus[name]; status != 0 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(meshapi.ErrorResponse{Error: meshapi.ErrorBody{Message: n.writeMessage, Type: "invalid_request"}})
			return
		}
		entry := meshapi.AliasEntry{Target: "Qwen3.8-27B", Fallbacks: []string{}, Ver: 1, TS: writtenTS, By: "gb1"}
		switch {
		case r.Method == http.MethodPut:
			var req meshapi.AliasWriteRequest
			_ = json.Unmarshal(body, &req)
			entry.Target, entry.Fallbacks = req.Target, req.Fallbacks
		case r.Method == http.MethodDelete:
			entry.Ver, entry.Target, entry.Deleted = 2, "", true
		default:
			entry.Ver = 3
		}
		_ = json.NewEncoder(w).Encode(meshapi.AliasBroadcast{Name: name, Entry: entry})
	default:
		http.NotFound(w, r)
	}
}

func (n *fakeNode) writes() []request {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []request
	for _, r := range n.requests {
		if r.method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

type result struct {
	code           int
	stdout, stderr string
	elapsed        time.Duration
}

type runOpts struct {
	env      map[string]string
	files    map[string]string
	dialLog  *[]string // when set, every dial is recorded and sent to redirect
	redirect string
}

func run(t *testing.T, o runOpts, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	client := &http.Client{}
	if o.dialLog != nil {
		var mu sync.Mutex
		client.Transport = &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			mu.Lock()
			*o.dialLog = append(*o.dialLog, addr)
			mu.Unlock()
			return (&net.Dialer{}).DialContext(ctx, network, o.redirect)
		}}
	}
	env := Env{
		Stdout: &stdout,
		Stderr: &stderr,
		LookupEnv: func(k string) (string, bool) {
			v, ok := o.env[k]
			return v, ok
		},
		Hostname: func() (string, error) { return "testhost", nil },
		Client:   client,
		ReadFile: func(p string) ([]byte, error) {
			if s, ok := o.files[p]; ok {
				return []byte(s), nil
			}
			return nil, errors.New("no such file")
		},
	}
	start := time.Now()
	code := Run(context.Background(), args, env)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String(), elapsed: time.Since(start)}
}

func nodeAddr(n *fakeNode) string { return n.srv.Listener.Addr().String() }

func TestCLIList(t *testing.T) {
	node := newFakeNode(t)
	resolved := "Qwen3.8-27B"
	node.infos = []meshapi.AliasInfo{
		{Name: "stable-coder", Target: "Qwen3.8-27B", Fallbacks: []string{"gemma-4-31B-it", "granite"}, UpdatedAt: writtenAt, UpdatedBy: "gb1", Resolved: &resolved, State: "ok"},
		{Name: "dead", Target: "nosuch", Fallbacks: []string{}, UpdatedAt: writtenAt, UpdatedBy: "node-a", State: "unavailable"},
	}
	r := run(t, runOpts{}, "ls", "--node", nodeAddr(node))
	lines := strings.Split(strings.TrimRight(r.stdout, "\n"), "\n")
	if r.code != 0 || len(lines) != 3 {
		t.Fatalf("C1: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if f := strings.Fields(lines[0]); strings.Join(f, " ") != "NAME TARGET FALLBACKS RESOLVED STATE UPDATED BY" {
		t.Errorf("C1: header %q", lines[0])
	}
	if f := strings.Fields(lines[1]); strings.Join(f, " ") != "stable-coder Qwen3.8-27B gemma-4-31B-it,granite Qwen3.8-27B ok "+writtenAt+" gb1" {
		t.Errorf("C1: row %q", lines[1])
	}
	if f := strings.Fields(lines[2]); strings.Join(f, " ") != "dead nosuch - - unavailable "+writtenAt+" node-a" {
		t.Errorf("C1: row %q", lines[2])
	}
	if strings.Index(lines[0], "TARGET") != strings.Index(lines[1], "Qwen3.8-27B") {
		t.Errorf("C1: columns are not aligned:\n%s", r.stdout)
	}
}

func threeConverged(t *testing.T) []*fakeMember {
	var ms []*fakeMember
	for i := 0; i < 3; i++ {
		ms = append(ms, newFakeMember(t, func(int64) []meshapi.AliasInfo { return converged("stable-coder") }))
	}
	return ms
}

func TestCLISet(t *testing.T) {
	t.Run("C2 body and output", func(t *testing.T) {
		node := newFakeNode(t, threeConverged(t)...)
		r := run(t, runOpts{}, "set", "stable-coder", "Qwen3.8-27B", "--fallback", "gemma-4-31B-it", "--fallback", "granite", "--node", nodeAddr(node))
		w := node.writes()
		if r.code != 0 || len(w) != 1 || w[0].method != http.MethodPut || w[0].path != "/v1/aliases/stable-coder" ||
			w[0].body != `{"target":"Qwen3.8-27B","fallbacks":["gemma-4-31B-it","granite"],"force":false}` {
			t.Fatalf("exit %d, writes %+v, stderr %q", r.code, w, r.stderr)
		}
		if !strings.HasPrefix(r.stdout, "stable-coder -> Qwen3.8-27B (ver 1)") {
			t.Errorf("stdout %q", r.stdout)
		}
		if w[0].header.Get(meshauth.HeaderAuth) != "" {
			t.Error("an unsigned write carries a signature header")
		}
	})

	t.Run("C3 signed", func(t *testing.T) {
		node := newFakeNode(t)
		node.secret = testSecret
		env := map[string]string{"VIIWORK_MESH_SECRET": base64.StdEncoding.EncodeToString(testSecret)}
		r := run(t, runOpts{env: env}, "set", "stable-coder", "Qwen3.8-27B", "--node", nodeAddr(node), "--wait=false")
		if w := node.writes(); r.code != 0 || len(w) != 1 || w[0].signer != "viiwork-cli@testhost" {
			t.Errorf("exit %d, writes %+v, stderr %q", r.code, w, r.stderr)
		}
	})

	t.Run("C4 bad secret", func(t *testing.T) {
		node := newFakeNode(t)
		r := run(t, runOpts{env: map[string]string{"VIIWORK_MESH_SECRET": "AAAA"}}, "set", "stable-coder", "Qwen3.8-27B", "--node", nodeAddr(node))
		if r.code != 2 || !strings.Contains(r.stderr, "VIIWORK_MESH_SECRET") || len(node.writes()) != 0 {
			t.Errorf("exit %d, stderr %q", r.code, r.stderr)
		}
	})

	for _, c := range []struct {
		id     string
		status int
		msg    string
		hint   string
	}{
		{"C5", 401, "alias writes need a valid X-Viiwork-Auth signature (sign with the mesh secret)", "this mesh is secured: set VIIWORK_MESH_SECRET to the mesh secret"},
		{"C6", 403, "alias writes are accepted only from this machine in an open mesh", "this mesh is open"},
		{"C7", 409, `"nosuch" is not served by any node (use --force to pre-stage it)`, ""},
	} {
		t.Run(c.id, func(t *testing.T) {
			node := newFakeNode(t)
			node.writeStatus["stable-coder"] = c.status
			node.writeMessage = c.msg
			r := run(t, runOpts{}, "set", "stable-coder", "nosuch", "--node", nodeAddr(node))
			if r.code != 1 || !strings.Contains(r.stderr, c.msg) || !strings.Contains(r.stderr, c.hint) {
				t.Errorf("exit %d, stderr %q", r.code, r.stderr)
			}
		})
	}
}

func TestCLIConvergence(t *testing.T) {
	t.Run("C8 partial convergence", func(t *testing.T) {
		members := []*fakeMember{
			newFakeMember(t, func(int64) []meshapi.AliasInfo { return converged("stable-coder") }),
			newFakeMember(t, func(int64) []meshapi.AliasInfo { return converged("stable-coder") }),
			newFakeMember(t, func(int64) []meshapi.AliasInfo { return stale("stable-coder") }),
		}
		node := newFakeNode(t, members...)
		resolved := "Qwen3.8-27B"
		node.infos = []meshapi.AliasInfo{{Name: "stable-coder", Resolved: &resolved}}
		r := run(t, runOpts{}, "set", "stable-coder", "Qwen3.8-27B", "--node", nodeAddr(node), "--timeout", "600ms")
		last := lastLine(r.stdout)
		if r.code != 0 || !strings.HasSuffix(last, "2/3 alive members") || !strings.HasPrefix(last, "stable-coder -> Qwen3.8-27B (ver 1):") || r.elapsed > 1500*time.Millisecond {
			t.Errorf("exit %d after %s, stdout %q", r.code, r.elapsed, r.stdout)
		}
	})

	t.Run("C9 converged on the second round", func(t *testing.T) {
		var members []*fakeMember
		for i := 0; i < 3; i++ {
			members = append(members, newFakeMember(t, func(n int64) []meshapi.AliasInfo {
				if n == 1 {
					return stale("stable-coder")
				}
				return converged("stable-coder")
			}))
		}
		node := newFakeNode(t, members...)
		r := run(t, runOpts{}, "set", "stable-coder", "Qwen3.8-27B", "--node", nodeAddr(node), "--timeout", "5s")
		if r.code != 0 || !strings.HasSuffix(lastLine(r.stdout), "3/3 alive members") || r.elapsed > 2*time.Second {
			t.Errorf("exit %d after %s, stdout %q", r.code, r.elapsed, r.stdout)
		}
	})

	t.Run("C10 cluster unavailable", func(t *testing.T) {
		node := newFakeNode(t)
		node.clusterFail = true
		r := run(t, runOpts{}, "set", "stable-coder", "Qwen3.8-27B", "--node", nodeAddr(node))
		if r.code != 0 || !strings.Contains(r.stdout, "convergence unknown:") {
			t.Errorf("exit %d, stdout %q", r.code, r.stdout)
		}
	})

	t.Run("C11 no wait", func(t *testing.T) {
		node := newFakeNode(t, threeConverged(t)...)
		r := run(t, runOpts{}, "set", "stable-coder", "Qwen3.8-27B", "--wait=false", "--node", nodeAddr(node))
		if r.code != 0 || node.clusterCalls.Load() != 0 {
			t.Errorf("exit %d, cluster calls %d", r.code, node.clusterCalls.Load())
		}
	})

	t.Run("C12 rm", func(t *testing.T) {
		var members []*fakeMember
		for i := 0; i < 3; i++ {
			members = append(members, newFakeMember(t, func(int64) []meshapi.AliasInfo { return nil }))
		}
		node := newFakeNode(t, members...)
		r := run(t, runOpts{}, "rm", "stable-coder", "--node", nodeAddr(node))
		if r.code != 0 || lastLine(r.stdout) != "stable-coder deleted (ver 2): 3/3 alive members" {
			t.Errorf("exit %d, stdout %q", r.code, r.stdout)
		}
	})

	t.Run("revert", func(t *testing.T) {
		node := newFakeNode(t)
		r := run(t, runOpts{}, "revert", "stable-coder", "--node", nodeAddr(node), "--wait=false")
		if w := node.writes(); r.code != 0 || len(w) != 1 || w[0].method != http.MethodPost || w[0].path != "/v1/aliases/stable-coder/revert" || !strings.HasPrefix(r.stdout, "stable-coder -> Qwen3.8-27B (ver 3)") {
			t.Errorf("exit %d, writes %+v, stdout %q", r.code, w, r.stdout)
		}
	})
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

func TestCLIHistoryExport(t *testing.T) {
	node := newFakeNode(t)
	node.table.Aliases["stable-coder"] = meshapi.AliasEntry{
		Target: "Qwen3.8-27B", Fallbacks: []string{"gemma-4-31B-it"}, Ver: 3, TS: writtenTS, By: "gb1",
		History: []meshapi.AliasVersion{
			{Target: "gemma-4-31B-it", Fallbacks: []string{}, TS: writtenTS - 60000, By: "node-a"},
			{Target: "old", Fallbacks: []string{"a", "b"}, TS: writtenTS - 120000, By: "gb2"},
		},
	}
	gone := meshapi.AliasEntry{Fallbacks: []string{}, Ver: 2, TS: writtenTS, By: "gb1", Deleted: true}
	node.table.Aliases["gone"] = gone

	r := run(t, runOpts{}, "history", "stable-coder", "--node", nodeAddr(node))
	want := "ver 3  2026-09-11T09:46:40.123Z  gb1  -> Qwen3.8-27B [fallbacks: gemma-4-31B-it]\n" +
		"       2026-09-11T09:45:40.123Z  node-a  -> gemma-4-31B-it\n" +
		"       2026-09-11T09:44:40.123Z  gb2  -> old [fallbacks: a, b]\n"
	if r.code != 0 || r.stdout != want {
		t.Errorf("C13: exit %d\n got %q\nwant %q", r.code, r.stdout, want)
	}
	if r := run(t, runOpts{}, "history", "gone", "--node", nodeAddr(node)); r.code != 0 || !strings.HasPrefix(r.stdout, "ver 2  2026-09-11T09:46:40.123Z  gb1  deleted") {
		t.Errorf("tombstone history: %q", r.stdout)
	}
	if r := run(t, runOpts{}, "history", "nosuch", "--node", nodeAddr(node)); r.code != 1 || !strings.Contains(r.stderr, "alias nosuch not found") {
		t.Errorf("C14: exit %d, stderr %q", r.code, r.stderr)
	}

	r = run(t, runOpts{}, "export", "--node", nodeAddr(node))
	var back meshapi.AliasTable
	if err := json.Unmarshal([]byte(r.stdout), &back); err != nil || r.code != 0 {
		t.Fatalf("C15: %v, %q", err, r.stdout)
	}
	orig, _ := json.Marshal(node.table)
	again, _ := json.Marshal(back)
	if string(orig) != string(again) || !strings.Contains(r.stdout, "\n  \"aliases\"") {
		t.Errorf("C15: exported %s", r.stdout)
	}
}

const importFile = `{"v":1,"aliases":{
	"zeta":{"target":"Qwen3.8-27B","fallbacks":["granite"],"ver":4,"ts":1,"by":"gb1","deleted":false},
	"gone":{"target":"","fallbacks":[],"ver":2,"ts":1,"by":"gb1","deleted":true},
	"alpha":{"target":"gemma-4-31B-it","fallbacks":[],"ver":1,"ts":1,"by":"gb1","deleted":false}}}`

func TestCLIImport(t *testing.T) {
	t.Run("C16", func(t *testing.T) {
		node := newFakeNode(t)
		r := run(t, runOpts{files: map[string]string{"table.json": importFile}}, "import", "table.json", "--node", nodeAddr(node))
		w := node.writes()
		if r.code != 0 || len(w) != 2 || w[0].path != "/v1/aliases/alpha" || w[1].path != "/v1/aliases/zeta" ||
			w[0].body != `{"target":"gemma-4-31B-it","fallbacks":[],"force":true}` || !strings.Contains(r.stdout, "imported 2 of 2") {
			t.Errorf("exit %d, writes %+v, stdout %q, stderr %q", r.code, w, r.stdout, r.stderr)
		}
	})

	t.Run("C17", func(t *testing.T) {
		node := newFakeNode(t)
		node.writeStatus["alpha"] = 409
		node.writeMessage = "conflict"
		r := run(t, runOpts{files: map[string]string{"table.json": importFile}}, "import", "table.json", "--node", nodeAddr(node))
		if r.code != 1 || !strings.Contains(r.stderr, "alpha") || len(node.writes()) != 2 || !strings.Contains(r.stdout, "imported 1 of 2") {
			t.Errorf("exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
		}
	})
}

func TestCLIConfig(t *testing.T) {
	node := newFakeNode(t)
	node.secret = testSecret
	var dials []string
	o := runOpts{
		env:      map[string]string{"MY_SECRET": base64.StdEncoding.EncodeToString(testSecret)},
		files:    map[string]string{"viiwork.yaml": "api:\n  port: 9999\nmesh:\n  secret_env: MY_SECRET\n"},
		dialLog:  &dials,
		redirect: nodeAddr(node),
	}
	r := run(t, o, "set", "stable-coder", "Qwen3.8-27B", "--config", "viiwork.yaml", "--wait=false")
	if w := node.writes(); r.code != 0 || len(dials) == 0 || dials[0] != "127.0.0.1:9999" || len(w) != 1 || w[0].signer != "viiwork-cli@testhost" {
		t.Errorf("C18: exit %d, dials %v, writes %+v, stderr %q", r.code, dials, w, r.stderr)
	}
}

func TestCLIUsage(t *testing.T) {
	if r := run(t, runOpts{}, "set", "onlyname"); r.code != 2 || !strings.Contains(r.stderr, "usage") {
		t.Errorf("C19: exit %d, stderr %q", r.code, r.stderr)
	}
	r := run(t, runOpts{}, "frobnicate")
	if r.code != 2 {
		t.Errorf("C20: exit %d", r.code)
	}
	for _, cmd := range []string{"ls", "set", "rm", "history", "revert", "export", "import"} {
		if !strings.Contains(r.stderr, cmd) {
			t.Errorf("C20: usage does not list %s: %q", cmd, r.stderr)
		}
	}
	if r := run(t, runOpts{}); r.code != 2 {
		t.Errorf("no command: exit %d", r.code)
	}
}
