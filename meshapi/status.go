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
	Name          string          `json:"name"`
	Engine        string          `json:"engine"`
	Slots         int             `json:"slots"`
	Busy          int             `json:"busy"`
	Queued        int             `json:"queued"`
	Ctx           int64           `json:"ctx"`
	RequestsTotal uint64          `json:"requests_total,omitempty"`
	TokensTotal   uint64          `json:"tokens_total,omitempty"`
	Backends      []BackendStatus `json:"backends"`
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
