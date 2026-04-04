package cost

import (
	"math"
	"testing"
	"time"
)

var testCfg = CostConfig{
	Transfer: TransferConfig{
		Winter: WinterTransferConfig{PeakCentsKWh: 4.28, OffpeakCentsKWh: 2.49},
		Summer: SummerTransferConfig{FlatCentsKWh: 2.49},
	},
	ElectricityTaxCentsKWh: 2.253,
	VATPercent:             25.5,
	Timezone:               "Europe/Helsinki",
}

func helsinki(t *testing.T) *time.Location {
	loc, err := time.LoadLocation("Europe/Helsinki")
	if err != nil { t.Fatal(err) }
	return loc
}

func approx(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func TestResolveTransferWinterPeak(t *testing.T) {
	loc := helsinki(t)
	ts := time.Date(2025, 1, 14, 14, 0, 0, 0, loc)
	got := ResolveTransfer(ts, testCfg)
	if got != 4.28 { t.Errorf("expected 4.28, got %f", got) }
}

func TestResolveTransferWinterOffpeakNight(t *testing.T) {
	loc := helsinki(t)
	ts := time.Date(2025, 1, 14, 3, 0, 0, 0, loc)
	got := ResolveTransfer(ts, testCfg)
	if got != 2.49 { t.Errorf("expected 2.49, got %f", got) }
}

func TestResolveTransferWinterOffpeakSunday(t *testing.T) {
	loc := helsinki(t)
	ts := time.Date(2025, 1, 12, 14, 0, 0, 0, loc)
	got := ResolveTransfer(ts, testCfg)
	if got != 2.49 { t.Errorf("expected 2.49, got %f", got) }
}

func TestResolveTransferSummer(t *testing.T) {
	loc := helsinki(t)
	ts := time.Date(2025, 7, 16, 14, 0, 0, 0, loc)
	got := ResolveTransfer(ts, testCfg)
	if got != 2.49 { t.Errorf("expected 2.49, got %f", got) }
}

func TestResolveTransferOctoberIsSummer(t *testing.T) {
	loc := helsinki(t)
	ts := time.Date(2025, 10, 15, 14, 0, 0, 0, loc)
	got := ResolveTransfer(ts, testCfg)
	if got != 2.49 { t.Errorf("expected 2.49 (summer), got %f", got) }
}

func TestResolveTransferNovemberIsWinter(t *testing.T) {
	loc := helsinki(t)
	ts := time.Date(2025, 11, 5, 14, 0, 0, 0, loc)
	got := ResolveTransfer(ts, testCfg)
	if got != 4.28 { t.Errorf("expected 4.28 (winter peak), got %f", got) }
}

func TestResolveTransferSaturdayPeak(t *testing.T) {
	loc := helsinki(t)
	ts := time.Date(2025, 1, 11, 14, 0, 0, 0, loc)
	got := ResolveTransfer(ts, testCfg)
	if got != 4.28 { t.Errorf("expected 4.28 (Saturday peak), got %f", got) }
}

func TestCalculate(t *testing.T) {
	loc := helsinki(t)
	ts := time.Date(2025, 1, 14, 14, 0, 0, 0, loc)
	spot := 5.0
	watts := 280.0
	bd := Calculate(spot, watts, testCfg, ts)
	if bd.SpotCentsKWh != 5.0 { t.Errorf("spot: expected 5.0, got %f", bd.SpotCentsKWh) }
	if bd.TransferCentsKWh != 4.28 { t.Errorf("transfer: expected 4.28, got %f", bd.TransferCentsKWh) }
	if bd.TaxCentsKWh != 2.253 { t.Errorf("tax: expected 2.253, got %f", bd.TaxCentsKWh) }
	if !approx(bd.TotalCentsKWh, 14.47) { t.Errorf("total: expected ~14.47, got %f", bd.TotalCentsKWh) }
	if !approx(bd.CostEURPerHour, 0.0405) { t.Errorf("cost: expected ~0.0405, got %f", bd.CostEURPerHour) }
}

func TestCalculateNegativeSpot(t *testing.T) {
	loc := helsinki(t)
	ts := time.Date(2025, 7, 16, 3, 0, 0, 0, loc)
	spot := -2.0
	watts := 280.0
	bd := Calculate(spot, watts, testCfg, ts)
	if !approx(bd.TotalCentsKWh, 3.44) { t.Errorf("total: expected ~3.44, got %f", bd.TotalCentsKWh) }
}
