package gpu

import "sync"

type Broadcaster struct {
	mu      sync.Mutex
	clients []chan []byte
}

func NewBroadcaster() *Broadcaster { return &Broadcaster{} }

func (b *Broadcaster) Subscribe() chan []byte {
	ch := make(chan []byte, 4)
	b.mu.Lock()
	b.clients = append(b.clients, ch)
	b.mu.Unlock()
	return ch
}

func (b *Broadcaster) Unsubscribe(ch chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, c := range b.clients {
		if c == ch {
			b.clients = append(b.clients[:i], b.clients[i+1:]...)
			return
		}
	}
}

func (b *Broadcaster) Broadcast(data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.clients {
		select {
		case ch <- data:
		default:
		}
	}
}

func (b *Broadcaster) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.clients { close(ch) }
	b.clients = nil
}
