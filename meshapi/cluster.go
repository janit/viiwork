package meshapi

// Member roles (also used in mesh.NodeMeta).
const (
	RoleNode    = "node"
	RoleGateway = "gateway"
)

// Mesh modes, as reported in ClusterResponse.Mesh.
const (
	MeshSecured = "secured"
	MeshOpen    = "open"
)

// Member states, as memberlist reports them.
const (
	MemberAlive   = "alive"
	MemberSuspect = "suspect"
	MemberDead    = "dead"
	MemberLeft    = "left"
)

// ClusterResponse is the mesh as the serving node sees it, on PathCluster and
// as the SSECluster event.
type ClusterResponse struct {
	View    string   `json:"view"`
	Mesh    string   `json:"mesh"`
	Members []Member `json:"members"`
	Models  []string `json:"models"`
	// PowerControl is the chassis-control allowlist. It is the only place a
	// powered-off host's name still exists, so the dashboard needs it to
	// render a row for that host.
	PowerControl          *PowerControlInfo `json:"power_control,omitempty"`
	ClusterCostEURPerHour float64           `json:"cluster_cost_eur_per_hour,omitempty"`
	ClusterCostTodayEUR   float64           `json:"cluster_cost_today_eur,omitempty"`
}

// Member is one mesh member. Status is null when the member is not alive.
type Member struct {
	Node   string      `json:"node"`
	Addr   string      `json:"addr"`
	Role   string      `json:"role"`
	State  string      `json:"state"`
	Status *NodeStatus `json:"status"`
}

// PowerControlInfo is the chassis-control allowlist a node advertises.
// OutOfBand lists the hosts reachable while powered off.
type PowerControlInfo struct {
	Hosts     []string `json:"hosts"`
	OutOfBand []string `json:"out_of_band,omitempty"`
}
