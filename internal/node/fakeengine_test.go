package node

import (
	"bytes"
	"encoding/binary"
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
	"sync"
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
//	FAKE_REQUIRE_MODEL=<path>    exit 1 unless --model is exactly this path
//	FAKE_PREFILL_PER_1K=<dur>    a streaming completion sleeps this long per
//	                             1000 FAKE_PROMPT_TOKENS before its first chunk
//	FAKE_PROMPT_TOKENS=<n>       prompt tokens reported in a streamed
//	                             completion's usage, with zero cached tokens
//	FAKE_PROMPT_COUNTER=<path>   every completion adds 100 prompt tokens per
//	                             completion before it, counted in this file
//	                             (shared by the fakes of one test): a
//	                             conversation grows turn by turn
//	FAKE_LOG=<path>              append "chat slot=<id_slot> sess=<session>",
//	                             "save <slot> <tokens>" and "restore <slot>
//	                             <tokens>" lines (kv_share)
//
// With --slot-save-path it serves POST /slots/N?action=save|restore: a save
// writes a small but well-formed slot file (llamacpp's state file format) of
// the slot's last prompt, a restore reads one back into the slot.

func TestMain(m *testing.M) {
	switch os.Getenv("NODE_HELPER") {
	case "llama-server":
		serveFakeLlamaServer()
		os.Exit(0)
	case "vllm":
		serveFakeVLLM()
		os.Exit(0)
	case "ft":
		serveFakeFreeToken()
		os.Exit(0)
	case "strata":
		serveFakeStrata()
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

// fakeChatHook is set by serveFakeLlamaServer once its kv_share log exists.
var fakeChatHook atomic.Pointer[func(*http.Request, []byte)]

// fakeSlotFile is a slot save file of n tokens as llama.cpp writes one
// (state format 4): one K/V cache of one layer, rows depending only on the
// token's position, so a longer conversation's file shares its prefix, and
// one context checkpoint.
func fakeSlotFile(n int) []byte {
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("qsgg")
	w(uint32(4))
	w(uint32(n))
	for i := 0; i < n; i++ {
		w(int32(1000 + i))
	}
	w(uint32(1)) // n_stream
	w(uint32(n)) // cells
	for i := 0; i < n; i++ {
		w(int32(i))
		w(uint32(1))
		w(int32(0))
	}
	w(uint32(0))   // v_trans
	w(uint32(1))   // n_layer
	w(uint32(128)) // n_rot_k
	w(uint32(128)) // n_rot_v
	const row = 256
	for _, kv := range []byte{'k', 'v'} {
		w(int32(1))
		w(uint64(row))
		for i := 0; i < n; i++ {
			b.Write(bytes.Repeat([]byte{kv ^ byte(i)}, row))
		}
	}
	w(uint32(0x504b4353)) // SCKP
	w(uint32(1))
	w(uint32(1))
	w(int64(max(n-10, 0)))
	w(int32(max(n-11, 0)))
	w(int32(max(n-11, 0)))
	w(uint64(4096))
	b.Write(bytes.Repeat([]byte{0x5a}, 4096))
	w(uint64(0))
	w(uint64(0))
	return b.Bytes()
}

func serveFakeLlamaServer() {
	args := os.Args[1:]
	if want := os.Getenv("FAKE_REQUIRE_MODEL"); want != "" && argValue(args, "--model") != want {
		fmt.Printf("fake llama-server: --model %q, want %q\n", argValue(args, "--model"), want)
		os.Exit(1)
	}
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
	prefillPer1k, _ := time.ParseDuration(os.Getenv("FAKE_PREFILL_PER_1K"))
	promptTokens, _ := strconv.Atoi(os.Getenv("FAKE_PROMPT_TOKENS"))
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
		promptTokens := promptTokens
		if p := os.Getenv("FAKE_PROMPT_COUNTER"); p != "" {
			if st, err := os.Stat(p); err == nil {
				promptTokens += 100 * int(st.Size())
			}
			if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				_, _ = f.Write([]byte{'.'})
				_ = f.Close()
			}
		}
		r.Header.Set("X-Fake-Prompt-Tokens", strconv.Itoa(promptTokens)) // for the chat hook
		body, _ := io.ReadAll(r.Body)
		if h := fakeChatHook.Load(); h != nil {
			(*h)(r, body)
		}
		if strings.Contains(string(body), `"stream":true`) {
			time.Sleep(time.Duration(float64(prefillPer1k) * float64(promptTokens) / 1000))
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(streamHold)
			usage := `{"prompt_tokens":1,"completion_tokens":3,"total_tokens":4}`
			if promptTokens > 0 {
				usage = fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":3,"total_tokens":%d,"prompt_tokens_details":{"cached_tokens":0}}`, promptTokens, promptTokens+3)
			}
			_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":"+usage+"}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		time.Sleep(hold)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":3,"total_tokens":4}}`)
	})
	saveDir := argValue(args, "--slot-save-path")
	logPath := os.Getenv("FAKE_LOG")
	var logMu sync.Mutex
	logLine := func(format string, a ...any) {
		if logPath == "" {
			return
		}
		logMu.Lock()
		defer logMu.Unlock()
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintf(f, format+"\n", a...)
	}
	var slotMu sync.Mutex
	slotTokens := map[int]int{}
	if saveDir != "" {
		mux.HandleFunc("POST /slots/{id}", func(w http.ResponseWriter, r *http.Request) {
			id, _ := strconv.Atoi(r.PathValue("id"))
			var req struct{ Filename string }
			_ = json.NewDecoder(r.Body).Decode(&req)
			path := filepath.Join(saveDir, filepath.Base(req.Filename))
			switch r.URL.Query().Get("action") {
			case "save":
				slotMu.Lock()
				n := slotTokens[id]
				slotMu.Unlock()
				if err := os.WriteFile(path, fakeSlotFile(n), 0o600); err != nil {
					http.Error(w, `{"error":{"message":"write"}}`, 500)
					return
				}
				logLine("save %d %d", id, n)
			case "restore":
				b, err := os.ReadFile(path)
				if err != nil || len(b) < 12 || string(b[:4]) != "qsgg" {
					w.WriteHeader(400)
					_, _ = io.WriteString(w, `{"error":{"message":"invalid slot save file"}}`)
					return
				}
				n := int(binary.LittleEndian.Uint32(b[8:]))
				slotMu.Lock()
				slotTokens[id] = n
				slotMu.Unlock()
				logLine("restore %d %d", id, n)
			default:
				w.WriteHeader(400)
				return
			}
			_, _ = io.WriteString(w, `{}`)
		})
	}
	logChat := func(r *http.Request, body []byte) {
		var b struct {
			IDSlot *int `json:"id_slot"`
		}
		_ = json.Unmarshal(body, &b)
		slot := "none"
		if b.IDSlot != nil {
			slot = strconv.Itoa(*b.IDSlot)
			slotMu.Lock()
			n, _ := strconv.Atoi(r.Header.Get("X-Fake-Prompt-Tokens"))
			slotTokens[*b.IDSlot] = max(n, 1)
			slotMu.Unlock()
		}
		logLine("chat slot=%s sess=%s", slot, r.Header.Get("X-Session-Affinity"))
	}
	fakeChatHook.Store(&logChat)
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
	name         string
	args         []string
	parallel     int // 0 = 2
	readyAfter   string
	streamHold   string
	hold         string
	prefillPer1k string
	promptTokens string
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
	if m.prefillPer1k != "" {
		fmt.Fprintf(&b, "      FAKE_PREFILL_PER_1K: %s\n", m.prefillPer1k)
	}
	if m.promptTokens != "" {
		fmt.Fprintf(&b, "      FAKE_PROMPT_TOKENS: %s\n", m.promptTokens)
	}
	return b.String()
}

// nodeConfig is a v2 config for one test node: open mesh, no GPUs, no power
// probing, fast health checks, and the given models. extra is appended as
// top-level YAML.
func nodeConfig(name, stateDir string, models []fakeModel, extra string) string {
	rendered := make([]string, len(models))
	for i, m := range models {
		rendered[i] = m.yaml()
	}
	return nodeConfigYAML(name, stateDir, rendered, extra)
}

// nodeConfigYAML is nodeConfig over already-rendered models[] entries, so a
// test can mix engines in one config.
func nodeConfigYAML(name, stateDir string, models []string, extra string) string {
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
			b.WriteString(m)
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
