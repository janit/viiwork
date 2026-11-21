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
	// Performance score (v2.7): this node's own measured time to first token
	// for the model, from local slot admission to the first content chunk.
	// TTFT ~ TTFTOverheadMs + PrefillMsPer1k * uncached_tokens/1000. Absent
	// on a node that cannot say (never measured, or an older version).
	TTFTOverheadMs int `json:"ttft_overhead_ms,omitempty"`
	PrefillMsPer1k int `json:"prefill_ms_per_1k,omitempty"`
	// PerfSamples is the samples in the current ten-minute window; 0 with the
	// two fields above means they are the saved baseline alone.
	PerfSamples int `json:"perf_samples,omitempty"`
	// Images (v2.10) is whether this node's backends of the model read
	// images: true when every healthy backend says it does, false when every
	// one says it does not. Absent when the engine cannot say, when the
	// backends disagree, when none is healthy, and on an older version.
	// Absent is "cannot say", never "no".
	Images *bool `json:"images,omitempty"`
	// KVShareGroup (v2.11) is the model's kv_share group on this node: the
	// members that replicate a session's engine state to each other for a
	// spill (PathKVSharePut). Absent when the node shares nothing for the
	// model, and on an older version.
	KVShareGroup string `json:"kv_share_group,omitempty"`
	// KVShareImportMsPer1k (v2.11) is this node's measured time to bring a
	// session replica into a backend, per 1000 tokens of conversation: what
	// a spilled turn pays here instead of its prefill. Absent until the node
	// has imported one, and on an older version.
	KVShareImportMsPer1k int `json:"kv_share_import_ms_per_1k,omitempty"`
	// Modality (v2.13) is the model's kind (modality.go): absent is text.
	// A non-text model is listed in the fleet view but never receives text
	// inference, and Slots, Busy and Ctx count its own sessions.
	Modality string `json:"modality,omitempty"`
}

// Free is the number of free slots, never negative.
func (m ModelCapacity) Free() int {
	if f := m.Slots - m.Busy; f > 0 {
		return f
	}
	return 0
}
