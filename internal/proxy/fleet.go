package proxy

import (
	"net/http"
	"sort"
	"time"

	"github.com/janit/viiwork/v2/internal/httpjson"
	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

// handleFleetCapacity serves /v1/fleet/capacity: this node's view of what the
// whole mesh can serve, with a per-host breakdown.
//
// It exists because /v1/capacity is node-local by definition, and a consumer
// sizing its own concurrency from it gets its host's share rather than the
// fleet's — 12 where the fleet serves 36, with nothing to signal the
// difference. See the design spec.
//
// Nothing is polled here. Every node already holds every alive member's
// capacity report, refreshed every mesh.capacity_poll, so this is a read of
// state that exists and adds no inter-node traffic.
func (h *Handler) handleFleetCapacity(w http.ResponseWriter, r *http.Request) {
	out := h.fleetView()

	// A consumer that wants one model should not have to parse the fleet. An
	// unknown name yields an empty list rather than 404: "no capacity for that
	// model" is an answer, not a failure.
	out.Models, out.ResolvedFrom = filterModel(out.Models, r.URL.Query().Get("model"), h.d.Resolve)
	if out.Models == nil {
		out.Models = []meshapi.FleetModel{}
	}
	httpjson.Write(w, http.StatusOK, out)
}

// filterModel narrows the fleet view to one model, resolving an alias to the
// real name first, and reports the alias it resolved through ("" when it did
// not resolve one). want "" returns models untouched; resolve may be nil.
//
// An alias is a name for capacity, so it has to name capacity here too: the
// configuration we recommend to consumers is an alias, and an empty models[]
// is the documented signal for "the fleet cannot serve this". Without this the
// recommended config falls back to its own constant forever, silently.
//
// Resolve is identity for a real, unknown or shadowed name, so this only ever
// rewrites want when it really is a live alias with a served target.
//
// A ResolveError — a live alias nothing serves — is deliberately SWALLOWED
// rather than propagated. On the inference path a 503 is correct: the caller
// asked for work to be done and it cannot be. A capacity query is a different
// question, and this endpoint has already answered it; an alias whose target
// nobody serves has exactly zero capacity, which the empty list already says.
// A 503 would read as "the platform is down" and trip a consumer's outage
// branch over a normal operating state. Keeping the original want is what
// implements that: it matches nothing, and the list comes back empty.
func filterModel(models []meshapi.FleetModel, want string, resolve Resolver) ([]meshapi.FleetModel, string) {
	if want == "" {
		return models, ""
	}
	var from string
	if resolve != nil {
		if real, alias, err := resolve(want); err == nil && real != "" {
			want, from = real, alias
		}
	}
	kept := models[:0]
	for _, m := range models {
		if m.Name == want {
			kept = append(kept, m)
		}
	}
	return kept, from
}

// fleetAcc accumulates one model while walking hosts.
type fleetAcc struct {
	model meshapi.FleetModel
	seen  bool // a fresh host WITH SLOTS has contributed a Ctx
	// What the fresh hosts with slots say about reading images.
	imgYes, imgUnknown bool
}

// aggregateFleet builds the fleet view from this node's own capacity plus every
// member report it holds. Pure: no I/O, no clock of its own.
//
// self is the answering node's name, and is also how a report from ourselves is
// discarded — a mesh can transiently produce one, and counting it would double
// this node's slots.
//
// staleAfter is routing.stale_after, and freshness uses capacity.Fresh: the
// same predicate the router applies when deciding whether a peer may receive a
// request. Sharing it is deliberate — a second rule here would drift, and the
// number a consumer sees would stop matching the capacity the router will
// actually use.
//
// Modality: a model some host lists as text (this node's own models always
// are) is a text model, and only its text hosts count toward the totals and
// the context floor — a voice host of the same name lends nothing to text
// inference, so its slots would be capacity the router will never use. A
// host of another modality is still listed, carrying its Modality. A model
// that no host lists as text carries the modality instead, and its totals
// are its own hosts' sessions. Listing, like the host list, is age-blind.
func aggregateFleet(self string, local []meshapi.ModelCapacity, reports []capacity.Report, now time.Time, staleAfter time.Duration) meshapi.FleetCapacityResponse {
	byModel := map[string]*fleetAcc{}

	textNames := map[string]bool{}
	for _, m := range local {
		textNames[m.Name] = true
	}
	for _, rep := range reports {
		if rep.Node == self {
			continue
		}
		for _, m := range rep.Models {
			if meshapi.IsTextModality(m.Modality) {
				textNames[m.Name] = true
			}
		}
	}

	add := func(node, api string, m meshapi.ModelCapacity, fresh bool, age time.Duration) {
		a := byModel[m.Name]
		if a == nil {
			a = &fleetAcc{model: meshapi.FleetModel{Name: m.Name}}
			byModel[m.Name] = a
		}
		text := meshapi.IsTextModality(m.Modality)
		if !textNames[m.Name] && (a.model.Modality == "" || m.Modality < a.model.Modality) {
			a.model.Modality = m.Modality
		}
		// Engine names the model's engine; for a text model, a text host's.
		if a.model.Engine == "" && (text || !textNames[m.Name]) {
			a.model.Engine = m.Engine
		}
		host := meshapi.FleetHost{Node: node, API: api, AgeMS: age.Milliseconds()}
		if !text {
			host.Modality = m.Modality
		}
		if !fresh {
			// Absent is not zero: a host we cannot currently see has no numbers
			// this node can assert, so it carries none. It stays in the list so
			// "the fleet shrank" is distinguishable from "we lost sight of gb3".
			host.Stale = true
			a.model.Hosts = append(a.model.Hosts, host)
			return
		}

		free := m.Free() // clamps at 0
		slots, busy, ctx := m.Slots, m.Busy, m.Ctx
		host.Slots, host.Busy, host.Free, host.Ctx = &slots, &busy, &free, &ctx
		host.Images = m.Images
		if !text && textNames[m.Name] {
			// Its own numbers, but none of the text model's capacity.
			a.model.Hosts = append(a.model.Hosts, host)
			return
		}

		a.model.Slots += m.Slots
		a.model.Busy += m.Busy
		// Free is the sum of each host's clamped free, which is what the router
		// can place: an oversubscribed host (busy > slots, e.g. mid-drain) has
		// no spare slot to lend, and must not cancel spare slots elsewhere.
		a.model.Free += free
		a.model.Queued += m.Queued
		// The floor a consumer can rely on, not a mean no backend will honour.
		//
		// Only a host with SLOTS sets it. A draining or slot-less backend
		// contributes no capacity the router will ever use, so letting its
		// window depress the floor would publish a number no request is ever
		// sized by — and it is the smallest windows that drain first. When no
		// host has a slot the floor stays unset, which is correct: absent is
		// not zero, and nothing can promise a window it cannot serve.
		//
		// The host keeps its own numbers in the per-host list either way:
		// "gb2 is draining" and "gb2 is gone" call for different reactions.
		if m.Slots > 0 {
			if !a.seen || m.Ctx < a.model.Ctx {
				a.model.Ctx = m.Ctx
			}
			a.seen = true
			switch {
			case m.Images == nil:
				a.imgUnknown = true
			case *m.Images:
				a.imgYes = true
			}
		}
		a.model.Hosts = append(a.model.Hosts, host)
	}

	for _, m := range local {
		add(self, "", m, true, 0)
	}
	for _, rep := range reports {
		if rep.Node == self {
			continue // already counted from Local; a duplicate would double it
		}
		fresh := capacity.Fresh(rep, now, staleAfter)
		age := now.Sub(rep.Received)
		for _, m := range rep.Models {
			add(rep.Node, rep.APIAddr, m, fresh, age)
		}
	}

	out := meshapi.FleetCapacityResponse{
		View:        self,
		StaleAfterS: staleAfter.Seconds(),
		Models:      make([]meshapi.FleetModel, 0, len(byModel)),
	}
	for _, a := range byModel {
		m := a.model
		// Yes when a host the router can use says yes: that is where a request
		// with an image goes. No only when every such host says no.
		if a.imgYes || (a.seen && !a.imgUnknown) {
			yes := a.imgYes
			m.Images = &yes
		}
		sort.Slice(m.Hosts, func(i, j int) bool { return m.Hosts[i].Node < m.Hosts[j].Node })
		out.Models = append(out.Models, m)
	}
	// Ordered so a consumer diffing two polls sees real changes rather than map
	// iteration order.
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].Name < out.Models[j].Name })
	return out
}

// fleetView is this node's aggregate of the whole mesh, unfiltered.
func (h *Handler) fleetView() meshapi.FleetCapacityResponse {
	var local []meshapi.ModelCapacity
	if h.d.Local != nil {
		local = h.d.Local.Capacity()
	}
	var reports []capacity.Report
	if h.d.Reports != nil {
		reports = h.d.Reports.Reports()
	}
	out := aggregateFleet(h.d.Self, local, reports, time.Now(), h.d.StaleAfter)
	out.Ver = h.d.Version
	return out
}

// FleetModels is the fleet's per-model capacity, which carries the context
// window. It is exported as one half of discovery.Source: every serializer
// that advertises a window to a client sizes it from these figures.
func (h *Handler) FleetModels() []meshapi.FleetModel {
	return h.fleetView().Models
}
