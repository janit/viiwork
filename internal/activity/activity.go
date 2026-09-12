package activity

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

var nextRequestID atomic.Int64

// NewRequestID returns a unique ID for pairing request start/done events.
func NewRequestID() int64 {
	return nextRequestID.Add(1)
}

// Event is defined in meshapi, the public mesh protocol package, and aliased
// here so this package keeps its familiar name. The type travels on
// /v1/activity/stream and is the sole basis on which every dashboard in the
// fleet reconstructs in-flight requests, so its shape — and the grammar of its
// Message field — is wire contract, not a local logging concern.
type Event = meshapi.Event

// DefaultEventHistory is how many events the ring keeps when nothing is
// configured.
const DefaultEventHistory = 200

const maxSubscribers = 16

type Log struct {
	mu          sync.Mutex
	maxEvents   int
	events      []Event
	subscribers map[chan []byte]struct{}
	prompts     *PromptStore
}

// NewLog returns a log with the default prompt-history capacity.
func NewLog() *Log { return NewLogWithPromptHistory(DefaultPromptHistory) }

// NewLogWithPromptHistory returns a log whose prompt store holds promptHistory
// requests. Kept separate from NewLog so the many call sites that do not care
// stay unchanged.
func NewLogWithPromptHistory(promptHistory int) *Log {
	return NewLogWithHistory(promptHistory, DefaultEventHistory)
}

// NewLogWithHistory also sizes the event ring (C1 activity.event_history);
// eventHistory <= 0 means DefaultEventHistory. The ring is also how far back a
// reconnecting dashboard can replay, see Backlog.
func NewLogWithHistory(promptHistory, eventHistory int) *Log {
	if eventHistory <= 0 {
		eventHistory = DefaultEventHistory
	}
	return &Log{
		maxEvents:   eventHistory,
		events:      make([]Event, 0, min(eventHistory, DefaultEventHistory)),
		subscribers: make(map[chan []byte]struct{}),
		prompts:     NewPromptStore(promptHistory),
	}
}

// StorePrompt records the prompt text for a request alongside the activity
// log entry for it, so the mesh dashboard can fetch it on demand instead of
// carrying full prompt bodies on every SSE event.
func (l *Log) StorePrompt(rid int64, model, prompt string) {
	l.prompts.Store(rid, time.Now().Unix(), model, prompt)
}

// StoreOutput records the response text for a request once it has finished,
// against the same rid the prompt was stored under.
func (l *Log) StoreOutput(rid int64, model, output string, elapsedMS int64) {
	l.prompts.StoreOutput(rid, time.Now().Unix(), model, output, elapsedMS)
}

// PromptHistoryMax reports the prompt store's configured capacity.
func (l *Log) PromptHistoryMax() int { return l.prompts.Max() }

// GetPrompt looks up a previously stored prompt by request id.
func (l *Log) GetPrompt(rid int64) (PromptEntry, bool) {
	return l.prompts.Get(rid)
}

func (l *Log) Emit(typ string, gpuID int, format string, args ...any) {
	l.emit(Event{
		Time:    time.Now().Unix(),
		Type:    typ,
		Message: fmt.Sprintf(format, args...),
		GPUID:   gpuID,
	})
}

func (l *Log) EmitRequest(rid int64, gpuID int, format string, args ...any) {
	l.EmitRequestTask(rid, gpuID, "", format, args...)
}

func (l *Log) EmitRequestTask(rid int64, gpuID int, taskID string, format string, args ...any) {
	l.emit(Event{
		Time:      time.Now().Unix(),
		Type:      "request",
		Message:   fmt.Sprintf(format, args...),
		GPUID:     gpuID,
		RequestID: rid,
		TaskID:    taskID,
	})
}

func (l *Log) emit(ev Event) {
	// Marshalled before the lock: it is the expensive part and needs nothing
	// the lock protects.
	data, _ := json.Marshal(ev)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ev)
	if len(l.events) > l.maxEvents {
		kept := make([]Event, l.maxEvents)
		copy(kept, l.events[len(l.events)-l.maxEvents:])
		l.events = kept
	}
	// Sent while holding the lock, not to a snapshot taken under it and
	// released first. Subscribe closes the oldest subscriber's channel when it
	// is at capacity, and a send racing that close panics — "send on closed
	// channel", which a select's default case does not prevent. Holding the
	// lock makes closing and sending mutually exclusive. It cannot stall on a
	// slow client, because every send here is non-blocking.
	for ch := range l.subscribers {
		select {
		case ch <- data:
		default: // skip slow clients
		}
	}
}

// Backlog returns the ring marked as replay, for a stream that has just
// opened.
//
// This is what makes a dropped connection recoverable. A consumer
// reconstructing in-flight requests from start/done pairs loses the pairing for
// anything that completes while it is away — a laptop sleeping, a tab throttled
// in the background, a node restarting — and a start with no matching done
// strands a row that never leaves. Replaying the ring hands back both halves.
//
// The ring bounds how far back that works. A gap longer than the ring on a
// given node cannot be repaired from here, which is why a consumer should also
// treat a reconnect as a reason to rebuild rather than to carry state across.
func (l *Log) Backlog() []Event {
	out := l.Recent()
	for i := range out {
		out[i].Replay = true
	}
	return out
}

func (l *Log) Recent() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Event, len(l.events))
	copy(out, l.events)
	return out
}

func (l *Log) Subscribe() chan []byte {
	ch := make(chan []byte, 32)
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.subscribers) >= maxSubscribers {
		// Evict one to make room. Which one is arbitrary: subscribers are a
		// map and Go randomises its iteration order, so this is not "the
		// oldest" however much that would be nicer. It does not matter for
		// correctness — the evicted reader sees its channel close and returns,
		// and a dashboard reconnects — but it is worth not mistaking for an
		// ordering guarantee.
		for evict := range l.subscribers {
			delete(l.subscribers, evict)
			close(evict)
			break
		}
	}
	l.subscribers[ch] = struct{}{}
	return ch
}

func (l *Log) Unsubscribe(ch chan []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.subscribers, ch)
}
