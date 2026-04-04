package cost

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

const minRefetchInterval = 5 * time.Minute

type SpotFetcher struct {
	apiKey  string
	zone    string
	baseURL string
	logger  *log.Logger
	client  *http.Client

	mu          sync.Mutex
	prices      []PricePoint
	lastFetch   time.Time
	lastAttempt time.Time
}

func NewSpotFetcher(apiKey string, zone string, baseURL string) *SpotFetcher {
	return &SpotFetcher{
		apiKey:  apiKey,
		zone:    zone,
		baseURL: baseURL,
		logger:  log.New(os.Stdout, "[spot] ", log.LstdFlags),
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (f *SpotFetcher) Fetch(ctx context.Context) {
	if f.client == nil {
		return
	}
	f.mu.Lock()
	if time.Since(f.lastAttempt) < minRefetchInterval {
		f.mu.Unlock()
		return
	}
	f.lastAttempt = time.Now()
	f.mu.Unlock()

	now := time.Now().UTC()
	start := now.Truncate(24 * time.Hour)
	end := start.Add(48 * time.Hour)

	params := url.Values{}
	params.Set("securityToken", f.apiKey)
	params.Set("documentType", "A44")
	params.Set("contract_MarketAgreement.type", "A01")
	params.Set("in_Domain", f.zone)
	params.Set("out_Domain", f.zone)
	params.Set("periodStart", start.Format("200601021504"))
	params.Set("periodEnd", end.Format("200601021504"))

	reqURL := f.baseURL + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		f.logger.Printf("failed to create request: %v", err)
		return
	}

	resp, err := f.client.Do(req)
	if err != nil {
		f.logger.Printf("ENTSO-E fetch failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		f.logger.Printf("ENTSO-E returned %d: %s", resp.StatusCode, string(body[:min(len(body), 200)]))
		return
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		f.logger.Printf("failed to read response: %v", err)
		return
	}

	prices, err := ParsePrices(data)
	if err != nil {
		f.logger.Printf("failed to parse prices: %v", err)
		return
	}

	f.mu.Lock()
	f.prices = prices
	f.lastFetch = time.Now()
	f.mu.Unlock()

	f.logger.Printf("fetched %d price points", len(prices))
}

func (f *SpotFetcher) PriceAt(t time.Time) (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.priceAtLocked(t)
}

func (f *SpotFetcher) NeedsFetch(t time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.priceAtLocked(t)
	return !ok
}

func (f *SpotFetcher) priceAtLocked(t time.Time) (float64, bool) {
	t = t.UTC()
	for i, p := range f.prices {
		var next time.Time
		if i+1 < len(f.prices) {
			next = f.prices[i+1].Time
		} else {
			next = p.Time.Add(time.Hour)
		}
		if !t.Before(p.Time) && t.Before(next) {
			return p.CentsKWh, true
		}
	}
	return 0, false
}
