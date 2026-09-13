package node

// Fake vLLM and FreeToken servers, run as this test binary through the model's
// env (NODE_HELPER=vllm or ft), exactly as the llama-server fake is. The real
// engines build the command line and run the real probes against these, so the
// node tests exercise both without a GPU.
//
// A fake asserts the flags its engine is specified to generate. It cannot call
// t.Errorf from inside a child process, so a wrong flag exits non-zero: the
// backend never starts, and the test that expected it healthy fails with the
// reason on stderr.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// wantFlag exits non-zero unless args carries flag with exactly value want.
func wantFlag(args []string, flag, want string) {
	if got := argValue(args, flag); got != want {
		fmt.Fprintf(os.Stderr, "fake engine: %s = %q, want %q\nargs: %v\n", flag, got, want, args)
		os.Exit(1)
	}
}

// wantSubcommand exits non-zero unless args[0] is the expected subcommand.
func wantSubcommand(args []string, want string) {
	if len(args) == 0 || args[0] != want {
		fmt.Fprintf(os.Stderr, "fake engine: first argument %v, want %q\n", args, want)
		os.Exit(1)
	}
}

// listenOn binds the port the engine was told to use. Binding 127.0.0.1 is
// itself part of the contract: a backend on a routable interface would be an
// unauthenticated inference server on the tailnet.
func listenOn(port string) net.Listener {
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake engine listen:", err)
		os.Exit(1)
	}
	return ln
}

// serveUntilTerm runs mux on ln until SIGTERM, so the supervisor's ordinary
// stop path ends the fake.
func serveUntilTerm(ln net.Listener, mux *http.ServeMux) {
	go func() { _ = http.Serve(ln, mux) }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	select {
	case <-sig:
	case <-time.After(10 * time.Minute): // a stray helper never outlives a test run
	}
}

// serveFakeVLLM asserts the flags of Task 2's command line and then serves
// /health and /metrics.
//
// Honoured from the model's env:
//
//	FAKE_READY_AFTER=<duration>  /health answers 503 until then (default 0)
//	FAKE_BUSY=<n>                num_requests_running (default 0)
//	FAKE_WAITING=<n>             num_requests_waiting (default 0)
//	FAKE_NO_RUNNING_GAUGE=1      omit the running gauge entirely
func serveFakeVLLM() {
	args := os.Args[1:]
	wantSubcommand(args, "serve")
	wantFlag(args, "--host", "127.0.0.1")
	// Without --served-model-name vLLM advertises the model by its path, and
	// no client could ask for it by the name the fleet uses.
	wantFlag(args, "--served-model-name", os.Getenv("FAKE_WANT_NAME"))
	wantFlag(args, "--max-model-len", os.Getenv("FAKE_WANT_CTX"))
	wantFlag(args, "--max-num-seqs", os.Getenv("FAKE_WANT_PARALLEL"))
	wantFlag(args, "--tensor-parallel-size", os.Getenv("FAKE_WANT_TP"))

	ln := listenOn(argValue(args, "--port"))
	readyAfter, _ := time.ParseDuration(os.Getenv("FAKE_READY_AFTER"))
	busy, _ := strconv.Atoi(os.Getenv("FAKE_BUSY"))
	waiting, _ := strconv.Atoi(os.Getenv("FAKE_WAITING"))
	noRunning := os.Getenv("FAKE_NO_RUNNING_GAUGE") == "1"
	start := time.Now()
	inFlight := new(atomic.Int64)

	mux := http.NewServeMux()
	// vLLM answers /health only once it is ready; it does not answer 200 while
	// loading the way FreeToken does.
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		if time.Since(start) < readyAfter {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		var b strings.Builder
		b.WriteString("# HELP vllm:num_requests_waiting Number of requests waiting.\n")
		b.WriteString("# TYPE vllm:num_requests_waiting gauge\n")
		fmt.Fprintf(&b, "vllm:num_requests_waiting{model_name=\"m\"} %d.0\n", waiting)
		if !noRunning {
			b.WriteString("# HELP vllm:num_requests_running Number of requests running.\n")
			b.WriteString("# TYPE vllm:num_requests_running gauge\n")
			fmt.Fprintf(&b, "vllm:num_requests_running{model_name=\"m\"} %d.0\n",
				int64(busy)+inFlight.Load())
		}
		b.WriteString("vllm:kv_cache_usage_perc{model_name=\"m\"} 0.25\n")
		_, _ = io.WriteString(w, b.String())
	})
	mux.HandleFunc("POST /v1/chat/completions", completionsHandler(inFlight))
	serveUntilTerm(ln, mux)
}

// serveFakeFreeToken asserts the flags of Task 3's command line and then
// serves /health and /v1/stats.
//
// Honoured from the model's env:
//
//	FAKE_STATUS=<s>       /health status: ok (default), loading, error
//	FAKE_PHASE=<s>        phase reported while loading
//	FAKE_MAINTENANCE=<s>  maintenance field; "serving" still accepts work
//	FAKE_BUSY=<n>         requests.active (default 0)
//	FAKE_GPU_UUID=<s>     the UUID BoundGPU reports; unset means the engine
//	                      says nothing, which is unknown and not a mismatch
func serveFakeFreeToken() {
	args := os.Args[1:]
	wantSubcommand(args, "serve")
	wantFlag(args, "--host", "127.0.0.1")
	wantFlag(args, "--served-model-name", os.Getenv("FAKE_WANT_NAME"))
	wantFlag(args, "--max-seq-len-override", os.Getenv("FAKE_WANT_CTX"))
	wantFlag(args, "--max-running-requests", os.Getenv("FAKE_WANT_PARALLEL"))
	// The old spelling stays because 0.1.2 knows no other.
	wantFlag(args, "--moe-backend", os.Getenv("FAKE_WANT_MOE"))

	ln := listenOn(argValue(args, "--port"))
	status := os.Getenv("FAKE_STATUS")
	if status == "" {
		status = "ok"
	}
	busy, _ := strconv.Atoi(os.Getenv("FAKE_BUSY"))
	inFlight := new(atomic.Int64)

	mux := http.NewServeMux()
	// FreeToken answers 200 in EVERY lifecycle state, so the body is the
	// readiness rule, not the status code.
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		doc := map[string]any{"status": status}
		if p := os.Getenv("FAKE_PHASE"); p != "" {
			doc["phase"] = p
			doc["progress"] = map[string]any{"done_bytes": 3, "total_bytes": 4}
		}
		if mt := os.Getenv("FAKE_MAINTENANCE"); mt != "" {
			doc["maintenance"] = mt
		}
		if u := os.Getenv("FAKE_GPU_UUID"); u != "" {
			doc["gpus"] = []map[string]any{{"uuid": u}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/v1/stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"requests": map[string]any{"active": int64(busy) + inFlight.Load()},
			"kv":       map[string]any{"used_pages": 1, "total_pages": 4},
		})
	})
	mux.HandleFunc("POST /v1/chat/completions", completionsHandler(inFlight))
	serveUntilTerm(ln, mux)
}

// completionsHandler answers an OpenAI completion and counts itself in
// flight, so a load poll taken during a request sees a busy backend.
func completionsHandler(inFlight *atomic.Int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inFlight.Add(1)
		defer inFlight.Add(-1)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":3,\"total_tokens\":4}}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":3,"total_tokens":4}}`)
	}
}

// gpuModel is a models[] entry served by one of the two GPU-only fakes. Both
// engines refuse a backend with no cards, so gpus is never empty.
type gpuModel struct {
	name    string
	engine  string // "vllm" or "freetoken"
	helper  string // NODE_HELPER value: "vllm" or "ft"
	gpus    []int
	perBack int // gpus_per_backend, 0 = len(gpus)
	ctx     int // 0 = 8192
	para    int // 0 = 2
	env     map[string]string
}

func (m gpuModel) yaml() string {
	ctx, para := m.ctx, m.para
	if ctx == 0 {
		ctx = 8192
	}
	if para == 0 {
		para = 2
	}
	perBack := m.perBack
	if perBack == 0 {
		perBack = len(m.gpus)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "  - name: %s\n    engine: %s\n    path: /models/%s\n", m.name, m.engine, m.name)
	fmt.Fprintf(&b, "    gpus: [%s]\n    gpus_per_backend: %d\n", joinInts(m.gpus), perBack)
	fmt.Fprintf(&b, "    context: %d\n    parallel: %d\n", ctx, para)
	// Every backend must load well inside the test's patience, and vLLM's own
	// default is 20 minutes.
	b.WriteString("    startup_timeout: 30s\n")
	fmt.Fprintf(&b, "    %s:\n      binary: %q\n", m.engine, os.Args[0])

	b.WriteString("    env:\n")
	fmt.Fprintf(&b, "      NODE_HELPER: %s\n", m.helper)
	// What the fake asserts the engine generated.
	fmt.Fprintf(&b, "      FAKE_WANT_NAME: %q\n", m.name)
	fmt.Fprintf(&b, "      FAKE_WANT_CTX: %q\n", strconv.Itoa(ctx))
	fmt.Fprintf(&b, "      FAKE_WANT_PARALLEL: %q\n", strconv.Itoa(para))
	if m.engine == "vllm" {
		fmt.Fprintf(&b, "      FAKE_WANT_TP: %q\n", strconv.Itoa(perBack))
	} else {
		b.WriteString("      FAKE_WANT_MOE: \"auto\"\n")
	}
	for k, v := range m.env {
		fmt.Fprintf(&b, "      %s: %q\n", k, v)
	}
	return b.String()
}

func joinInts(ns []int) string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = strconv.Itoa(n)
	}
	return strings.Join(out, ", ")
}
