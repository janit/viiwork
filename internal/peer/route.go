// internal/peer/route.go
package peer

import (
	"errors"

	"github.com/janit/viiwork/internal/balancer"
)

const (
	RouteLocal = "local"
	RoutePeer  = "peer"
)

var ErrNoRoute = errors.New("no route available for model")

type Route struct {
	Type     string
	Backend  *balancer.BackendState // non-nil for local
	Addr     string                 // peer address for remote
	InFlight int64
}

// PickRoute selects the best route using adaptive logic:
// - Filter out local routes at capacity (InFlight >= maxInFlightPerGPU)
// - Among available routes, pick least loaded
// - Prefer local over peer at equal load
// - Among locals at equal load, prefer lower latency
func PickRoute(routes []Route, maxInFlightPerGPU int) (*Route, error) {
	if len(routes) == 0 {
		return nil, ErrNoRoute
	}

	maxLocal := int64(maxInFlightPerGPU)

	// Filter to routes with capacity
	var available []Route
	for _, r := range routes {
		if r.Type == RouteLocal && r.InFlight >= maxLocal {
			continue
		}
		available = append(available, r)
	}
	if len(available) == 0 {
		return nil, balancer.ErrBackpressure
	}

	// Pick least loaded
	best := &available[0]
	for i := 1; i < len(available); i++ {
		r := &available[i]
		if r.InFlight < best.InFlight {
			best = r
		} else if r.InFlight == best.InFlight {
			// Prefer local over peer at equal load
			if r.Type == RouteLocal && best.Type == RoutePeer {
				best = r
			}
			// Among locals, prefer lower latency
			if r.Type == RouteLocal && best.Type == RouteLocal &&
				r.Backend != nil && best.Backend != nil &&
				r.Backend.LatencyAvg() < best.Backend.LatencyAvg() {
				best = r
			}
		}
	}
	return best, nil
}
