package meshapi

// ClusterResponse is the whole-mesh snapshot as one node sees it, served on
// PathCluster and pushed as the SSECluster event on PathMeshStream. Every node
// serves it, which is what makes one reachable node enough to view the fleet.
//
// It is assembled from the serving node's own state plus each peer's last
// PathStatus poll, so freshness differs by field and that difference matters:
// activity merged onto the mesh stream is live, while everything reconstructed
// from peer polls is up to peers.poll_interval stale (10s by default).
type ClusterResponse struct {
	NodeID     string            `json:"node_id"`
	Version    string            `json:"version,omitempty"`
	Hostname   string            `json:"hostname,omitempty"`
	SingleHost bool              `json:"single_host,omitempty"`
	Local      ClusterLocalInfo  `json:"local"`
	Peers      []ClusterPeerInfo `json:"peers"`
	Models     []string          `json:"models"`
	// PowerControl lists the hosts this node will accept chassis commands for.
	// Published because the dashboard has to render a row for a host that is
	// powered off — which is precisely a host absent from the mesh, so the
	// allowlist is the only place its name still exists.
	PowerControl          *PowerControlInfo `json:"power_control,omitempty"`
	ClusterCostEURPerHour float64           `json:"cluster_cost_eur_per_hour,omitempty"`
	ClusterCostTodayEUR   float64           `json:"cluster_cost_today_eur,omitempty"`
}

// PowerControlInfo is the chassis-control allowlist a node advertises.
//
// The allowlist is the entire authorization story and it has no wildcard:
// being peered with a node is not consent to switch its machine off. A node
// that can control nothing publishes nothing here rather than an empty list,
// which would read as "enabled, no hosts".
type PowerControlInfo struct {
	Hosts []string `json:"hosts"`
	// OutOfBand lists the subset reachable while powered off. A host outside it
	// can be switched off but not back on, and the view should say so rather
	// than offering a button that will fail.
	OutOfBand []string `json:"out_of_band,omitempty"`
}

// ClusterLocalInfo is the serving node's own contribution to the snapshot.
// It carries the extras a node knows about itself but cannot learn from a
// peer poll — its version, and the host readings it measures directly.
type ClusterLocalInfo struct {
	GPUs           []GPUInfo            `json:"gpus,omitempty"`
	Model          string               `json:"model,omitempty"`
	ListenAddr     string               `json:"listen_addr,omitempty"`
	PowerWatts     float64              `json:"power_watts,omitempty"`
	PowerAvailable bool                 `json:"power_available,omitempty"`
	PowerSource    string               `json:"power_source,omitempty"`
	Backends       []ClusterBackendInfo `json:"backends,omitempty"`
	TotalInFlight  int64                `json:"total_in_flight,omitempty"`
	PromptHistory  int                  `json:"prompt_history,omitempty"`
	EnergyKWh24h   float64              `json:"energy_kwh_24h,omitempty"`
	EnergyKWh30d   float64              `json:"energy_kwh_30d,omitempty"`
	HostMemTotalMB int64                `json:"host_mem_total_mb,omitempty"`
	HostMemUsedMB  int64                `json:"host_mem_used_mb,omitempty"`
	CostAvailable  bool                 `json:"cost_available,omitempty"`
	CostEURPerHour float64              `json:"cost_eur_per_hour,omitempty"`
	CostTodayEUR   float64              `json:"cost_today_eur,omitempty"`
}

// ClusterPeerInfo is one remote node as the serving node last saw it.
//
// Hostname is the hostname the peer *reports*, not one derived from the
// address it is dialled on. They disagree exactly where it matters:
// co-located instances are configured by IP, so deriving from the address
// splits one machine into two hosts on screen — and a per-host wattage that
// cannot be deduplicated. Deriving from the address is only a fallback for a
// peer too old to report a hostname.
//
// A peer that is unreachable carries Addr, Hostname and Status and nothing
// else; consumers must not read the absent fields as zeroes.
type ClusterPeerInfo struct {
	GPUs            []GPUInfo            `json:"gpus,omitempty"`
	Addr            string               `json:"addr"`
	Hostname        string               `json:"hostname,omitempty"`
	Status          string               `json:"status"`
	NodeID          string               `json:"node_id,omitempty"`
	Models          []string             `json:"models,omitempty"`
	Backends        []ClusterBackendInfo `json:"backends,omitempty"`
	TotalInFlight   int64                `json:"total_in_flight,omitempty"`
	HealthyBackends int                  `json:"healthy_backends,omitempty"`
	PromptHistory   int                  `json:"prompt_history,omitempty"`
	PowerWatts      float64              `json:"power_watts,omitempty"`
	PowerAvailable  bool                 `json:"power_available,omitempty"`
	PowerSource     string               `json:"power_source,omitempty"`
	EnergyKWh24h    float64              `json:"energy_kwh_24h,omitempty"`
	EnergyKWh30d    float64              `json:"energy_kwh_30d,omitempty"`
	HostMemTotalMB  int64                `json:"host_mem_total_mb,omitempty"`
	HostMemUsedMB   int64                `json:"host_mem_used_mb,omitempty"`
	CostAvailable   bool                 `json:"cost_available,omitempty"`
	CostEURPerHour  float64              `json:"cost_eur_per_hour,omitempty"`
	CostTodayEUR    float64              `json:"cost_today_eur,omitempty"`
	// Origin says how the serving node came to know this peer: "config" for
	// an address from peers.hosts, "learned" for one adopted by gossip.
	// Absent means a node too old to distinguish them — read that as
	// "config", which is what every pre-gossip node's peers were.
	Origin string `json:"origin,omitempty"`
}

// ClusterBackendInfo is BackendInfo as it appears inside a cluster snapshot.
// It is a separate type from BackendInfo because the mesh view groups by
// model, so Model is carried per backend rather than per node — a node serves
// one model today, but reading it off the backend keeps the grouping correct
// if that ever stops being true.
type ClusterBackendInfo struct {
	GPUID  int   `json:"gpu_id"`
	GPUIDs []int `json:"gpu_ids,omitempty"`
	// Model is per-backend rather than per-node because the mesh view groups by
	// it. A node serves one model today, but reading it off the backend keeps
	// the grouping correct if that ever stops being true.
	Model      string `json:"model,omitempty"`
	Status     string `json:"status"`
	InFlight   int64  `json:"in_flight"`
	RSSMB      int64  `json:"rss_mb,omitempty"`
	SlotCtx    int64  `json:"slot_ctx,omitempty"`
	SlotCount  int    `json:"slot_count,omitempty"`
	SlotActive int    `json:"slot_active,omitempty"`
	TokDecoded int64  `json:"tok_decoded,omitempty"`
	TokRemain  int64  `json:"tok_remain,omitempty"`
}
