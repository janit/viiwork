package activity

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

var nextRequestID atomic.Int64

// NewRequestID returns a unique ID for pairing request start/done events.
func NewRequestID() int64 {
	return nextRequestID.Add(1)
}

type Event struct {
	Time      int64  `json:"t"`
	Type      string `json:"type"` // "backend", "request", "system"
	Message   string `json:"message"`
	GPUID     int    `json:"gpu_id,omitempty"`
	RequestID int64  `json:"rid,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	// Replay marks an event delivered from the ring when a stream opens,
	// rather than as it happened. A consumer that reconstructs state from the
	// stream needs it: replayed events are the authoritative rebuild of that
	// state, while a consumer keeping a visible event list has to deduplicate
	// them against what it already shows.
	Replay bool `json:"replay,omitempty"`
}

const maxEvents = 200
const maxSubscribers = 16

type Log struct {
	mu          sync.Mutex
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
	return &Log{
		events:      make([]Event, 0, maxEvents),
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

	l.mu.Lock()
	l.events = append(l.events, ev)
	if len(l.events) > maxEvents {
		kept := make([]Event, maxEvents)
		copy(kept, l.events[len(l.events)-maxEvents:])
		l.events = kept
	}
	// Snapshot subscribers
	subs := make([]chan []byte, 0, len(l.subscribers))
	for ch := range l.subscribers {
		subs = append(subs, ch)
	}
	l.mu.Unlock()

	data, _ := json.Marshal(ev)
	for _, ch := range subs {
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
// The ring bounds how far back that works. A gap longer than maxEvents on a
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
		// Drop oldest subscriber
		for old := range l.subscribers {
			delete(l.subscribers, old)
			close(old)
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
