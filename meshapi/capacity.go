package meshapi

// CapacityResponse is the compact per-model occupancy every member polls
// once a second on PathCapacity.
type CapacityResponse struct {
	Node   string          `json:"node"`
	Ver    string          `json:"ver"`
	Models []ModelCapacity `json:"models"`
}

// ModelCapacity is one model's occupancy on one node. Slots counts healthy
// backends only; Busy is per backend max(engine busy, node in-flight), summed.
type ModelCapacity struct {
	Name            string `json:"name"`
	Engine          string `json:"engine"`
	Slots           int    `json:"slots"`
	Busy            int    `json:"busy"`
	Queued          int    `json:"queued"`
	Ctx             int64  `json:"ctx"`
	Backends        int    `json:"backends"`
	HealthyBackends int    `json:"healthy_backends"`
}

// Free is the number of free slots, never negative.
func (m ModelCapacity) Free() int {
	if f := m.Slots - m.Busy; f > 0 {
		return f
	}
	return 0
}
