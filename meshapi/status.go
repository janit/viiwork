package meshapi

// Backend status words as published in BackendStatus.Status. Only
// StatusHealthy is routable; treat an unknown word as not routable.
const (
	StatusStarting  = "starting"
	StatusHealthy   = "healthy"
	StatusUnhealthy = "unhealthy"
	StatusDead      = "dead"
)

// NodeStatus is one machine's state, served on PathStatus.
type NodeStatus struct {
	Node    string `json:"node"`
	NodeID  string `json:"node_id"`
	Ver     string `json:"ver"`
	Addr    string `json:"addr"`
	APIPort int    `json:"api_port"`
	// UptimeS lets a consumer detect a restart, which resets the per-model
	// counters in ModelStatus.
	UptimeS        int64         `json:"uptime_s"`
	HostMemTotalMB int64         `json:"host_mem_total_mb,omitempty"`
	HostMemUsedMB  int64         `json:"host_mem_used_mb,omitempty"`
	GPUs           []GPUInfo     `json:"gpus,omitempty"`
	Models         []ModelStatus `json:"models"`
	Power          PowerInfo     `json:"power"`
	EnergyKWh24h   float64       `json:"energy_kwh_24h,omitempty"`
	EnergyKWh30d   float64       `json:"energy_kwh_30d,omitempty"`
	// ClientEnergy (v2.10) is the GPU energy this node spent per named
	// client. Absent on a node with no energy store, on one that keeps no
	// client energy, on an older version, and until the node has served a
	// named client. Absent is "cannot say", never zero.
	ClientEnergy  *ClientEnergy `json:"client_energy,omitempty"`
	Cost          CostInfo      `json:"cost"`
	PromptHistory int           `json:"prompt_history,omitempty"`
}

// GPUInfo is one card as the host's SMI tool reports it.
type GPUInfo struct {
	Index       int     `json:"index"`
	UUID        string  `json:"uuid,omitempty"`
	Name        string  `json:"name,omitempty"`
	Vendor      string  `json:"vendor,omitempty"`
	Util        float64 `json:"util"`
	VRAMUsedMB  float64 `json:"vram_used_mb"`
	VRAMTotalMB float64 `json:"vram_total_mb"`
	PowerW      float64 `json:"power_w,omitempty"`
}

// ModelStatus is one model on a node. Slots counts healthy backends only.
// RequestsTotal and TokensTotal are cumulative since the node started and are
// counted on the node that executed each request.
type ModelStatus struct {
	Name   string `json:"name"`
	Engine string `json:"engine"`
	// EngineName (v2.9.2) is the engine's name for people ("llama.cpp" where
	// Engine is "llamacpp"); Engine stays the stable id. Absent on an older
	// node: show Engine instead.
	EngineName string `json:"engine_name,omitempty"`
	// EngineVersion (v2.9.2) is the version of the engine this node runs, in
	// the engine's own spelling ("b11371", "0.31.0"). Absent when the engine
	// reports none, when it could not be read, and until the node has read
	// it. Absent is "cannot say".
	EngineVersion string          `json:"engine_version,omitempty"`
	Slots         int             `json:"slots"`
	Busy          int             `json:"busy"`
	Queued        int             `json:"queued"`
	Ctx           int64           `json:"ctx"`
	RequestsTotal uint64          `json:"requests_total,omitempty"`
	TokensTotal   uint64          `json:"tokens_total,omitempty"`
	Perf          *PerfScore      `json:"perf,omitempty"` // v2.7; nil = no score
	Backends      []BackendStatus `json:"backends"`
	// Parked (v2.7.1) is a configured model taken down on this node by
	// `viiwork down`: no backends and no slots until `viiwork up` or a
	// restart. It tells a parked model from a broken one; absent on older
	// nodes.
	Parked bool `json:"parked,omitempty"`
	// Images (v2.10) is whether the model reads images on this node, as in
	// ModelCapacity. Absent is "cannot say", never "no".
	Images *bool `json:"images,omitempty"`
	// KVShare (v2.11) counts this node's session replication for the model
	// since it started. Absent when the model shares nothing.
	KVShare *KVShareStats `json:"kv_share,omitempty"`
	// Modality (v2.13) is the model's kind, as in ModelCapacity: absent is
	// text. A dashboard marks a non-text model rather than offering chat.
	Modality string `json:"modality,omitempty"`
}

// KVShareStats is one model's session replication on one node: pushes it
// sent to the members of its kv_share group, puts it received, and imports
// into its own backends ahead of a spilled turn.
type KVShareStats struct {
	Group        string `json:"group"`
	Pushes       int64  `json:"pushes"`
	PushFailures int64  `json:"push_failures"`
	BytesSent    int64  `json:"bytes_sent"`
	Puts         int64  `json:"puts"`
	Imports      int64  `json:"imports"`
	ImportFails  int64  `json:"import_failures"`
	Replicas     int    `json:"replicas"`
	StoreBytes   int64  `json:"store_bytes"`
}

// BackendStatus is one backend process of a model.
type BackendStatus struct {
	ID         string `json:"id"`
	GPUs       []int  `json:"gpus,omitempty"`
	Status     string `json:"status"`
	Phase      string `json:"phase,omitempty"`
	PID        int    `json:"pid,omitempty"`
	RSSMB      int64  `json:"rss_mb,omitempty"`
	Slots      int    `json:"slots"`
	Busy       int    `json:"busy"`
	TokDecoded int64  `json:"tok_decoded,omitempty"`
	TokRemain  int64  `json:"tok_remain,omitempty"`
	Respawns   int    `json:"respawns"`
	UptimeS    int64  `json:"uptime_s"`
}

// PowerInfo is whole-node power from IPMI.
type PowerInfo struct {
	Watts     float64 `json:"watts"`
	Available bool    `json:"available"`
	Source    string  `json:"source,omitempty"`
}

// CostInfo is the node's electricity cost.
type CostInfo struct {
	Available  bool           `json:"available"`
	EURPerHour float64        `json:"eur_per_hour,omitempty"`
	TodayEUR   float64        `json:"today_eur,omitempty"`
	Breakdown  *CostBreakdown `json:"breakdown,omitempty"`
}

// ClientEnergy is one node's energy per named client. The counters in it are
// this node's alone. There is no mesh-wide total, on purpose: a sum over a
// set of nodes that changes goes down when a node leaves, which a counter
// must never do. A consumer keeps one counter per node, keyed by NodeID, and
// adds differences. docs/client-energy.md gives the rule.
type ClientEnergy struct {
	// Since is when this node began counting, Unix seconds. It changes only
	// when the node's store is recreated, and then the counters start at zero.
	Since int64 `json:"since"`
	// Through is the start of the last minute counted. The counters move once
	// a minute and run 60 to 90 seconds behind the clock.
	Through int64 `json:"through"`
	// Clients is sorted by name and holds at most 32, the largest by WhTotal.
	Clients []ClientEnergyEntry `json:"clients"`
}

// ClientEnergyEntry is one client on one node. WhTotal, BusySTotal and
// TokensTotal only grow while Since stays the same. W1m, KWh24h and KWh30d
// are this node's own readings, for someone looking at one node.
type ClientEnergyEntry struct {
	Name        string  `json:"name"`
	WhTotal     float64 `json:"wh_total"`
	W1m         float64 `json:"w_1m"` // mean draw over the minute at Through
	KWh24h      float64 `json:"kwh_24h"`
	KWh30d      float64 `json:"kwh_30d"`
	BusySTotal  uint64  `json:"busy_s_total"`
	TokensTotal uint64  `json:"tokens_total"`
}

// CostBreakdown splits the current per-kWh price into its components.
type CostBreakdown struct {
	SpotCentsKWh     float64 `json:"spot_cents_kwh"`
	TransferCentsKWh float64 `json:"transfer_cents_kwh"`
	TaxCentsKWh      float64 `json:"tax_cents_kwh"`
	VATPercent       float64 `json:"vat_percent"`
	TotalCentsKWh    float64 `json:"total_cents_kwh"`
}

// PerfScore is a model's measured time to first token on this node, as
// /v1/status shows it. The router reads the flat fields on ModelCapacity.
type PerfScore struct {
	OverheadMs   int   `json:"overhead_ms"`
	MsPer1k      int   `json:"prefill_ms_per_1k"`
	Samples      int   `json:"samples,omitempty"`
	BaselineAgeS int64 `json:"baseline_age_s,omitempty"`
}
