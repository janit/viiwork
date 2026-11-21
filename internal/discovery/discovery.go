// Package discovery is viiwork's canonical answer to "what can this fleet
// serve, and how large a prompt may I send it".
//
// The market standardised the transport and never standardised discovery.
// OpenAI's GET /v1/models returns {id, object, created, owned_by} and nothing
// else — no window, no capabilities — so every coding client either ships a
// hardcoded table of models it knows or makes the user type the numbers in.
// Several projects are independently closing that gap right now, in mutually
// incompatible spellings of the same integer.
//
// viiwork is unusually well placed to answer, because every one of those
// formats assumes one host with one model loaded while a viiwork node already
// computes the minimum context across the hosts its router would route to.
// That number, not a checkpoint's architectural maximum, is what a client can
// safely size a prompt against.
//
// This package holds ONE record of it. The external spellings are thin
// serializers over this type and never state of their own: three formats that
// each assembled their own view would be three things to keep true, and the
// first one written would silently become the source of truth for the rest.
// A serializer may drop a field its format has no room for, and may supply a
// substitute for a field its format demands — but it may not invent a number
// and call it viiwork's.
package discovery

import (
	"sort"

	"github.com/janit/viiwork/v2/meshapi"
)

// Model is what this fleet can serve, as a client can rely on it.
//
// Absent is not zero, as everywhere else in viiwork: a field this node cannot
// measure is left at its zero value meaning "no host can currently say", never
// meaning "measured as none". Deliberately absent from the type: any
// prompt-only ceiling, any output ceiling, cost, and any modality beyond text
// and images. viiwork knows none of them, and a record that carried them
// would be fabricating rather than reporting.
type Model struct {
	// Name is the id a client names, and is what /v1/models lists.
	Name string
	// Kind is a meshapi.OwnedBy* value: local, peer, pipeline or alias.
	Kind string
	// Target is the alias's target, else "". An alias with a target nothing
	// serves keeps the name and inherits no numbers.
	Target string
	// Engine is the inference engine serving it, or "" when nothing can say.
	Engine string

	// ServedContext is the total prompt+completion budget per slot that the
	// fleet GUARANTEES: the minimum across the hosts the router would route
	// to. It is NOT the checkpoint's architectural maximum, which no node
	// knows — a model loaded at 32K on one machine and 98K on another is
	// served at 32K by the fleet, because a request may land on either.
	//
	// Zero means no host can currently say.
	ServedContext int64

	// Tools is whether a client may use this model as an agent. Every model
	// viiwork serves is reached through one OpenAI-compatible endpoint that
	// passes tool definitions straight through to the engine, so this is a
	// property of the seam rather than of the checkpoint.
	Tools bool

	// Images is whether a request that carries an image has somewhere to go:
	// a fresh host with slots whose engine says its backends read images.
	// False covers "no host does" and "no host can say"; the fleet view keeps
	// the two apart for a consumer that needs to.
	Images bool

	// Hosts is how many hosts stand behind ServedContext: fresh, and with
	// slots the router can use. Zero means nothing can currently take work
	// for this model, which is also why ServedContext may be zero.
	Hosts int
	// OldestAgeMS is the age of the oldest report among those hosts. It is
	// the honest half of the record: a window is only as current as the
	// slowest host that agreed to it.
	OldestAgeMS int64
}

// Source is the node's two published model views: what it serves, and how much
// context each model has. *proxy.Handler satisfies it.
type Source interface {
	// ModelEntries is everything GET /v1/models lists.
	ModelEntries() []meshapi.ModelEntry
	// FleetModels is the fleet's per-model capacity, which carries context.
	FleetModels() []meshapi.FleetModel
}

// Models builds the record: one entry per model the node lists, joined to the
// fleet's capacity view for its numbers.
//
// /v1/models is the authority on WHICH models exist, because it is the list a
// client may name and includes the node-local ones — pipelines and aliases —
// that never appear in a capacity report. The fleet view is the authority on
// their NUMBERS. Driving the walk from the entry list is what stops a model
// being callable but undiscoverable.
//
// The one exception runs the other way: a model the fleet view marks with a
// non-text modality (v2.13: a voice session model, an image generator) is
// left out, and so is an alias that targets one. Text inference refuses it,
// so listing it would make it discoverable but not callable.
//
// Pure: no I/O and no clock. Both inputs are already-computed snapshots.
func Models(src Source) []Model {
	fleet := map[string]meshapi.FleetModel{}
	for _, fm := range src.FleetModels() {
		fleet[fm.Name] = fm
	}

	entries := src.ModelEntries()
	out := make([]Model, 0, len(entries))
	for _, e := range entries {
		m := Model{
			Name:   e.ID,
			Kind:   e.OwnedBy,
			Target: e.Target,
			Tools:  true,
		}
		// An alias has no capacity row of its own — it is a name for another
		// model's capacity — so it reads its target's row. A target nothing
		// serves has no row either, and the zero values are then correct.
		name := e.ID
		if e.Target != "" {
			name = e.Target
		}
		fm, ok := fleet[name]
		if ok && !meshapi.IsTextModality(fm.Modality) {
			// A model no host serves as text (a voice session model, an
			// image generator) is not one a chat client can use, and an
			// alias to one is no better. Every serializer of this record is
			// a chat-client surface, so it is left out here, once.
			continue
		}
		if ok {
			m.Engine = fm.Engine
			m.ServedContext = fm.Ctx
			m.Hosts, m.OldestAgeMS = provenance(fm)
			m.Images = fm.Images != nil && *fm.Images
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// provenance counts the hosts that stand behind a model's window, and the age
// of the oldest of their reports.
//
// The set is exactly the one the window is a minimum over: fresh, and with
// slots. A stale host asserts no numbers at all, and a host reporting zero
// slots contributes no capacity the router will ever use — counting either as
// provenance would attach a figure to hosts that cannot honour it.
func provenance(fm meshapi.FleetModel) (hosts int, oldestMS int64) {
	for _, h := range fm.Hosts {
		if h.Stale || h.Slots == nil || *h.Slots <= 0 {
			continue
		}
		hosts++
		if h.AgeMS > oldestMS {
			oldestMS = h.AgeMS
		}
	}
	return hosts, oldestMS
}
