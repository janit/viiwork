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
	Cost           CostInfo      `json:"cost"`
	PromptHistory  int           `json:"prompt_history,omitempty"`
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
