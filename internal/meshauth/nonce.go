package meshauth

import (
	"sync"
	"time"
)

// NonceCache closes replay on the one call that is not idempotent: a
// forwarded inference request. A GET poll leaks nothing and changes nothing,
// so the poll path deliberately keeps no cache and pays no memory for one.
//
// Bounded by the number of forwards inside two skew windows, because anything
// older is refused by the skew check before it ever reaches here.
type NonceCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
	ttl  time.Duration
	now  func() time.Time
}

func NewNonceCache(ttl time.Duration) *NonceCache {
	return &NonceCache{seen: make(map[string]time.Time), ttl: ttl, now: time.Now}
}

// Use records a nonce and reports whether it was fresh. False means replay.
func (c *NonceCache) Use(nonce string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, t := range c.seen {
		if now.Sub(t) > c.ttl {
			delete(c.seen, k)
		}
	}
	if _, dup := c.seen[nonce]; dup {
		return false
	}
	c.seen[nonce] = now
	return true
}

func (c *NonceCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}
