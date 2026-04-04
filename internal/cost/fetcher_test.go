package cost

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSpotFetcherPriceAt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sampleXML)) // sampleXML from entsoe_test.go (same package)
	}))
	defer srv.Close()

	f := NewSpotFetcher("test-key", "10YFI-1--------U", srv.URL+"/api")
	f.Fetch(context.Background())

	// First price: 2025-01-14T23:00Z = 4.52 c/kWh
	price, ok := f.PriceAt(time.Date(2025, 1, 14, 23, 0, 0, 0, time.UTC))
	if !ok { t.Fatal("expected price to be available") }
	if !approx(price, 4.52) { t.Errorf("expected 4.52, got %f", price) }

	// Second hour: 2025-01-15T00:00Z = 4.21
	price, ok = f.PriceAt(time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC))
	if !ok { t.Fatal("expected price") }
	if !approx(price, 4.21) { t.Errorf("expected 4.21, got %f", price) }
}

func TestSpotFetcherPriceAtMissing(t *testing.T) {
	f := NewSpotFetcher("test-key", "10YFI-1--------U", "http://127.0.0.1:1/api")
	_, ok := f.PriceAt(time.Now())
	if ok { t.Error("expected no price when cache is empty") }
}

func TestSpotFetcherRefetchBackoff(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(500)
	}))
	defer srv.Close()

	f := NewSpotFetcher("test-key", "10YFI-1--------U", srv.URL+"/api")
	f.Fetch(context.Background())
	f.Fetch(context.Background()) // should be skipped (backoff)
	if calls != 1 { t.Errorf("expected 1 call (backoff), got %d", calls) }
}
