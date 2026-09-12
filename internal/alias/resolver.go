package alias

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/internal/proxy"
	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

// maxBroadcastBytes bounds an alias broadcast so it fits one gossip packet
// (Decision 1).
const maxBroadcastBytes = 1000

// LocalCapacity is this node's per-model occupancy; *supervisor.Supervisor
// satisfies it.
type LocalCapacity interface {
	Capacity() []meshapi.ModelCapacity
}

// Reports is the members' capacity reports; *capacity.Poller satisfies it.
type Reports interface {
	Reports() []capacity.Report
}

// ServedView answers which models exist and which are served (Decision 3).
type ServedView interface {
	// Exists: configured on this node, or listed in any member's report,
	// whatever its age. Validation and shadowing use it, so they do not flap
	// while a model restarts.
	Exists(model string) bool
	// Served: a healthy backend here, or a fresh report with healthy backends.
	// Resolution uses it, so a fallback answers only when the target cannot.
	Served(model string) bool
}

type servedView struct {
	self       string
	local      LocalCapacity
	reports    Reports
	staleAfter time.Duration
	now        func() time.Time
}

func NewServedView(self string, local LocalCapacity, reports Reports, staleAfter time.Duration, now func() time.Time) ServedView {
	if now == nil {
		now = time.Now
	}
	return &servedView{self: self, local: local, reports: reports, staleAfter: staleAfter, now: now}
}

func (v *servedView) Exists(model string) bool {
	for _, mc := range v.local.Capacity() {
		if mc.Name == model {
			return true
		}
	}
	for _, rep := range v.reports.Reports() {
		if rep.Node == v.self {
			continue
		}
		if _, ok := rep.Model(model); ok {
			return true
		}
	}
	return false
}

func (v *servedView) Served(model string) bool {
	for _, mc := range v.local.Capacity() {
		if mc.Name == model && mc.HealthyBackends > 0 {
			return true
		}
	}
	now := v.now()
	for _, rep := range v.reports.Reports() {
		if rep.Node == v.self || !capacity.Fresh(rep, now, v.staleAfter) {
			continue
		}
		if mc, ok := rep.Model(model); ok && mc.HealthyBackends > 0 {
			return true
		}
	}
	return false
}

// WriteError is a refused alias write: 404 or 409, with a message for the
// operator.
type WriteError struct {
	Status  int // 404 or 409
	Message string
}

func (e *WriteError) Error() string { return e.Message }

func conflictf(format string, args ...any) *WriteError {
	return &WriteError{Status: 409, Message: fmt.Sprintf(format, args...)}
}

// Resolver turns requested names into real models, on the origin node only.
type Resolver struct {
	store         *Store
	served        ServedView
	pipelineNames func() []string
	logf          func(string, ...any)

	mu       sync.Mutex
	shadowed map[string]bool // shadowing already logged
}

var (
	_ proxy.Resolver    = (*Resolver)(nil).Resolve
	_ proxy.ModelLister = (*Resolver)(nil).ModelEntries
)

func NewResolver(store *Store, served ServedView, pipelineNames func() []string, logf func(string, ...any)) *Resolver {
	if pipelineNames == nil {
		pipelineNames = func() []string { return nil }
	}
	return &Resolver{store: store, served: served, pipelineNames: pipelineNames, logf: logf, shadowed: map[string]bool{}}
}

// real reports whether name is a real model or a pipeline name.
func (r *Resolver) real(name string) bool {
	return r.served.Exists(name) || r.isPipeline(name)
}

// isPipeline reports whether name is one of this node's pipeline virtual
// models.
func (r *Resolver) isPipeline(name string) bool {
	for _, p := range r.pipelineNames() {
		if p == name {
			return true
		}
	}
	return false
}

// Resolve is proxy.Resolver. It runs on every request, so a name that is not
// a live alias returns before anything else is consulted: real names and
// unknown names both resolve to themselves.
func (r *Resolver) Resolve(requested string) (model, alias string, err error) {
	e, ok := r.store.peek(requested)
	if !ok || e.Deleted {
		return requested, "", nil
	}
	if r.checkShadowed(requested) {
		return requested, "", nil
	}
	if m, ok := r.pick(e); ok {
		return m, requested, nil
	}
	return "", "", &proxy.ResolveError{
		Status:     503,
		Type:       meshapi.ErrTypeUnavailable,
		Message:    fmt.Sprintf("alias %s: no node serves %s or its fallbacks", requested, e.Target),
		RetryAfter: 5,
	}
}

// pick is the target when served, else the first served fallback. A target
// that is served but full is still the answer: the request queues for it.
func (r *Resolver) pick(e meshapi.AliasEntry) (string, bool) {
	if r.served.Served(e.Target) {
		return e.Target, true
	}
	for _, f := range e.Fallbacks {
		if r.served.Served(f) {
			return f, true
		}
	}
	return "", false
}

// checkShadowed reports whether a live alias's name is real, logging it once
// per name until the name stops being shadowed.
func (r *Resolver) checkShadowed(name string) bool {
	shadowed := r.real(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case shadowed && !r.shadowed[name]:
		r.shadowed[name] = true
		if r.logf != nil {
			r.logf("alias %s is shadowed by a real model or pipeline of the same name", name)
		}
	case !shadowed:
		delete(r.shadowed, name)
	}
	return shadowed
}

// Info lists the live aliases as this node resolves them, sorted by name.
func (r *Resolver) Info() []meshapi.AliasInfo {
	t := r.store.Table()
	out := []meshapi.AliasInfo{}
	for _, name := range liveNames(t) {
		e := t.Aliases[name]
		in := meshapi.AliasInfo{
			Name:      name,
			Target:    e.Target,
			Fallbacks: copyStrings(e.Fallbacks),
			UpdatedAt: formatUpdatedAt(e.TS),
			UpdatedBy: e.By,
			State:     meshapi.AliasStateUnavailable,
		}
		switch m, ok := r.pick(e); {
		case r.checkShadowed(name):
			in.State = meshapi.AliasStateShadowed
		case ok && m == e.Target:
			in.State, in.Resolved = meshapi.AliasStateOK, &m
		case ok:
			in.State, in.Resolved = meshapi.AliasStateFallback, &m
		}
		out = append(out, in)
	}
	return out
}

// ModelEntries is proxy.ModelLister: every live alias as a /v1/models entry.
func (r *Resolver) ModelEntries() []meshapi.ModelEntry {
	t := r.store.Table()
	var out []meshapi.ModelEntry
	for _, name := range liveNames(t) {
		out = append(out, meshapi.ModelEntry{ID: name, Object: "model", OwnedBy: meshapi.OwnedByAlias, Target: t.Aliases[name].Target})
	}
	return out
}

// ValidateWrite checks a local write before it is stored; every refusal is a
// 409 naming the offending value.
func (r *Resolver) ValidateWrite(name string, req meshapi.AliasWriteRequest) error {
	if !meshapi.ValidAliasName(name) {
		return conflictf("alias name %q must match [A-Za-z0-9._-]{1,64}", name)
	}
	if r.real(name) {
		return conflictf("%q is the name of a real model or pipeline", name)
	}
	if req.Target == "" {
		return conflictf("target is required")
	}
	refs := append([]string{req.Target}, req.Fallbacks...)
	for _, x := range refs {
		if e, ok := r.store.peek(x); ok && !e.Deleted {
			return conflictf("%q is an alias; aliases cannot point at aliases", x)
		}
	}
	// A pipeline cannot be a target, and --force does not change that. Force
	// pre-stages a model that will later appear in a capacity report; a
	// pipeline never does, because it is node-local: absent from reports and
	// status, and handled only on the origin node. The alias table is
	// replicated to every member, so an alias pointing at one would dangle on
	// every node but the one that configures it. Refused before the --force
	// branch, whose message would otherwise tell the operator to force exactly
	// the alias that can never resolve.
	for _, x := range refs {
		if r.isPipeline(x) {
			return conflictf("%q is a pipeline: an alias is replicated to every node, but a pipeline runs only on the node that configures it", x)
		}
	}
	if !req.Force {
		for _, x := range refs {
			if !r.served.Exists(x) {
				return conflictf("%q is not served by any node (use --force to pre-stage it)", x)
			}
		}
	}
	seen := map[string]bool{req.Target: true}
	for _, f := range req.Fallbacks {
		if seen[f] {
			return conflictf("fallback %q is listed twice", f)
		}
		seen[f] = true
	}
	current, exists := r.store.peek(name)
	if (!exists || current.Deleted) && r.store.Live() >= meshapi.MaxAliases {
		return conflictf("alias table is full (%d)", meshapi.MaxAliases)
	}
	next := meshapi.AliasBroadcast{Name: name, Entry: meshapi.AliasEntry{
		Target:    req.Target,
		Fallbacks: copyStrings(req.Fallbacks),
		Ver:       current.Ver + 1,
		TS:        r.store.now().UnixMilli(),
		By:        r.store.self,
	}}
	if raw, err := json.Marshal(next); err != nil {
		return err
	} else if len(raw) > maxBroadcastBytes {
		return conflictf("alias %q is too large to gossip (%d bytes, limit %d)", name, len(raw), maxBroadcastBytes)
	}
	return nil
}

// formatUpdatedAt renders an entry's TS as RFC 3339 in UTC with milliseconds
// (Decision 9): two writes in one second stay distinguishable.
func formatUpdatedAt(ts int64) string {
	return time.UnixMilli(ts).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

func liveNames(t meshapi.AliasTable) []string {
	var names []string
	for name, e := range t.Aliases {
		if !e.Deleted {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
