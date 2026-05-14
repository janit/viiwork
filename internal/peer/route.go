// internal/peer/route.go
package peer

import (
	"errors"
	"sync/atomic"

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
	Peer     *PeerState             // non-nil for remote; lets the caller record write-through in-flight
	InFlight int64
}

// peerRRIdx round-robins among peers tied at equal in-flight. Without this,
// the picker always returns whichever tied peer appears first in the routes
// slice, which deterministically pins burst traffic to one peer until its
// polled count finally moves.
var peerRRIdx atomic.Uint64

// PickRoute selects the best route using adaptive logic:
//   - Filter out local routes at capacity (InFlight >= maxInFlightPerGPU)
//   - Among routes tied at the lowest InFlight: prefer local, and among
//     locals prefer lower latency; among tied peers, round-robin.
func PickRoute(routes []Route, maxInFlightPerGPU int) (*Route, error) {
	if len(routes) == 0 {
		return nil, ErrNoRoute
	}

	maxLocal := int64(maxInFlightPerGPU)

	// Filter to routes with capacity and find the minimum in-flight in one pass.
	var available []*Route
	var minIF int64
	for i := range routes {
		r := &routes[i]
		if r.Type == RouteLocal && r.InFlight >= maxLocal {
			continue
		}
		if len(available) == 0 || r.InFlight < minIF {
			minIF = r.InFlight
		}
		available = append(available, r)
	}
	if len(available) == 0 {
		return nil, balancer.ErrBackpressure
	}

	// Among routes tied at minIF: prefer local (lower latency wins among
	// locals); otherwise collect tied peers for round-robin selection.
	var bestLocal *Route
	var tiedPeers []*Route
	for _, r := range available {
		if r.InFlight != minIF {
			continue
		}
		if r.Type == RouteLocal {
			if bestLocal == nil {
				bestLocal = r
				continue
			}
			if r.Backend != nil && bestLocal.Backend != nil &&
				r.Backend.LatencyAvg() < bestLocal.Backend.LatencyAvg() {
				bestLocal = r
			}
			continue
		}
		tiedPeers = append(tiedPeers, r)
	}

	if bestLocal != nil {
		return bestLocal, nil
	}
	if len(tiedPeers) == 1 {
		return tiedPeers[0], nil
	}
	// Round-robin among tied peers. Atomic increment guarantees fair rotation
	// across concurrent callers without a mutex.
	idx := peerRRIdx.Add(1) - 1
	return tiedPeers[int(idx%uint64(len(tiedPeers)))], nil
}
