package meshapi

// Fleet capacity: one node's view of what the whole mesh can serve.
//
// /v1/capacity is node-local by definition, and a consumer that reaches for it
// to size its own concurrency gets a number as small as its host's share — 12
// where the fleet serves 36. This is the endpoint that answers the question it
// was actually asking.
//
// Design: docs/superpowers/specs/2026-09-13-fleet-capacity-api-design.md.

// FleetCapacityResponse is one node's view of the whole mesh's capacity.
//
// It is ONE NODE'S VIEW and says so. Every node polls its own members, so a
// node that just joined, or one partitioned from part of the tailnet, answers
// differently. There is no fleet-wide truth to report, only whose picture this
// is.
type FleetCapacityResponse struct {
	View string `json:"view"` // the node that answered
	Ver  string `json:"ver,omitempty"`
	// StaleAfterS is routing.stale_after, the age past which a member's report
	// stops counting. Published so a consumer can interpret AgeMS itself.
	StaleAfterS float64      `json:"stale_after_s"`
	Models      []FleetModel `json:"models"`
	// ResolvedFrom is the alias the `model` query named, when it was one.
	// Present only when resolution actually rewrote the query.
	//
	// The alias never goes in FleetModel.Name: that would make two nodes
	// disagree about a model's name depending on how it was asked for. This
	// matches inference, where a request through an alias reports the real
	// model in the response body — one rule instead of two — and makes the
	// behaviour self-describing in a curl.
	ResolvedFrom string `json:"resolved_from,omitempty"`
}

// FleetModel aggregates one model across every host that serves it.
//
// The totals count ONLY hosts whose report is fresh — the same predicate the
// router uses to decide whether a peer may receive a request — so Free is the
// capacity the router would actually use, never an optimistic one.
type FleetModel struct {
	Name   string `json:"name"`
	Engine string `json:"engine,omitempty"`
	Slots  int    `json:"slots"`
	Busy   int    `json:"busy"`
	Free   int    `json:"free"`
	// Queued is the sum across fresh hosts, not this node's own queue: a field
	// beside fleet totals is read as a fleet total. A consumer wanting one
	// node's figure has the per-host entry.
	Queued int `json:"queued"`
	// Ctx is the MINIMUM context per slot across fresh hosts THAT HAVE SLOTS.
	// Nothing stops an operator serving one model at different contexts on
	// different machines, and a consumer sizing prompts needs the floor it can
	// rely on rather than a mean no backend will honour.
	//
	// The slot condition is what keeps the floor a promise: a draining host
	// takes no work, so its window is not one the fleet is offering. Absent
	// when no host has a slot — nothing can promise a window it cannot serve.
	Ctx   int64       `json:"ctx,omitempty"`
	Hosts []FleetHost `json:"hosts"`
	// Images (v2.10) is whether the fleet reads images with this model: true
	// when a fresh host with slots says it does, which is where the router
	// sends a request that carries one; false when every fresh host with
	// slots says it does not, and such a request is refused. Absent when no
	// host says yes and at least one cannot say.
	Images *bool `json:"images,omitempty"`
	// Modality (v2.13) is the model's kind when no host that lists it serves
	// it as text: every such host publishes a non-text modality (the
	// smallest name, should they differ). Absent is text. When some host
	// serves the name as text, the model is text, and the totals and Ctx
	// count only those hosts: a host of another modality is still listed,
	// with its own Modality, but lends no capacity to text inference.
	Modality string `json:"modality,omitempty"`
}

// FleetHost is one node's contribution to a model.
//
// A host whose report has gone stale is LISTED, not deleted: "the fleet shrank"
// and "we have lost sight of gb3" call for different reactions, and a bare
// total conflates them. Its numbers are omitted rather than zeroed or carried
// over — absent is not zero, applied to a host instead of a field.
type FleetHost struct {
	Node string `json:"node"`
	API  string `json:"api,omitempty"` // host:port a consumer can send to
	// Slots, Busy, Free and Ctx are pointers because a stale host has no values
	// this node can assert, and spelling that as zero would be a lie in the
	// shape of a measurement. They are non-nil for a fresh host — where Busy 0
	// is a real reading — and nil when Stale is true. A plain int with
	// omitempty cannot tell a measured 0 from an absent one, which is the
	// whole distinction.
	Slots *int   `json:"slots,omitempty"`
	Busy  *int   `json:"busy,omitempty"`
	Free  *int   `json:"free,omitempty"`
	Ctx   *int64 `json:"ctx,omitempty"`
	// AgeMS is how long ago this node last had a report from the host. Always
	// present: it is the one thing still assertable about a stale host.
	AgeMS int64 `json:"age_ms"`
	Stale bool  `json:"stale,omitempty"`
	// Images (v2.10) is the host's own answer, as in ModelCapacity. Absent
	// for a stale host and for one that cannot say.
	Images *bool `json:"images,omitempty"`
	// Modality (v2.13) is the host's own published modality, stale or not:
	// it describes the model, not an occupancy reading. Absent is text.
	Modality string `json:"modality,omitempty"`
}
