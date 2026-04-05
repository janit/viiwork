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
	Type      string `json:"type"`    // "backend", "request", "system"
	Message   string `json:"message"`
	GPUID     int    `json:"gpu_id,omitempty"`
	RequestID int64  `json:"rid,omitempty"`
}

const maxEvents = 200
const maxSubscribers = 16

type Log struct {
	mu          sync.Mutex
	events      []Event
	subscribers map[chan []byte]struct{}
}

func NewLog() *Log {
	return &Log{
		events:      make([]Event, 0, maxEvents),
		subscribers: make(map[chan []byte]struct{}),
	}
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
	l.emit(Event{
		Time:      time.Now().Unix(),
		Type:      "request",
		Message:   fmt.Sprintf(format, args...),
		GPUID:     gpuID,
		RequestID: rid,
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
