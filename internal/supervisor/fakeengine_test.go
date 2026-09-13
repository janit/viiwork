package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
)

// The fake engine runs the test binary in helper mode "serve": an HTTP server
// on the backend's port that answers /health and /load the way the tests ask.
// Its behaviour per model comes from key=value entries in the model's args,
// so a config change (Task 10) changes the behaviour too:
//
//	ready_after=<duration>  /health answers 503 until then (default 0)
//	exit_after=<duration>   the helper exits with status 1 (default never)
//	slots=<n>               /load slots (default 2)
//	first_mode=<mode>       helper mode for this model's first Command only
//	mode=<mode>             helper mode for every Command (default serve)
//	command_error           Command fails
//	path=<path>             Command returns this binary instead of the test binary
//	print_env=<A,B>         the helper prints these variables at start
//	term=ignore             the helper ignores SIGTERM

type fakeCall struct {
	Spec engine.Spec
	Port int
	At   time.Time
}

var fakeLog struct {
	mu    sync.Mutex
	calls []fakeCall
}

// fakeCallsFor returns the recorded Command calls for one model name.
func fakeCallsFor(model string) []fakeCall {
	fakeLog.mu.Lock()
	defer fakeLog.mu.Unlock()
	var out []fakeCall
	for _, c := range fakeLog.calls {
		if c.Spec.Name == model {
			out = append(out, c)
		}
	}
	return out
}

// resetFakeCalls forgets the recorded calls for one model name.
func resetFakeCalls(model string) {
	fakeLog.mu.Lock()
	defer fakeLog.mu.Unlock()
	fakeLog.calls = slices.DeleteFunc(fakeLog.calls, func(c fakeCall) bool { return c.Spec.Name == model })
}

func fakeArgs(args []string) map[string]string {
	kv := map[string]string{}
	for _, a := range args {
		k, v, _ := strings.Cut(a, "=")
		kv[k] = v
	}
	return kv
}

type fakeEngine struct {
	name   string
	client *http.Client
}

func (f *fakeEngine) Name() string                         { return f.name }
func (f *fakeEngine) DefaultStartupTimeout() time.Duration { return 5 * time.Second }

func (f *fakeEngine) Command(s engine.Spec) (engine.Command, error) {
	fakeLog.mu.Lock()
	first := true
	for _, c := range fakeLog.calls {
		if c.Spec.Name == s.Name {
			first = false
			break
		}
	}
	fakeLog.calls = append(fakeLog.calls, fakeCall{Spec: s, Port: s.Port, At: time.Now()})
	fakeLog.mu.Unlock()

	kv := fakeArgs(s.Args)
	if _, ok := kv["command_error"]; ok {
		return engine.Command{}, errors.New("fake: command_error requested")
	}
	mode := "serve"
	if m, ok := kv["mode"]; ok {
		mode = m
	}
	if m, ok := kv["first_mode"]; ok && first {
		mode = m
	}
	path := os.Args[0]
	if p, ok := kv["path"]; ok {
		path = p
	}
	env := []string{"SUPERVISOR_HELPER=" + mode, "FAKE_PORT=" + strconv.Itoa(s.Port)}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		env = append(env, "FAKE_"+strings.ToUpper(k)+"="+kv[k])
	}
	return engine.Command{Path: path, Env: env}, nil
}

func (f *fakeEngine) get(ctx context.Context, addr, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return nil, err
	}
	return f.client.Do(req)
}

func (f *fakeEngine) Probe(ctx context.Context, addr string) (engine.Probe, error) {
	resp, err := f.get(ctx, addr, "/health")
	if err != nil {
		return engine.Probe{}, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return engine.Probe{Ready: true, Progress: -1}, nil
	case http.StatusServiceUnavailable:
		return engine.Probe{Phase: "loading", Progress: -1}, nil
	default:
		return engine.Probe{Progress: -1, Reason: fmt.Sprintf("health returned HTTP %d", resp.StatusCode)}, nil
	}
}

type fakeLoadBody struct {
	Slots   int   `json:"slots"`
	Busy    int   `json:"busy"`
	Decoded int64 `json:"decoded"`
	Remain  int64 `json:"remain"`
}

func (f *fakeEngine) readLoad(ctx context.Context, addr string) (fakeLoadBody, error) {
	resp, err := f.get(ctx, addr, "/load")
	if err != nil {
		return fakeLoadBody{}, err
	}
	defer resp.Body.Close()
	var body fakeLoadBody
	if resp.StatusCode != http.StatusOK {
		return body, fmt.Errorf("load returned HTTP %d", resp.StatusCode)
	}
	err = json.NewDecoder(resp.Body).Decode(&body)
	return body, err
}

func (f *fakeEngine) Load(ctx context.Context, _ engine.Spec, addr string) (engine.Load, error) {
	b, err := f.readLoad(ctx, addr)
	return engine.Load{Slots: b.Slots, Busy: b.Busy}, err
}

// fakeProgressEngine also reports token progress (engine.TokenProgressReader).
type fakeProgressEngine struct{ fakeEngine }

func (f *fakeProgressEngine) LoadProgress(ctx context.Context, _ engine.Spec, addr string) (engine.Load, int64, int64, error) {
	b, err := f.readLoad(ctx, addr)
	return engine.Load{Slots: b.Slots, Busy: b.Busy}, b.Decoded, b.Remain, err
}

var (
	_ engine.Engine              = (*fakeEngine)(nil)
	_ engine.TokenProgressReader = (*fakeProgressEngine)(nil)
)

func init() {
	engine.Register(&fakeEngine{name: "fake", client: &http.Client{}})
	engine.Register(&fakeProgressEngine{fakeEngine{name: "fake-progress", client: &http.Client{}}})
	helperModes["serve"] = serveFake
	helperModes["exit1"] = func() { os.Exit(1) }
}

// fakeServeRoutes lets other test files add routes to the serve helper.
var fakeServeRoutes = map[string]http.HandlerFunc{}

func serveFake() {
	ln, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("FAKE_PORT"))
	if err != nil {
		fmt.Println("listen:", err)
		os.Exit(1)
	}
	for _, name := range strings.Split(os.Getenv("FAKE_PRINT_ENV"), ",") {
		if name != "" {
			fmt.Printf("%s=%s\n", name, os.Getenv(name))
		}
	}
	readyAfter, _ := time.ParseDuration(os.Getenv("FAKE_READY_AFTER"))
	slots := 2
	if n, err := strconv.Atoi(os.Getenv("FAKE_SLOTS")); err == nil {
		slots = n
	}
	start := time.Now()
	var failed atomic.Bool
	var busy atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		switch {
		case failed.Load():
			w.WriteHeader(http.StatusInternalServerError)
		case time.Since(start) < readyAfter:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	mux.HandleFunc("POST /fail", func(w http.ResponseWriter, _ *http.Request) { failed.Store(true) })
	mux.HandleFunc("POST /busy", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.ParseInt(r.URL.Query().Get("n"), 10, 64)
		busy.Store(n)
	})
	mux.HandleFunc("/load", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(fakeLoadBody{Slots: slots, Busy: int(busy.Load()), Decoded: 120, Remain: 380})
	})
	for pattern, h := range fakeServeRoutes {
		mux.HandleFunc(pattern, h)
	}
	go func() { _ = http.Serve(ln, mux) }()

	sig := make(chan os.Signal, 1)
	if os.Getenv("FAKE_TERM") == "ignore" {
		signal.Ignore(syscall.SIGTERM)
	} else {
		signal.Notify(sig, syscall.SIGTERM)
	}
	var exit <-chan time.Time
	if d, err := time.ParseDuration(os.Getenv("FAKE_EXIT_AFTER")); err == nil {
		exit = time.After(d)
	}
	select {
	case <-sig:
	case <-exit:
		os.Exit(1)
	case <-time.After(10 * time.Minute): // a stray helper never outlives a test run
	}
}

// postFake sends a control request (/fail, /busy?n=) to a running fake backend.
func postFake(addr, pathAndQuery string) error {
	resp, err := http.Post("http://"+addr+pathAndQuery, "text/plain", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
