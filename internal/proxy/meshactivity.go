package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/janit/viiwork/internal/activity"
	"github.com/janit/viiwork/internal/logging"
	"github.com/janit/viiwork/internal/peer"
)

// clusterPushInterval is how often the snapshot is rebuilt and compared. It is
// a server-side heartbeat shared by all viewers, not a per-client poll: at one
// second the view tracks in-flight changes closely while an idle mesh sends
// nothing at all, because identical snapshots are suppressed.
const clusterPushInterval = time.Second

// MeshEvent is an activity event tagged with the node it came from.
//
// The per-node dashboard can render bare activity.Event because everything on
// screen belongs to one host. The mesh view cannot: two nodes both reporting
// "gpu-0" are different GPUs, and an in-flight request has to be attributed to
// a host before it means anything.
type MeshEvent struct {
	activity.Event
	NodeID   string `json:"node_id,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Addr     string `json:"addr,omitempty"`
}

// handleMeshStream serves everything the mesh view needs over ONE held-open
// connection: activity events from this node and every reachable peer, plus
// cluster snapshots pushed when they change.
//
// Why SSE rather than WebSockets: viiwork is deliberately stdlib-only (its one
// dependency is yaml.v3), and Go has no WebSocket in the standard library, so
// that would mean taking on gorilla/websocket for a strictly one-way feed. SSE
// is the same held-open TCP socket with server push, needs no handshake
// library, and reconnects on its own in the browser. Nothing here flows
// client-to-server, so the extra machinery would buy nothing.
//
// Events are NAMED so one connection can carry both kinds:
//   event: activity  -> a MeshEvent
//   event: cluster   -> a full peer.ClusterResponse snapshot
//
// Why aggregate server-side rather than let the page open one EventSource per
// host: the browser viewing /mesh may not be able to reach every peer directly
// (peers are addressed by LAN IP, and the viewer may be on a different network
// or tunnelled to one node), and EventSource is subject to CORS, which the
// per-node activity endpoint does not set. Fanning out here keeps the mesh view
// working from any single reachable node, which is the property the mesh design
// is built around.
func (h *Handler) handleMeshStream(w http.ResponseWriter, r *http.Request) {
	if h.activity == nil {
		http.Error(w, "activity log unavailable", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()

	// out serialises writes from the local subscriber and every peer reader.
	// http.ResponseWriter is not safe for concurrent use, and this handler has
	// N+1 goroutines writing into it.
	var mu sync.Mutex
	send := func(event string, payload any) bool {
		b, err := json.Marshal(payload)
		if err != nil {
			return true
		}
		mu.Lock()
		defer mu.Unlock()
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	emit := func(ev MeshEvent) bool { return send("activity", ev) }

	localID, localHost := "", ""
	if h.registry != nil {
		localID = h.registry.NodeID()
		localHost = h.registry.Hostname()
	}

	// Local events. Subscribe before replaying the backlog so nothing falls
	// between the two; the overlap that creates is deduplicated downstream.
	sub := h.activity.Subscribe()
	defer h.activity.Unsubscribe(sub)

	// Replaying on open is what makes a dropped connection recoverable. The
	// mesh view reconstructs in-flight requests from start/done pairs, so an
	// event lost while a browser is away strands a row that never leaves —
	// which is what a slept laptop or a throttled background tab produces.
	// Peer backlogs arrive on their own: each peer's /v1/activity/stream
	// replays too, and this handler opens fresh peer connections per client.
	for _, ev := range h.activity.Backlog() {
		if !send("activity", MeshEvent{Event: ev, NodeID: localID, Hostname: localHost}) {
			return
		}
	}

	// Peer events. Each peer gets a goroutine that follows its activity stream
	// and reconnects with backoff, so a peer that is down or restarting does not
	// take the mesh view down with it.
	if h.registry != nil {
		for _, p := range h.registry.Peers() {
			go followPeerActivity(ctx, p, emit)
		}
	}

	// Cluster snapshots. Pushed on change rather than on a timer the client
	// drives, so the browser never polls. The diff matters: in-flight counts and
	// GPU load change constantly, but re-sending an identical snapshot every
	// tick would be the same waste as polling, just moved server-side.
	if h.registry != nil {
		go func() {
			var last []byte
			deadband := newHostMemDeadband()
			ticker := time.NewTicker(clusterPushInterval)
			defer ticker.Stop()
			for {
				state := BuildClusterState(h.registry)
				deadband.apply(&state)
				if b, err := json.Marshal(state); err == nil && !bytes.Equal(b, last) {
					last = b
					if !send("cluster", state) {
						return
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case raw, ok := <-sub:
			if !ok {
				return
			}
			// Subscribe delivers already-encoded JSON, so it has to be decoded
			// to re-tag it with the node. Activity events are low-rate (backend
			// state changes and request start/finish), so this is not a hot path.
			var ev activity.Event
			if err := json.Unmarshal(raw, &ev); err != nil {
				continue
			}
			if !emit(MeshEvent{Event: ev, NodeID: localID, Hostname: localHost}) {
				return
			}
		}
	}
}

// followPeerActivity streams one peer's activity into emit until ctx ends.
func followPeerActivity(ctx context.Context, p *peer.PeerState, emit func(MeshEvent) bool) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if alive := streamOnePeer(ctx, p, emit); !alive {
			return // client went away; stop bothering the peer
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

// streamOnePeer connects once and pumps events. It returns false only when the
// downstream client is gone, which is the signal to stop retrying entirely.
func streamOnePeer(ctx context.Context, p *peer.PeerState, emit func(MeshEvent) bool) bool {
	url := "http://" + p.Addr + "/v1/activity/stream"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return true
	}
	// No client-side timeout: this is a long-lived stream. ctx cancellation is
	// what ends it.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if logging.DebugEnabled() {
			log.Printf("[debug] mesh activity: peer %s unreachable: %v", p.Addr, err)
		}
		return true
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return true
	}

	hostname := p.Hostname()
	if hostname == "" {
		hostname = hostOnly(p.Addr)
	}
	nodeID := p.NodeID()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev activity.Event
		if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
			continue
		}
		if !emit(MeshEvent{Event: ev, NodeID: nodeID, Hostname: hostname, Addr: p.Addr}) {
			return false
		}
	}
	return true
}

// hostOnly strips the port from a host:port address.
func hostOnly(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}

// hostMemBuckets is how many distinct levels of host memory survive into the
// pushed snapshot. 64 puts a step at ~1 GB on a 64 GB host and ~2 GB on a
// 128 GB one, which is under a pixel on the strip that renders it.
const hostMemBuckets = 64

// hostMemDeadband coarsens host memory before the snapshot is diffed.
//
// This field used to be stripped outright, because it ticks every second on a
// live host (measured on gb1: ~86 MB of movement per second, spiking past
// 600 MB) and an exact value defeats the change detection in the push loop --
// the stream then sends a full snapshot every second forever, every one of
// them differing only in host_mem_used_mb. Stripping was the right call while
// nothing rendered it. Now the mesh view draws a per-host memory strip, which
// needs about a hundred levels, not a megabyte, so the field is made exactly
// as precise as the only thing that reads it.
//
// Rounding alone is not enough: a host sitting near a bucket boundary flips
// between two levels on every tick and pushes just as hard as before. So the
// published value is held until the reading drifts a full step away from it.
// That is a deadband, and it is what makes the suppression hold for a host
// whose memory hovers rather than moves.
//
// State is per stream, and /v1/cluster keeps the exact figures for anything
// that needs them.
type hostMemDeadband struct{ published map[string]int64 }

func newHostMemDeadband() *hostMemDeadband {
	return &hostMemDeadband{published: make(map[string]int64)}
}

func (d *hostMemDeadband) apply(state *peer.ClusterResponse) {
	state.Local.HostMemUsedMB = d.value("\x00local", state.Local.HostMemUsedMB, state.Local.HostMemTotalMB)
	for i := range state.Peers {
		p := &state.Peers[i]
		p.HostMemUsedMB = d.value(p.Addr, p.HostMemUsedMB, p.HostMemTotalMB)
	}
}

func (d *hostMemDeadband) value(key string, usedMB, totalMB int64) int64 {
	// A node that reports no total -- an older peer, or an unreadable
	// /proc/meminfo -- has nothing to scale a step from, so it passes through.
	if usedMB <= 0 || totalMB <= 0 { return usedMB }
	step := totalMB / hostMemBuckets
	if step < 1 { return usedMB }
	if prev, ok := d.published[key]; ok && absInt64(usedMB-prev) < step {
		return prev
	}
	v := (usedMB + step/2) / step * step
	d.published[key] = v
	return v
}

func absInt64(v int64) int64 {
	if v < 0 { return -v }
	return v
}
