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

// localRRIdx round-robins among LOCAL backends tied at equal in-flight, for the
// same reason as peers: route.InFlight is a snapshot taken when routes are
// built, and IncrInFlight runs later in the proxy. Phase-locked concurrent
// callers (a fixed-concurrency generator with ~equal generation times) all read
// the same all-zero snapshot. Resolving the tie by lowest-latency-first is
// deterministic, so every racing request lands on the same pair while the
// others idle. Atomic rotation fans them out across the tied pairs instead.
var localRRIdx atomic.Uint64

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

	// Among routes tied at minIF: prefer local over peer. Collect tied locals
	// and tied peers separately so each class round-robins fairly (see
	// localRRIdx / peerRRIdx). Latency is no longer the local tiebreak: with
	// identical pairs it never exactly ties, so it deterministically pinned
	// every racing request to one pair. InFlight-based steering at the minIF
	// filter already routes away from a genuinely slower (longer-held) pair.
	var tiedLocals, tiedPeers []*Route
	for _, r := range available {
		if r.InFlight != minIF {
			continue
		}
		if r.Type == RouteLocal {
			tiedLocals = append(tiedLocals, r)
		} else {
			tiedPeers = append(tiedPeers, r)
		}
	}

	if len(tiedLocals) == 1 {
		return tiedLocals[0], nil
	}
	if len(tiedLocals) > 1 {
		// Round-robin among tied locals. Atomic increment guarantees fair
		// rotation across concurrent callers without a mutex.
		idx := localRRIdx.Add(1) - 1
		return tiedLocals[int(idx%uint64(len(tiedLocals)))], nil
	}
	if len(tiedPeers) == 1 {
		return tiedPeers[0], nil
	}
	// Round-robin among tied peers. Atomic increment guarantees fair rotation
	// across concurrent callers without a mutex.
	idx := peerRRIdx.Add(1) - 1
	return tiedPeers[int(idx%uint64(len(tiedPeers)))], nil
}
