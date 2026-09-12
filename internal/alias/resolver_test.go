package alias

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/proxy"
	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

type fakeLocal struct {
	mu     sync.Mutex
	models []meshapi.ModelCapacity
}

func (f *fakeLocal) Capacity() []meshapi.ModelCapacity {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]meshapi.ModelCapacity(nil), f.models...)
}

func (f *fakeLocal) setHealthy(name string, healthy int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.models {
		if f.models[i].Name == name {
			f.models[i].HealthyBackends = healthy
		}
	}
}

type fakeReports []capacity.Report

func (f fakeReports) Reports() []capacity.Report { return f }

func model(name string, healthy int) meshapi.ModelCapacity {
	return meshapi.ModelCapacity{Name: name, Engine: "llamacpp", Slots: 1, Backends: 1, HealthyBackends: healthy}
}

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *logLines) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, sub) {
			n++
		}
	}
	return n
}

type resolverFx struct {
	store    *Store
	local    *fakeLocal
	resolver *Resolver
	logs     *logLines
	clock    *testClock
}

// newResolverFx is the Task 3 fixture: local Qwen3.8-27B (healthy) and granite
// (not healthy); peer P's fresh report serves gemma-4-31B-it; peer Q's stale
// report lists old-model; pipelines translate and translate-fi.
func newResolverFx(t *testing.T) *resolverFx {
	t.Helper()
	clk := newClock()
	now := clk.now()
	fx := &resolverFx{
		store: openTest(t, t.TempDir(), clk),
		local: &fakeLocal{models: []meshapi.ModelCapacity{model("Qwen3.8-27B", 1), model("granite", 0)}},
		logs:  &logLines{},
		clock: clk,
	}
	reports := fakeReports{
		{Node: "P", Received: now, Models: []meshapi.ModelCapacity{model("gemma-4-31B-it", 1)}},
		{Node: "Q", Received: now.Add(-time.Minute), Models: []meshapi.ModelCapacity{model("old-model", 1)}},
		{Node: "gb1", Received: now, Models: []meshapi.ModelCapacity{model("self-only", 1)}},
	}
	served := NewServedView("gb1", fx.local, reports, 3*time.Second, clk.now)
	fx.resolver = NewResolver(fx.store, served, func() []string { return []string{"translate", "translate-fi"} }, fx.logs.logf)
	return fx
}

func (fx *resolverFx) aliases(t *testing.T) {
	t.Helper()
	mustSet(t, fx.store, "stable-coder", "Qwen3.8-27B", "gemma-4-31B-it")
	mustSet(t, fx.store, "stable-granite", "granite", "old-model", "gemma-4-31B-it")
}

func TestServedView(t *testing.T) {
	fx := newResolverFx(t)
	served := NewServedView("gb1", fx.local, fakeReports{
		{Node: "P", Received: fx.clock.now(), Models: []meshapi.ModelCapacity{model("gemma-4-31B-it", 1)}},
		{Node: "Q", Received: fx.clock.now().Add(-time.Minute), Models: []meshapi.ModelCapacity{model("old-model", 1)}},
		{Node: "gb1", Received: fx.clock.now(), Models: []meshapi.ModelCapacity{model("self-only", 1)}},
	}, 3*time.Second, fx.clock.now)
	cases := []struct {
		model          string
		exists, served bool
	}{
		{"Qwen3.8-27B", true, true},
		{"granite", true, false},
		{"gemma-4-31B-it", true, true},
		{"old-model", true, false},
		{"nosuch", false, false},
		{"self-only", false, false}, // a report under this node's own name is not a peer's
	}
	for _, c := range cases {
		if got := served.Exists(c.model); got != c.exists {
			t.Errorf("Exists(%s) = %v", c.model, got)
		}
		if got := served.Served(c.model); got != c.served {
			t.Errorf("Served(%s) = %v", c.model, got)
		}
	}
}

func TestResolve(t *testing.T) {
	type want struct{ model, alias string }
	check := func(t *testing.T, r *Resolver, requested string, w want) {
		t.Helper()
		m, a, err := r.Resolve(requested)
		if err != nil || m != w.model || a != w.alias {
			t.Errorf("Resolve(%s) = (%q, %q, %v), want (%q, %q)", requested, m, a, err, w.model, w.alias)
		}
	}

	fx := newResolverFx(t)
	fx.aliases(t)
	check(t, fx.resolver, "Qwen3.8-27B", want{"Qwen3.8-27B", ""})                     // V1
	check(t, fx.resolver, "translate-fi", want{"translate-fi", ""})                   // V2
	check(t, fx.resolver, "stable-coder", want{"Qwen3.8-27B", "stable-coder"})        // V3
	check(t, fx.resolver, "stable-granite", want{"gemma-4-31B-it", "stable-granite"}) // V5
	check(t, fx.resolver, "nosuch", want{"nosuch", ""})                               // V7

	t.Run("V4 target unhealthy", func(t *testing.T) {
		fx := newResolverFx(t)
		fx.aliases(t)
		fx.local.setHealthy("Qwen3.8-27B", 0)
		check(t, fx.resolver, "stable-coder", want{"gemma-4-31B-it", "stable-coder"})
	})

	t.Run("V6 nothing served", func(t *testing.T) {
		fx := newResolverFx(t)
		mustSet(t, fx.store, "dead", "nosuch", "old-model")
		_, _, err := fx.resolver.Resolve("dead")
		var re *proxy.ResolveError
		if !errors.As(err, &re) || re.Status != 503 || re.Type != meshapi.ErrTypeUnavailable || re.RetryAfter != 5 ||
			re.Message != "alias dead: no node serves nosuch or its fallbacks" {
			t.Errorf("err = %#v", err)
		}
	})

	t.Run("V8 shadowed", func(t *testing.T) {
		fx := newResolverFx(t)
		mustSet(t, fx.store, "granite", "Qwen3.8-27B")
		for i := 0; i < 3; i++ {
			check(t, fx.resolver, "granite", want{"granite", ""})
		}
		if n := fx.logs.count("alias granite is shadowed by a real model or pipeline of the same name"); n != 1 {
			t.Errorf("shadow log lines = %d, want 1", n)
		}
	})

	t.Run("V9 a full target does not fall back", func(t *testing.T) {
		fx := newResolverFx(t)
		fx.aliases(t)
		fx.local.mu.Lock()
		fx.local.models[0].Busy = 1
		fx.local.mu.Unlock()
		check(t, fx.resolver, "stable-coder", want{"Qwen3.8-27B", "stable-coder"})
	})
}

func TestResolverInfo(t *testing.T) {
	fx := newResolverFx(t)
	fx.aliases(t)
	mustSet(t, fx.store, "dead", "nosuch")
	mustSet(t, fx.store, "granite", "Qwen3.8-27B")
	mustSet(t, fx.store, "gone", "Qwen3.8-27B")
	if _, err := fx.store.Delete("gone"); err != nil {
		t.Fatal(err)
	}
	infos := fx.resolver.Info()
	byName := map[string]meshapi.AliasInfo{}
	var names []string
	for _, in := range infos {
		byName[in.Name] = in
		names = append(names, in.Name)
	}
	if strings.Join(names, ",") != "dead,granite,stable-coder,stable-granite" {
		t.Fatalf("names = %v", names)
	}
	sc := byName["stable-coder"]
	e, _ := fx.store.Get("stable-coder")
	if sc.State != meshapi.AliasStateOK || sc.Resolved == nil || *sc.Resolved != "Qwen3.8-27B" || sc.UpdatedAt != formatUpdatedAt(e.TS) || sc.UpdatedBy != "gb1" || sc.Target != "Qwen3.8-27B" {
		t.Errorf("I1: %+v", sc)
	}
	if sg := byName["stable-granite"]; sg.State != meshapi.AliasStateFallback || sg.Resolved == nil || *sg.Resolved != "gemma-4-31B-it" {
		t.Errorf("I1: %+v", sg)
	}
	dead := byName["dead"]
	raw, _ := json.Marshal(dead)
	if dead.State != meshapi.AliasStateUnavailable || !strings.Contains(string(raw), `"resolved":null`) || dead.Fallbacks == nil {
		t.Errorf("I2: %s", raw)
	}
	if g := byName["granite"]; g.State != meshapi.AliasStateShadowed || g.Resolved != nil {
		t.Errorf("I3: %+v", g)
	}
	if got := formatUpdatedAt(1789120000123); got != "2026-09-11T09:46:40.123Z" {
		t.Errorf("I4: %s", got)
	}

	entries := fx.resolver.ModelEntries()
	var ids []string
	for _, e := range entries {
		if e.Object != "model" || e.OwnedBy != meshapi.OwnedByAlias || e.Target == "" {
			t.Errorf("I5: %+v", e)
		}
		ids = append(ids, e.ID)
	}
	if strings.Join(ids, ",") != "dead,granite,stable-coder,stable-granite" {
		t.Errorf("I5: ids = %v", ids)
	}
}

func TestValidateWrite(t *testing.T) {
	fx := newResolverFx(t)
	fx.aliases(t)
	req := func(target string, force bool, fallbacks ...string) meshapi.AliasWriteRequest {
		return meshapi.AliasWriteRequest{Target: target, Fallbacks: fallbacks, Force: force}
	}
	long := func(i int) string { return fmt.Sprintf("%02d%s", i, strings.Repeat("x", 58)) }
	var many []string
	for i := 0; i < 20; i++ {
		many = append(many, long(i))
	}
	cases := []struct {
		id, name string
		req      meshapi.AliasWriteRequest
		want     string // "" = valid
	}{
		{"W1", "bad name", req("Qwen3.8-27B", false), "must match"},
		{"W2", "Qwen3.8-27B", req("gemma-4-31B-it", false), "real model or pipeline"},
		{"W3", "translate", req("gemma-4-31B-it", false), "real model or pipeline"},
		{"W4", "new", req("", false), "target is required"},
		{"W5", "new", req("stable-coder", false), "cannot point at aliases"},
		{"W6", "new", req("Qwen3.8-27B", false, "stable-coder"), "cannot point at aliases"},
		{"W7", "new", req("nosuch", false), "use --force"},
		{"W8", "new", req("nosuch", true), ""},
		{"W9", "new", req("Qwen3.8-27B", false, "Qwen3.8-27B"), "listed twice"},
		{"W10", "new", req("Qwen3.8-27B", false, "gemma-4-31B-it", "gemma-4-31B-it"), "listed twice"},
		{"W13", "new", req("Qwen3.8-27B", true, many...), "too large to gossip"},
		{"W14", "new", req("Qwen3.8-27B", false, "gemma-4-31B-it"), ""},
		// A pipeline target is refused outright, and --force does not help:
		// force pre-stages a model that will later appear in a report, and a
		// pipeline never does. An alias is replicated to every node; a
		// pipeline runs only where it is configured.
		{"W15", "new", req("translate", false), "is a pipeline"},
		{"W16", "new", req("translate", true), "is a pipeline"},
		{"W17", "new", req("Qwen3.8-27B", true, "translate-fi"), "is a pipeline"},
	}
	for _, c := range cases {
		err := fx.resolver.ValidateWrite(c.name, c.req)
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: %v", c.id, err)
			}
			continue
		}
		var we *WriteError
		if !errors.As(err, &we) || we.Status != 409 || !strings.Contains(we.Message, c.want) {
			t.Errorf("%s: err = %v, want 409 containing %q", c.id, err, c.want)
		}
	}

	t.Run("W11 W12 table full", func(t *testing.T) {
		fx := newResolverFx(t)
		for i := 0; i < meshapi.MaxAliases; i++ {
			mustSet(t, fx.store, fmt.Sprintf("a%03d", i), "Qwen3.8-27B")
		}
		var we *WriteError
		if err := fx.resolver.ValidateWrite("new", req("Qwen3.8-27B", false)); !errors.As(err, &we) || !strings.Contains(we.Message, "table is full") {
			t.Errorf("W11: %v", err)
		}
		if err := fx.resolver.ValidateWrite("a000", req("gemma-4-31B-it", false)); err != nil {
			t.Errorf("W12: %v", err)
		}
	})
}
