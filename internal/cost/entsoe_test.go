package cost

import (
	"testing"
	"time"
)

const sampleXML = `<?xml version="1.0" encoding="UTF-8"?>
<Publication_MarketDocument xmlns="urn:iec62325.351:tc57wg16:451-3:publicationdocument:7:3">
  <TimeSeries>
    <Period>
      <timeInterval>
        <start>2025-01-14T23:00Z</start>
        <end>2025-01-15T23:00Z</end>
      </timeInterval>
      <resolution>PT60M</resolution>
      <Point><position>1</position><price.amount>45.20</price.amount></Point>
      <Point><position>2</position><price.amount>42.10</price.amount></Point>
      <Point><position>3</position><price.amount>-1.50</price.amount></Point>
    </Period>
  </TimeSeries>
</Publication_MarketDocument>`

func TestParsePrices(t *testing.T) {
	prices, err := ParsePrices([]byte(sampleXML))
	if err != nil { t.Fatalf("parse error: %v", err) }
	if len(prices) != 3 { t.Fatalf("expected 3 prices, got %d", len(prices)) }
	if !prices[0].Time.Equal(time.Date(2025, 1, 14, 23, 0, 0, 0, time.UTC)) {
		t.Errorf("price[0] time: expected 2025-01-14T23:00Z, got %v", prices[0].Time)
	}
	if !approx(prices[0].CentsKWh, 4.52) { t.Errorf("price[0]: expected 4.52, got %f", prices[0].CentsKWh) }
	if !approx(prices[1].CentsKWh, 4.21) { t.Errorf("price[1]: expected 4.21, got %f", prices[1].CentsKWh) }
	if !approx(prices[2].CentsKWh, -0.15) { t.Errorf("price[2]: expected -0.15, got %f", prices[2].CentsKWh) }
}

func TestParsePricesEmpty(t *testing.T) {
	prices, err := ParsePrices([]byte(`<?xml version="1.0"?><Publication_MarketDocument xmlns="urn:iec62325.351:tc57wg16:451-3:publicationdocument:7:3"></Publication_MarketDocument>`))
	if err != nil { t.Fatalf("parse error: %v", err) }
	if len(prices) != 0 { t.Errorf("expected 0 prices, got %d", len(prices)) }
}

func TestParsePrices15Min(t *testing.T) {
	xml15 := `<?xml version="1.0" encoding="UTF-8"?>
<Publication_MarketDocument xmlns="urn:iec62325.351:tc57wg16:451-3:publicationdocument:7:3">
  <TimeSeries>
    <Period>
      <timeInterval>
        <start>2025-01-14T23:00Z</start>
        <end>2025-01-15T00:00Z</end>
      </timeInterval>
      <resolution>PT15M</resolution>
      <Point><position>1</position><price.amount>45.20</price.amount></Point>
      <Point><position>2</position><price.amount>46.00</price.amount></Point>
      <Point><position>3</position><price.amount>44.80</price.amount></Point>
      <Point><position>4</position><price.amount>43.50</price.amount></Point>
    </Period>
  </TimeSeries>
</Publication_MarketDocument>`
	prices, err := ParsePrices([]byte(xml15))
	if err != nil { t.Fatalf("parse error: %v", err) }
	if len(prices) != 4 { t.Fatalf("expected 4 prices, got %d", len(prices)) }
	if !prices[1].Time.Equal(time.Date(2025, 1, 14, 23, 15, 0, 0, time.UTC)) {
		t.Errorf("price[1] time: expected 23:15, got %v", prices[1].Time)
	}
}
