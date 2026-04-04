package cost

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"time"
)

type PricePoint struct {
	Time      time.Time
	CentsKWh float64
}

type marketDocument struct {
	XMLName    xml.Name     `xml:"Publication_MarketDocument"`
	TimeSeries []timeSeries `xml:"TimeSeries"`
}

type timeSeries struct {
	Period []period `xml:"Period"`
}

type period struct {
	TimeInterval timeInterval `xml:"timeInterval"`
	Resolution   string       `xml:"resolution"`
	Points       []point      `xml:"Point"`
}

type timeInterval struct {
	Start string `xml:"start"`
	End   string `xml:"end"`
}

type point struct {
	Position int
	Price    float64
}

func (p *point) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	for {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			var val string
			if err := d.DecodeElement(&val, &t); err != nil {
				return err
			}
			switch t.Name.Local {
			case "position":
				p.Position, _ = strconv.Atoi(val)
			case "price.amount":
				p.Price, _ = strconv.ParseFloat(val, 64)
			}
		case xml.EndElement:
			if t.Name.Local == start.Name.Local {
				return nil
			}
		}
	}
}

func parseResolution(res string) (time.Duration, error) {
	switch res {
	case "PT60M", "PT1H":
		return time.Hour, nil
	case "PT15M":
		return 15 * time.Minute, nil
	case "PT30M":
		return 30 * time.Minute, nil
	default:
		return 0, fmt.Errorf("unsupported resolution: %s", res)
	}
}

func ParsePrices(data []byte) ([]PricePoint, error) {
	var doc marketDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing ENTSO-E XML: %w", err)
	}
	var prices []PricePoint
	for _, ts := range doc.TimeSeries {
		for _, p := range ts.Period {
			start, err := time.Parse(time.RFC3339, p.TimeInterval.Start)
			if err != nil {
				start, err = time.Parse("2006-01-02T15:04Z", p.TimeInterval.Start)
				if err != nil {
					continue
				}
			}
			res, err := parseResolution(p.Resolution)
			if err != nil {
				continue
			}
			for _, pt := range p.Points {
				t := start.Add(time.Duration(pt.Position-1) * res)
				prices = append(prices, PricePoint{Time: t, CentsKWh: pt.Price / 10})
			}
		}
	}
	return prices, nil
}
