package cost

import (
	"context"
	"log"
	"os"
	"sync"
	"time"
)

type PowerReader interface {
	Watts() float64
	Available() bool
}

type Tracker struct {
	fetcher  *SpotFetcher
	cfg      CostConfig
	power    PowerReader
	location *time.Location
	logger   *log.Logger

	mu         sync.RWMutex
	available  bool
	breakdown  CostBreakdown
	todayEUR   float64
	lastUpdate time.Time
	lastDate   string // "2006-01-02" for midnight reset detection
}

func NewTracker(fetcher *SpotFetcher, cfg CostConfig, power PowerReader) *Tracker {
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil { loc = time.UTC }
	return &Tracker{
		fetcher: fetcher, cfg: cfg, power: power,
		location: loc, logger: log.New(os.Stdout, "[cost] ", log.LstdFlags),
	}
}

func (t *Tracker) Update(ctx context.Context) {
	now := time.Now()

	if t.fetcher.NeedsFetch(now.UTC()) {
		t.fetcher.Fetch(ctx)
	}

	spot, ok := t.fetcher.PriceAt(now.UTC())
	if !ok || !t.power.Available() {
		t.mu.Lock()
		t.available = false
		t.breakdown = CostBreakdown{}
		t.mu.Unlock()
		return
	}

	localNow := now.In(t.location)
	bd := Calculate(spot, t.power.Watts(), t.cfg, localNow)

	t.mu.Lock()
	defer t.mu.Unlock()

	today := localNow.Format("2006-01-02")
	if t.lastDate != "" && t.lastDate != today {
		t.logger.Printf("midnight reset: accumulated %.4f EUR for %s", t.todayEUR, t.lastDate)
		t.todayEUR = 0
	}
	t.lastDate = today

	if !t.lastUpdate.IsZero() {
		elapsed := now.Sub(t.lastUpdate).Seconds()
		t.todayEUR += bd.CostEURPerHour * elapsed / 3600
	}

	t.available = true
	t.breakdown = bd
	t.lastUpdate = now
}

func (t *Tracker) Available() bool {
	t.mu.RLock(); defer t.mu.RUnlock(); return t.available
}

func (t *Tracker) EURPerHour() float64 {
	t.mu.RLock(); defer t.mu.RUnlock(); return t.breakdown.CostEURPerHour
}

func (t *Tracker) TodayEUR() float64 {
	t.mu.RLock(); defer t.mu.RUnlock(); return t.todayEUR
}

// CostReader interface methods (individual field accessors for cross-package use)
func (t *Tracker) SpotCentsKWh() float64     { t.mu.RLock(); defer t.mu.RUnlock(); return t.breakdown.SpotCentsKWh }
func (t *Tracker) TransferCentsKWh() float64  { t.mu.RLock(); defer t.mu.RUnlock(); return t.breakdown.TransferCentsKWh }
func (t *Tracker) TaxCentsKWh() float64       { t.mu.RLock(); defer t.mu.RUnlock(); return t.breakdown.TaxCentsKWh }
func (t *Tracker) VATPercent() float64         { t.mu.RLock(); defer t.mu.RUnlock(); return t.breakdown.VATPercent }
func (t *Tracker) TotalCentsKWh() float64      { t.mu.RLock(); defer t.mu.RUnlock(); return t.breakdown.TotalCentsKWh }
