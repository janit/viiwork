package node

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The node tests run the real llamacpp engine with this test binary as its
// llama-server: a model's env sets NODE_HELPER=llama-server, the supervisor
// passes it to the process, and TestMain serves a fake llama-server instead of
// running tests. (P2's fake engine is registered under the name "fake", which
// config validation refuses, and Reload loads a validated config.)
//
// The fake honours these variables from the model's env:
//
//	FAKE_READY_AFTER=<duration>  /health answers 503 until then (default 0)
//	FAKE_STREAM_HOLD=<duration>  a streaming completion waits this long
//	                             between its first chunk and the rest
//	FAKE_HOLD=<duration>         a non-streaming completion waits this long

func TestMain(m *testing.M) {
	if os.Getenv("NODE_HELPER") == "llama-server" {
		serveFakeLlamaServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// argValue is the value after flag in a llama-server command line.
func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func serveFakeLlamaServer() {
	args := os.Args[1:]
	ln, err := net.Listen("tcp", "127.0.0.1:"+argValue(args, "--port"))
	if err != nil {
		fmt.Println("listen:", err)
		os.Exit(1)
	}
	parallel, _ := strconv.Atoi(argValue(args, "--parallel"))
	parallel = max(parallel, 1)
	readyAfter, _ := time.ParseDuration(os.Getenv("FAKE_READY_AFTER"))
	streamHold, _ := time.ParseDuration(os.Getenv("FAKE_STREAM_HOLD"))
	hold, _ := time.ParseDuration(os.Getenv("FAKE_HOLD"))
	start := time.Now()
	var busy atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		if time.Since(start) < readyAfter {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/slots", func(w http.ResponseWriter, _ *http.Request) {
		slots := make([]map[string]any, parallel)
		n := int(busy.Load())
		for i := range slots {
			slots[i] = map[string]any{"id": i, "n_ctx": 512, "is_processing": i < n}
		}
		_ = json.NewEncoder(w).Encode(slots)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		busy.Add(1)
		defer busy.Add(-1)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(streamHold)
			_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":3,\"total_tokens\":4}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		time.Sleep(hold)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":3,"total_tokens":4}}`)
	})
	go func() { _ = http.Serve(ln, mux) }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	select {
	case <-sig:
	case <-time.After(10 * time.Minute): // a stray helper never outlives a test run
	}
}

// processGone is true when pid no longer exists or is a zombie. A zombie
// counts as gone: inside the gorun container PID 1 is the go command, which
// never reaps orphaned grandchildren.
func processGone(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return true
	}
	s := string(data)
	closeParen := strings.LastIndexByte(s, ')')
	if closeParen < 0 {
		return true
	}
	fields := strings.Fields(s[closeParen+1:])
	return len(fields) > 0 && fields[0] == "Z"
}

// fakeModel is a models[] entry served by the fake llama-server.
type fakeModel struct {
	name       string
	args       []string
	parallel   int // 0 = 2
	readyAfter string
	streamHold string
	hold       string
}

func (m fakeModel) yaml() string {
	var b strings.Builder
	parallel := m.parallel
	if parallel == 0 {
		parallel = 2
	}
	fmt.Fprintf(&b, "  - name: %s\n    engine: llamacpp\n    path: /models/%s.gguf\n    context: 512\n    parallel: %d\n", m.name, m.name, parallel)
	fmt.Fprintf(&b, "    llamacpp:\n      binary: %q\n      threads: 1\n", os.Args[0])
	if len(m.args) > 0 {
		b.WriteString("    args:\n")
		for _, a := range m.args {
			fmt.Fprintf(&b, "      - %q\n", a)
		}
	}
	b.WriteString("    env:\n      NODE_HELPER: llama-server\n")
	if m.readyAfter != "" {
		fmt.Fprintf(&b, "      FAKE_READY_AFTER: %s\n", m.readyAfter)
	}
	if m.streamHold != "" {
		fmt.Fprintf(&b, "      FAKE_STREAM_HOLD: %s\n", m.streamHold)
	}
	if m.hold != "" {
		fmt.Fprintf(&b, "      FAKE_HOLD: %s\n", m.hold)
	}
	return b.String()
}

// nodeConfig is a v2 config for one test node: open mesh, no GPUs, no power
// probing, fast health checks, and the given models. extra is appended as
// top-level YAML.
func nodeConfig(name, stateDir string, models []fakeModel, extra string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "node:\n  name: %s\n  state_dir: %q\n", name, stateDir)
	b.WriteString("api:\n  host: 127.0.0.1\n  port: 18086\n")
	b.WriteString("mesh:\n  open: true\n  network: tailnet\n  capacity_poll: 250ms\n  tailnet:\n    enabled: false\n")
	b.WriteString("gpu:\n  vendor: none\n")
	b.WriteString("power:\n  source: none\n")
	b.WriteString("health:\n  interval: 200ms\n  timeout: 1s\n  respawn_grace: 2s\n")
	b.WriteString("routing:\n  queue_timeout: 2s\n")
	if len(models) > 0 {
		b.WriteString("models:\n")
		for _, m := range models {
			b.WriteString(m.yaml())
		}
	}
	b.WriteString(extra)
	return b.String()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
