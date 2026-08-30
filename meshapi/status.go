package meshapi

// Backend status vocabulary, as it appears in BackendInfo.Status. These are
// the strings a node publishes, not an implementation's internal enum: viiwork
// derives them from balancer.BackendStatus, and another node may compute them
// from something else entirely. What the mesh agrees on is the word.
//
// Only StatusHealthy is routable. The dashboard renders the rest as degraded,
// and a consumer must treat an unrecognised value as not-routable rather than
// assuming it is fine — a newer node may report a state this build predates.
const (
	StatusStarting  = "starting"
	StatusHealthy   = "healthy"
	StatusUnhealthy = "unhealthy"
	StatusDead      = "dead"
)

// StatusResponse is what a node publishes on PathStatus. It is polled by every
// peer on peers.poll_interval and is the sole basis on which a node exists in
// the mesh: models, load, GPUs, memory and power for one host.
//
// Nearly every field below is omitempty, and that is the compatibility story
// for the whole mesh — see the package doc. A peer on an older build omits
// what it does not know and the fleet view degrades to blanks for that host
// rather than breaking.
type StatusResponse struct {
	NodeID          string        `json:"node_id"`
	Hostname        string        `json:"hostname,omitempty"`
	ListenAddr      string        `json:"listen_addr,omitempty"`
	Models          []string      `json:"models"`
	Backends        []BackendInfo `json:"backends"`
	TotalInFlight   int64         `json:"total_in_flight"`
	HealthyBackends int           `json:"healthy_backends"`
	TotalBackends   int           `json:"total_backends"`
	PowerWatts      float64       `json:"power_watts"`
	PowerAvailable  bool          `json:"power_available"`
	// PowerSource names which IPMI reading this node settled on ("dcmi",
	// `sdr type "Power Supply"`, "sensor:SYS_POWER"). It exists so a fleet-wide
	// power outage of the reporting kind is visible without reading logs on
	// five hosts. Absent from nodes older than the probing sampler.
	PowerSource string `json:"power_source,omitempty"`

	CostAvailable  bool               `json:"cost_available"`
	CostEURPerHour float64            `json:"cost_eur_per_hour,omitempty"`
	CostTodayEUR   float64            `json:"cost_today_eur,omitempty"`
	CostBreakdown  *CostBreakdownJSON `json:"cost_breakdown,omitempty"`

	// GPUs lets any node render GPU load for every host in the mesh, not just
	// its own. omitempty keeps this compatible with nodes that predate it.
	//
	// This is every GPU the host's SMI tool reports, not only the cards this
	// instance drives. On a host running several instances that means each one
	// publishes the same full list, so a consumer summing GPUs as they arrive
	// counts a co-located host once per instance — key by Hostname and GPUID.
	GPUs []GPUInfo `json:"gpus,omitempty"`

	// PromptHistory is how many requests this node keeps prompt and output
	// for. Absent from nodes older than v1.1.1, which is why a consumer must
	// read zero as "unknown", not as "keeps nothing".
	PromptHistory int `json:"prompt_history,omitempty"`

	// Host RAM, so the mesh view can show memory pressure for every host and
	// not only the one it is served from. Used is MemTotal - MemAvailable, so
	// reclaimable page cache is not counted as pressure. Absent from older
	// nodes, and a zero total means "unknown", not "no memory".
	HostMemTotalMB int64 `json:"host_mem_total_mb,omitempty"`
	HostMemUsedMB  int64 `json:"host_mem_used_mb,omitempty"`

	// EnergyKWh24h is whole-node energy over the rolling last 24 hours, from
	// the durable store, which runs on exactly one instance per host — so on a
	// multi-model host the other instances report nothing rather than a second
	// copy of it.
	EnergyKWh24h float64 `json:"energy_kwh_24h,omitempty"`
	// EnergyKWh30d is the same over the rolling last 30 days.
	EnergyKWh30d float64 `json:"energy_kwh_30d,omitempty"`
}

// BackendInfo is one routable inference backend: a model server this node
// supervises, on one GPU or on a group of them.
//
// The Slot* and Tok* fields describe occupancy of whatever batching the server
// underneath actually does. In viiwork they come from llama.cpp's /slots; an
// implementation over a server with a different scheduler maps its own
// equivalents on, and omits what has no honest counterpart rather than
// inventing one. omitempty is what makes that safe.
type BackendInfo struct {
	GPUID    int    `json:"gpu_id"`
	GPUIDs   []int  `json:"gpu_ids,omitempty"` // populated in tensor-split mode
	Model    string `json:"model"`
	Status   string `json:"status"`
	InFlight int64  `json:"in_flight"`
	// Everything below is additive and omitempty: a node running an older
	// build simply omits these and the mesh view degrades to blanks for that
	// host rather than breaking. Without them a peer's backends arrive with
	// no RSS or context figures, which is most of what the mesh view shows.
	RSSMB      int64 `json:"rss_mb,omitempty"`
	SlotCtx    int64 `json:"slot_ctx,omitempty"`
	SlotCount  int   `json:"slot_count,omitempty"`
	SlotActive int   `json:"slot_active,omitempty"`
	TokDecoded int64 `json:"tok_decoded,omitempty"`
	TokRemain  int64 `json:"tok_remain,omitempty"`
}

// GPUInfo is a single GPU's live utilisation, propagated across the mesh so any
// node can render GPU load for every host. Locally this comes from the host's
// SMI collector; for peers it arrives on the PathStatus poll.
type GPUInfo struct {
	GPUID       int     `json:"gpu_id"`
	Util        float64 `json:"util"`
	VRAMUsedMB  float64 `json:"vram_used_mb"`
	VRAMTotalMB float64 `json:"vram_total_mb"`
}

// CostBreakdownJSON is the per-kWh price a node is currently paying, split into
// its components so the dashboard can show why the rate is what it is.
type CostBreakdownJSON struct {
	SpotCentsKWh     float64 `json:"spot_cents_kwh"`
	TransferCentsKWh float64 `json:"transfer_cents_kwh"`
	TaxCentsKWh      float64 `json:"tax_cents_kwh"`
	VATPercent       float64 `json:"vat_percent"`
	TotalCentsKWh    float64 `json:"total_cents_kwh"`
}
