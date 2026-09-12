package node

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

	"github.com/janit/viiwork/v2/internal/logging"
	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/meshapi"
)

// clusterPushInterval is how often the snapshot is rebuilt and compared. It is
// a server-side heartbeat shared by all viewers, not a per-client poll: at one
// second the view tracks in-flight changes closely while an idle mesh sends
// nothing at all, because identical snapshots are suppressed.
const clusterPushInterval = time.Second

// frameBuffer absorbs a burst of producer events while the writer is busy. It
// is backpressure, not a queue to be drained on shutdown: a producer that
// cannot enqueue blocks until it can, or until ctx ends.
const frameBuffer = 64

// sseAliases is the third named event on the mesh stream (P6 Decision 2),
// carrying a meshapi.AliasesResponse. The frozen meshapi names only the first
// two, so the name lives here.
const sseAliases = "aliases"

// sseFrame is one encoded event on its way to the single writer.
type sseFrame struct {
	event string
	data  []byte
}

// handleMeshStream serves everything the mesh view needs over ONE held-open
// connection: activity events from this node and every alive member, plus
// cluster and alias snapshots pushed when they change.
//
// Why SSE rather than WebSockets: viiwork is deliberately close to stdlib-only,
// and Go has no WebSocket in the standard library, so that would mean taking on
// a dependency for a strictly one-way feed. SSE is the same held-open TCP socket
// with server push, needs no handshake library, and reconnects on its own in
// the browser.
//
// Events are NAMED so one connection can carry every kind:
//
//	event: activity  -> a MeshEvent
//	event: cluster   -> a full ClusterResponse snapshot
//	event: aliases   -> an AliasesResponse
//
// Why aggregate server-side rather than let the page open one EventSource per
// host: the browser viewing /mesh may not be able to reach every member
// directly, and EventSource is subject to CORS, which the per-node activity
// endpoint does not set. Fanning out here keeps the mesh view working from any
// single reachable node.
func (s *server) handleMeshStream(w http.ResponseWriter, r *http.Request) {
	if s.d.Activity == nil {
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

	// Cancelling on the way out is what stops the producers below. They are
	// deliberately not waited for -- see the note on frames. The stream also
	// ends when the node shuts down (P6 Decision 7).
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stopOnShutdown := context.AfterFunc(s.d.StreamCtx, cancel)
	defer stopOnShutdown()

	// Every byte of this response is written by this goroutine and no other.
	//
	// The handler runs N+1 producers: one follower per member, plus the
	// snapshot loop. They used to write to w directly under a mutex, which made
	// those writes mutually exclusive but said nothing about when they *stop*.
	// The handler can return while a producer is still inside Fprintf, and
	// writing to an http.ResponseWriter after ServeHTTP returns is a data race
	// and a violation of net/http's contract. It reproduced under -race.
	//
	// Waiting for the producers instead would deadlock. An SSE response must not
	// carry a WriteTimeout, so a connected-but-stalled client can block a write
	// indefinitely -- and the handler would then be waiting on a producer that
	// cannot finish. That case is real rather than theoretical: the activity log
	// closes the oldest subscriber's channel when maxSubscribers is reached,
	// which returns this handler with the client still perfectly healthy.
	//
	// Funnelling through a channel removes the question instead of answering it.
	// A producer that has lost its reader is freed by ctx, and it never held a
	// reference to w to begin with.
	frames := make(chan sseFrame, frameBuffer)

	write := func(event string, data []byte) bool {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// enqueue is the producers' half of that split. It never touches w.
	enqueue := func(event string, payload any) bool {
		b, err := json.Marshal(payload)
		if err != nil {
			return true
		}
		select {
		case frames <- sseFrame{event: event, data: b}:
			return true
		case <-ctx.Done():
			return false
		}
	}

	localID, localHost := "", s.d.Self
	if s.d.Status != nil {
		localID = s.d.Status().NodeID
	}

	// Local events. Subscribe before replaying the backlog so nothing falls
	// between the two; the overlap that creates is deduplicated downstream.
	sub := s.d.Activity.Subscribe()
	defer s.d.Activity.Unsubscribe(sub)

	// Replaying on open is what makes a dropped connection recoverable. The
	// mesh view reconstructs in-flight requests from start/done pairs, so an
	// event lost while a browser is away strands a row that never leaves —
	// which is what a slept laptop or a throttled background tab produces.
	// Member backlogs arrive on their own: each member's /v1/activity/stream
	// replays too, and this handler opens fresh member connections per client.
	for _, ev := range s.d.Activity.Backlog() {
		b, err := json.Marshal(meshapi.MeshEvent{Event: ev, NodeID: localID, Hostname: localHost})
		if err != nil {
			continue
		}
		if !write(meshapi.SSEActivity, b) {
			return
		}
	}

	// Snapshots and member followers. Snapshots are pushed on change rather
	// than on a timer the client drives, so the browser never polls. The diff
	// matters: in-flight counts and GPU load change constantly, but re-sending
	// an identical snapshot every tick would be the same waste as polling, just
	// moved server-side. The same tick picks up members that joined and stops
	// following members that are gone (P6 Decision 8).
	go func() {
		followers := newMemberFollowers(ctx, enqueue)
		var lastCluster, lastAliases []byte
		deadband := newHostMemDeadband()
		ticker := time.NewTicker(clusterPushInterval)
		defer ticker.Stop()
		for {
			state := s.d.Cluster()
			followers.update(s.members(), nodeIDs(state))
			deadband.apply(&state)
			if b, err := json.Marshal(state); err == nil && !bytes.Equal(b, lastCluster) {
				lastCluster = b
				if !enqueue(meshapi.SSECluster, state) {
					return
				}
			}
			if s.d.AliasInfo != nil {
				aliases := s.d.AliasInfo()
				if b, err := json.Marshal(aliases); err == nil && !bytes.Equal(b, lastAliases) {
					lastAliases = b
					if !enqueue(sseAliases, aliases) {
						return
					}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case f := <-frames:
			if !write(f.event, f.data) {
				return
			}
		case raw, ok := <-sub:
			if !ok {
				// The log evicted this subscriber to make room for a newer one.
				// The connection is still healthy; there is simply nothing more
				// to send on it.
				return
			}
			// Subscribe delivers already-encoded JSON, so it has to be decoded
			// to re-tag it with the node. Activity events are low-rate (backend
			// state changes and request start/finish), so this is not a hot path.
			var ev meshapi.Event
			if err := json.Unmarshal(raw, &ev); err != nil {
				continue
			}
			b, err := json.Marshal(meshapi.MeshEvent{Event: ev, NodeID: localID, Hostname: localHost})
			if err != nil {
				continue
			}
			if !write(meshapi.SSEActivity, b) {
				return
			}
		}
	}
}

func (s *server) members() []mesh.Member {
	if s.d.Members == nil {
		return nil
	}
	return s.d.Members()
}

// nodeIDs maps member names to the node_id in their last status.
func nodeIDs(c meshapi.ClusterResponse) map[string]string {
	ids := make(map[string]string, len(c.Members))
	for _, m := range c.Members {
		if m.Status != nil {
			ids[m.Node] = m.Status.NodeID
		}
	}
	return ids
}

// memberFollowers runs one activity follower per alive node member.
type memberFollowers struct {
	ctx     context.Context
	enqueue func(event string, payload any) bool

	mu      sync.Mutex
	ids     map[string]string // member name -> node_id, refreshed each tick
	running map[string]followerHandle
}

type followerHandle struct {
	addr   string
	cancel context.CancelFunc
}

func newMemberFollowers(ctx context.Context, enqueue func(string, any) bool) *memberFollowers {
	return &memberFollowers{ctx: ctx, enqueue: enqueue, ids: map[string]string{}, running: map[string]followerHandle{}}
}

// update starts followers for alive, non-local node members, and stops those
// whose member is gone or has moved to another address.
func (f *memberFollowers) update(members []mesh.Member, ids map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ids = ids
	want := map[string]string{}
	for _, m := range members {
		if m.State == meshapi.MemberAlive && !m.Local && m.Meta.Role == meshapi.RoleNode {
			want[m.Name] = m.APIAddr()
		}
	}
	for name, h := range f.running {
		if addr, ok := want[name]; !ok || addr != h.addr {
			h.cancel()
			delete(f.running, name)
		}
	}
	for name, addr := range want {
		if _, ok := f.running[name]; ok {
			continue
		}
		ctx, cancel := context.WithCancel(f.ctx)
		f.running[name] = followerHandle{addr: addr, cancel: cancel}
		go f.follow(ctx, name, addr)
	}
}

func (f *memberFollowers) nodeID(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ids[name]
}

// follow streams one member's activity until ctx ends, reconnecting with
// backoff, so a member that is down or restarting does not take the mesh view
// down with it.
func (f *memberFollowers) follow(ctx context.Context, name, addr string) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if alive := f.streamOnce(ctx, name, addr); !alive {
			return // the client went away; stop bothering the member
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

// streamOnce connects once and pumps events. It returns false only when the
// downstream client is gone, which is the signal to stop retrying entirely.
func (f *memberFollowers) streamOnce(ctx context.Context, name, addr string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+meshapi.PathActivityStream, nil)
	if err != nil {
		return true
	}
	// No client-side timeout: this is a long-lived stream. ctx cancellation is
	// what ends it.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if logging.DebugEnabled() {
			log.Printf("[debug] mesh activity: member %s (%s) unreachable: %v", name, addr, err)
		}
		return true
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return true
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev meshapi.Event
		if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
			continue
		}
		if !f.enqueue(meshapi.SSEActivity, meshapi.MeshEvent{Event: ev, NodeID: f.nodeID(name), Hostname: name, Addr: addr}) {
			return false
		}
	}
	return true
}

// hostMemBuckets is how many distinct levels of host memory survive into the
// pushed snapshot. 64 puts a step at ~1 GB on a 64 GB host and ~2 GB on a
// 128 GB one, which is under a pixel on the strip that renders it.
const hostMemBuckets = 64

// hostMemDeadband coarsens host memory before the snapshot is diffed.
//
// An exact figure ticks every second on a live host (measured on gb1: ~86 MB
// of movement per second, spiking past 600 MB) and defeats the change detection
// in the push loop -- the stream would send a full snapshot every second
// forever, every one differing only in host_mem_used_mb. The mesh view's
// per-host memory strip needs about a hundred levels, not a megabyte, so the
// field is made exactly as precise as the only thing that reads it.
//
// Rounding alone is not enough: a host sitting near a bucket boundary flips
// between two levels on every tick and pushes just as hard as before. So the
// published value is held until the reading drifts a full step away from it.
// That is a deadband, and it is what makes the suppression hold for a host
// whose memory hovers rather than moves.
//
// State is per stream, keyed by member name, and /v1/cluster keeps the exact
// figures for anything that needs them.
type hostMemDeadband struct{ published map[string]int64 }

func newHostMemDeadband() *hostMemDeadband {
	return &hostMemDeadband{published: make(map[string]int64)}
}

func (d *hostMemDeadband) apply(state *meshapi.ClusterResponse) {
	for i := range state.Members {
		m := &state.Members[i]
		if m.Status == nil {
			continue
		}
		st := *m.Status // the caller's status is not changed
		st.HostMemUsedMB = d.value(m.Node, st.HostMemUsedMB, st.HostMemTotalMB)
		m.Status = &st
	}
}

func (d *hostMemDeadband) value(key string, usedMB, totalMB int64) int64 {
	// A node that reports no total -- an unreadable /proc/meminfo -- has
	// nothing to scale a step from, so it passes through.
	if usedMB <= 0 || totalMB <= 0 {
		return usedMB
	}
	step := totalMB / hostMemBuckets
	if step < 1 {
		return usedMB
	}
	if prev, ok := d.published[key]; ok && absInt64(usedMB-prev) < step {
		return prev
	}
	v := (usedMB + step/2) / step * step
	d.published[key] = v
	return v
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
