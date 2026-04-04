package gpu

type GPUSample struct {
	GPUID       int     `json:"gpu_id"`
	Utilization float64 `json:"util"`
	VRAMUsedMB  float64 `json:"vram_used_mb"`
	VRAMTotalMB float64 `json:"vram_total_mb"`
	Timestamp   int64   `json:"t"`
}
