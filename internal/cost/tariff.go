package cost

import "time"

type CostBreakdown struct {
	SpotCentsKWh     float64 `json:"spot_cents_kwh"`
	TransferCentsKWh float64 `json:"transfer_cents_kwh"`
	TaxCentsKWh      float64 `json:"tax_cents_kwh"`
	VATPercent       float64 `json:"vat_percent"`
	TotalCentsKWh    float64 `json:"total_cents_kwh"`
	CostEURPerHour   float64 `json:"cost_eur_per_hour"`
}

// CostConfig is defined in the cost package to avoid import cycles with internal/config.
// The config package has its own CostConfig with yaml tags; mapping happens in main.go.
type CostConfig struct {
	Transfer               TransferConfig
	ElectricityTaxCentsKWh float64
	VATPercent             float64
	Timezone               string
}

type TransferConfig struct {
	Winter WinterTransferConfig
	Summer SummerTransferConfig
}

type WinterTransferConfig struct {
	PeakCentsKWh    float64
	OffpeakCentsKWh float64
}

type SummerTransferConfig struct {
	FlatCentsKWh float64
}

func isWinter(month time.Month) bool {
	return month >= time.November || month <= time.March
}

func isOffPeak(t time.Time) bool {
	if t.Weekday() == time.Sunday { return true }
	hour := t.Hour()
	return hour < 7 || hour >= 22
}

func ResolveTransfer(t time.Time, cfg CostConfig) float64 {
	if !isWinter(t.Month()) { return cfg.Transfer.Summer.FlatCentsKWh }
	if isOffPeak(t) { return cfg.Transfer.Winter.OffpeakCentsKWh }
	return cfg.Transfer.Winter.PeakCentsKWh
}

func Calculate(spot float64, watts float64, cfg CostConfig, t time.Time) CostBreakdown {
	transfer := ResolveTransfer(t, cfg)
	beforeVAT := spot + transfer + cfg.ElectricityTaxCentsKWh
	total := beforeVAT * (1 + cfg.VATPercent/100)
	costEUR := total / 100 * watts / 1000
	return CostBreakdown{
		SpotCentsKWh: spot, TransferCentsKWh: transfer, TaxCentsKWh: cfg.ElectricityTaxCentsKWh,
		VATPercent: cfg.VATPercent, TotalCentsKWh: total, CostEURPerHour: costEUR,
	}
}
