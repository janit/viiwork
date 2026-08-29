package gpu

type GPUSample struct {
	GPUID       int     `json:"gpu_id"`
	Utilization float64 `json:"util"`
	VRAMUsedMB  float64 `json:"vram_used_mb"`
	VRAMTotalMB float64 `json:"vram_total_mb"`
	// PowerW is the GPU package power draw. omitempty because rocm-smi builds
	// that predate --showpower, or reject it, leave it unset rather than zero:
	// a real 0 W reading does not occur on a card that is powered on, so a
	// consumer can read absent as "not measured here".
	PowerW    float64 `json:"power_w,omitempty"`
	Timestamp int64   `json:"t"`
}
