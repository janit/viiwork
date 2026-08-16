package gpu

import (
	"sort"
	"sync"
)

type RingBuffer struct {
	samples []GPUSample
	head    int
	count   int
	maxSize int
}

func newRingBuffer(maxSize int) *RingBuffer {
	return &RingBuffer{samples: make([]GPUSample, maxSize), maxSize: maxSize}
}

func (rb *RingBuffer) add(s GPUSample) {
	rb.samples[rb.head] = s
	rb.head = (rb.head + 1) % rb.maxSize
	if rb.count < rb.maxSize { rb.count++ }
}

func (rb *RingBuffer) slice() []GPUSample {
	if rb.count == 0 { return nil }
	out := make([]GPUSample, rb.count)
	if rb.count < rb.maxSize {
		copy(out, rb.samples[:rb.count])
	} else {
		start := rb.head
		n := copy(out, rb.samples[start:])
		copy(out[n:], rb.samples[:start])
	}
	return out
}

type History struct {
	mu      sync.RWMutex
	buffers map[int]*RingBuffer
	maxSize int
}

func NewHistory(maxSize int) *History {
	return &History{buffers: make(map[int]*RingBuffer), maxSize: maxSize}
}

func (h *History) Record(s GPUSample) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rb, ok := h.buffers[s.GPUID]
	if !ok { rb = newRingBuffer(h.maxSize); h.buffers[s.GPUID] = rb }
	rb.add(s)
}

func (h *History) Samples(gpuID int) []GPUSample {
	h.mu.RLock()
	defer h.mu.RUnlock()
	rb, ok := h.buffers[gpuID]
	if !ok { return nil }
	return rb.slice()
}

func (h *History) AllGPUSamples() map[int][]GPUSample {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[int][]GPUSample, len(h.buffers))
	for id, rb := range h.buffers { out[id] = rb.slice() }
	return out
}

// Latest returns the newest sample for each GPU, ordered by GPU ID.
//
// A status poll needs only the current value; AllGPUSamples copies the whole
// hour-long ring buffer for every GPU, which is far too much to put on a
// per-poll mesh payload.
func (h *History) Latest() []GPUSample {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]GPUSample, 0, len(h.buffers))
	for _, rb := range h.buffers {
		if rb.count == 0 {
			continue
		}
		// head points at the next write slot, so the newest entry is behind it.
		newest := (rb.head - 1 + rb.maxSize) % rb.maxSize
		out = append(out, rb.samples[newest])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GPUID < out[j].GPUID })
	return out
}
