// Package enginetest is the conformance kit for viiwork engines: the checks
// every engine must pass whatever inference server it drives, so that adding
// one is a package and a blank import rather than a reading of every call site.
//
// An engine package calls Run from its own test with the Spec shapes it
// supports. What these assert is the contract in docs/adding-an-engine.md.
//
// Nothing here needs a GPU, an engine binary or a network: backends are
// httptest servers on loopback, and the only process started is none.
package enginetest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
	"gopkg.in/yaml.v3"
)

// Case is one Spec an engine should accept. Options is the engine's block as
// an operator would write it, so a case reads like the config file it stands
// for; it is parsed into Spec.Options.
type Case struct {
	Name    string
	Spec    engine.Spec
	Options string
}

// sentinel is appended to every case's Args. Engines must put the operator's
// args last, and a value no engine would generate makes that visible.
var sentinel = []string{"--enginetest-sentinel", "1"}

// Run asserts the contract. Call it from the engine package's own test:
//
//	func TestConformance(t *testing.T) { enginetest.Run(t, New(), cases...) }
func Run(t *testing.T, e engine.Engine, cases ...Case) {
	t.Helper()
	t.Run("Name", func(t *testing.T) { checkName(t, e) })
	t.Run("DefaultStartupTimeout", func(t *testing.T) { checkStartupTimeout(t, e) })
	t.Run("Probe", func(t *testing.T) { checkProbe(t, e) })
	for _, c := range cases {
		name := c.Name
		if name == "" {
			name = "case"
		}
		t.Run("Command/"+name, func(t *testing.T) { checkCommand(t, e, c) })
		t.Run("Load/"+name, func(t *testing.T) { checkLoad(t, e, specOf(t, c)) })
	}
	t.Run("Command/garbage options", func(t *testing.T) { checkGarbageOptions(t, e, cases) })
}

func checkName(t *testing.T, e engine.Engine) {
	t.Helper()
	name := e.Name()
	if strings.TrimSpace(name) == "" {
		t.Fatal("Name() is empty: it is how the engine is named in models[].engine and how its config block is named")
	}
	if name != strings.ToLower(name) || strings.ContainsAny(name, " \t") {
		t.Errorf("Name() = %q: an engine name is lowercase and has no whitespace", name)
	}
	got, ok := engine.Lookup(name)
	if !ok {
		t.Fatalf("Lookup(%q) found nothing: the engine must call engine.Register from init, or nothing can configure it", name)
	}
	if got.Name() != name {
		t.Errorf("Lookup(%q).Name() = %q: an engine must be registered under the name it reports", name, got.Name())
	}
}

func checkStartupTimeout(t *testing.T, e engine.Engine) {
	t.Helper()
	if d := e.DefaultStartupTimeout(); d <= 0 {
		t.Errorf("DefaultStartupTimeout() = %v, want positive: a zero would fail every backend before it loaded", d)
	}
}

func checkCommand(t *testing.T, e engine.Engine, c Case) {
	t.Helper()
	spec := specOf(t, c)
	spec.Args = append(slices.Clone(spec.Args), sentinel...)

	cmd, err := command(e, spec)
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if strings.TrimSpace(cmd.Path) == "" {
		t.Error("Command.Path is empty: there is nothing to execute")
	}
	if !slices.Contains(cmd.Args, spec.Name) {
		t.Errorf("Command.Args does not contain the model name %q: a backend that does not answer to it is unreachable through the mesh\nargs: %v", spec.Name, cmd.Args)
	}
	if !containsSubstring(cmd.Args, "127.0.0.1") {
		t.Errorf("Command.Args does not bind 127.0.0.1: a backend on a routable interface is an unauthenticated inference server on the tailnet\nargs: %v", cmd.Args)
	}
	if port := strconv.Itoa(spec.Port); !containsSubstring(cmd.Args, port) {
		t.Errorf("Command.Args does not contain the assigned port %s\nargs: %v", port, cmd.Args)
	}
	if n := len(cmd.Args); n < len(sentinel) || !slices.Equal(cmd.Args[n-len(sentinel):], sentinel) {
		t.Errorf("Spec.Args must be appended LAST so an operator's flag wins — every engine in the fleet takes the last occurrence of a repeated flag\nargs: %v", cmd.Args)
	}
}

// checkGarbageOptions gives the engine a block no engine defines. Returning an
// error is right and succeeding is acceptable (an engine may ignore options
// entirely); panicking is not, because it takes the node down rather than the
// backend.
func checkGarbageOptions(t *testing.T, e engine.Engine, cases []Case) {
	t.Helper()
	spec := engine.Spec{Name: "conformance", Path: "/models/conformance", GPUs: []int{0}, Port: 41999, Context: 4096, Parallel: 1, Backends: 1}
	if len(cases) > 0 {
		spec = specOf(t, cases[0])
	}
	var node yaml.Node
	if err := yaml.Unmarshal([]byte("no_engine_defines_this_key: 1\n"), &node); err != nil {
		t.Fatal(err)
	}
	spec.Options = *node.Content[0]
	if _, err := command(e, spec); err != nil {
		return // the expected outcome for an engine that decodes its block
	}
}

func checkProbe(t *testing.T, e engine.Engine) {
	t.Helper()
	// A closed port is a transport failure and must be reported as an error:
	// the supervisor cannot tell "loading" from "gone" without it.
	if _, err := e.Probe(timeout(t), closedAddr(t)); err == nil {
		t.Error("Probe against a closed port returned no error: a transport failure must be an error, not Ready: false")
	}
	// A server that answers is NOT a transport failure, whatever it says. An
	// engine may read this as ready or not ready — that is its own rule — but
	// it must not report it as an error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not the document you were looking for"))
	}))
	defer srv.Close()
	if _, err := e.Probe(timeout(t), addrOf(t, srv)); err != nil {
		t.Errorf("Probe against a server that answered returned an error (%v): errors are for transport failures only", err)
	}
}

func checkLoad(t *testing.T, e engine.Engine, spec engine.Spec) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	load, err := e.Load(timeout(t), spec, addrOf(t, srv))
	if err != nil {
		return // "cannot say" is a legitimate answer, and better than a guess
	}
	if load.Slots < 0 || load.Busy < 0 || load.Waiting < 0 || load.CtxPerSlot < 0 {
		t.Errorf("Load() = %+v: no field may be negative", load)
	}
	// Slots and CtxPerSlot are what the backend will actually serve. An engine
	// that cannot observe them from this server must fall back to the Spec
	// rather than report a zero, which the mesh would read as fact.
	if load.Slots == 0 && spec.Parallel > 0 {
		t.Errorf("Load().Slots = 0 with Spec.Parallel = %d: report what the engine says, or the Spec, never zero", spec.Parallel)
	}
	if load.CtxPerSlot == 0 && spec.Context > 0 {
		t.Errorf("Load().CtxPerSlot = 0 with Spec.Context = %d: same rule", spec.Context)
	}
}

// command calls Command and turns a panic into a test failure rather than a
// dead test binary.
func command(e engine.Engine, s engine.Spec) (cmd engine.Command, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &panicError{r}
		}
	}()
	return e.Command(s)
}

type panicError struct{ v any }

func (p *panicError) Error() string { return "Command panicked: it must return an error instead" }

func specOf(t *testing.T, c Case) engine.Spec {
	t.Helper()
	s := c.Spec
	if s.Name == "" {
		s.Name = "conformance-model"
	}
	if s.Port == 0 {
		s.Port = 41999
	}
	if s.Context == 0 {
		s.Context = 4096
	}
	if s.Parallel == 0 {
		s.Parallel = 1
	}
	if s.Backends == 0 {
		s.Backends = 1
	}
	if c.Options != "" {
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(c.Options), &node); err != nil {
			t.Fatalf("Case.Options is not valid YAML: %v", err)
		}
		s.Options = *node.Content[0]
	}
	return s
}

func containsSubstring(args []string, want string) bool {
	return slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, want) })
}

func addrOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// closedAddr is a port nothing listens on: taken and released, so a dial is
// refused rather than hanging.
func closedAddr(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := addrOf(t, srv)
	srv.Close()
	return addr
}

func timeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}
