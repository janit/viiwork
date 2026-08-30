package meshapi

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Event types carried on PathActivityStream. A consumer dispatches on Type,
// and must ignore a type it does not recognise rather than treating it as a
// request — a newer node may emit kinds this build predates.
const (
	// EventBackend is a backend lifecycle change: spawned, healthy, respawned,
	// dead.
	EventBackend = "backend"
	// EventRequest is a request starting or finishing. These are the only
	// events a consumer reconstructs in-flight state from, and they are the
	// only ones that carry RequestID.
	EventRequest = "request"
	// EventSystem is node-level news: startup, shutdown, config adoption.
	EventSystem = "system"
)

// Event is one entry in a node's activity log, as it appears on
// PathActivity and PathActivityStream.
//
// There is no server-side registry of running jobs anywhere in the mesh: the
// dashboards reconstruct in-flight requests from these events alone, a start
// event adding and a terminal event removing, keyed by node and RequestID.
// That is why Message has a grammar (see RequestStarted and friends) rather
// than being free text, and why Replay exists.
type Event struct {
	Time    int64  `json:"t"`
	Type    string `json:"type"` // "backend", "request", "system"
	Message string `json:"message"`
	GPUID   int    `json:"gpu_id,omitempty"`
	// RequestID is a per-process counter, NOT cluster-wide. An id only means
	// anything on the node that minted it, which is why a prompt lookup has to
	// be addressed to that node — see PathMeshPrompt.
	RequestID int64  `json:"rid,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	// Replay marks an event delivered from the ring when a stream opens,
	// rather than as it happened. A consumer that reconstructs state from the
	// stream needs it: replayed events are the authoritative rebuild of that
	// state, while a consumer keeping a visible event list has to deduplicate
	// them against what it already shows.
	Replay bool `json:"replay,omitempty"`
}

// MeshEvent is an Event tagged with the node it came from, as delivered on the
// SSEActivity channel of PathMeshStream.
//
// Fan-out happens on the server: the node merges its own activity with every
// peer's before pushing. Doing it in the browser would fail, because the
// viewer may not be able to reach peers directly and EventSource is CORS-bound.
// One reachable node is therefore enough to watch the whole mesh.
type MeshEvent struct {
	Event
	NodeID   string `json:"node_id,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Addr     string `json:"addr,omitempty"`
}

// The request message grammar.
//
// A request event's Message is "<model> → <destination>" while the request is
// running, and the same string with a terminal suffix once it finishes. This
// is a wire format, not a log line: consumers split on the arrow to recover
// model and destination, and match the suffix to decide whether to clear the
// row. Change these and in-flight rows stop clearing on every dashboard in the
// fleet, including ones served by nodes you did not upgrade.
//
// Build them with the helpers below rather than by hand, so a node written
// against this package cannot drift from the reference implementation.
const (
	// arrowSep separates model from destination. A literal U+2192, and the
	// only arrow-like character consumers split on.
	arrowSep = " → "
	// doneSuffix and abortedSuffix are the terminal forms. Both contain a word
	// IsRequestTerminal matches; see its note on why the match is loose.
	doneSuffix    = " done (%s)"
	abortedSuffix = " aborted by client (%s)"
)

// BackendLabel names a local inference backend as the mesh refers to it:
// "gpu-3" for a single-GPU backend, "ts-4,5" for a tensor-split group.
//
// The tensor-split form exists because a group has no single GPU id. Labelling
// such a backend by its GPUID field alone rendered every backend in a
// multi-group fleet as "gpu--1", leaving no way to attribute a request to a
// backend from the client side — this label is also the X-GPU-Backend response
// header, not just a log string.
func BackendLabel(gpuID int, gpuIDs []int) string {
	if len(gpuIDs) > 0 {
		parts := make([]string, len(gpuIDs))
		for i, id := range gpuIDs {
			parts[i] = strconv.Itoa(id)
		}
		return "ts-" + strings.Join(parts, ",")
	}
	return "gpu-" + strconv.Itoa(gpuID)
}

// PeerLabel names a remote node as a request destination: "peer gb2:9302".
// A request routed to a peer is logged on both nodes — once here as a peer
// dispatch, and once on the peer itself against its own local backend.
func PeerLabel(addr string) string { return "peer " + addr }

// RequestStarted builds the Message for a request beginning on dest, where
// dest comes from BackendLabel or PeerLabel.
func RequestStarted(model, dest string) string {
	return model + arrowSep + dest
}

// RequestDone builds the Message for a request that completed normally.
//
// Round elapsed before passing it — the reference implementation rounds to the
// millisecond, and an unrounded Duration renders nine significant digits of
// noise into a column a human is meant to scan.
func RequestDone(model, dest string, elapsed time.Duration) string {
	return RequestStarted(model, dest) + fmt.Sprintf(doneSuffix, elapsed)
}

// RequestAborted builds the Message for a request whose client disconnected
// before it finished. Distinguished from RequestDone because an abort is the
// client's doing and says nothing about backend health.
func RequestAborted(model, dest string, elapsed time.Duration) string {
	return RequestStarted(model, dest) + fmt.Sprintf(abortedSuffix, elapsed)
}

// IsRequestTerminal reports whether a request event's Message marks the
// request as over, and so should clear the reconstructed in-flight row.
//
// The match is deliberately loose — any of "done", "aborted" or "finished"
// anywhere in the message. A consumer that anchored on an exact suffix would
// strand rows the moment a node added a terminal form it did not know about,
// and a stranded row ages in red forever with no way to clear it. Erring
// toward clearing can only under-report work in flight; erring the other way
// invents work that has already finished.
func IsRequestTerminal(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "done") ||
		strings.Contains(lower, "aborted") ||
		strings.Contains(lower, "finished")
}

// SplitRequestMessage recovers the model and destination from a request
// Message, including a terminal one. Destination keeps any terminal suffix
// stripped, so "m → gpu-1 done (1.2s)" yields ("m", "gpu-1").
//
// ok is false for a message with no arrow, which is any event this grammar
// does not cover — a consumer should show it verbatim rather than guessing.
func SplitRequestMessage(msg string) (model, dest string, ok bool) {
	i := strings.Index(msg, arrowSep)
	if i < 0 {
		return "", "", false
	}
	model = strings.TrimSpace(msg[:i])
	dest = strings.TrimSpace(msg[i+len(arrowSep):])
	// Trim the terminal suffix by cutting at its opening word. Both forms end
	// in " (<elapsed>)", so cutting at the first space that begins a known
	// terminal word leaves the destination intact.
	for _, w := range []string{" done (", " aborted by client (", " finished"} {
		if j := strings.Index(dest, w); j >= 0 {
			dest = dest[:j]
			break
		}
	}
	return model, dest, true
}
