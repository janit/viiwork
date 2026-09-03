package meshauth

import (
	"testing"
	"time"
)

func TestNonceCacheRejectsReplayInsideWindow(t *testing.T) {
	now := time.Unix(1756800000, 0)
	c := NewNonceCache(2 * SkewWindow)
	c.now = func() time.Time { return now }

	if !c.Use("AAAAAAAAAAAAAAAAAAAAAA") {
		t.Fatal("first use should be accepted")
	}
	if c.Use("AAAAAAAAAAAAAAAAAAAAAA") {
		t.Fatal("replay inside the window should be rejected")
	}
}

func TestNonceCacheForgetsAfterTTL(t *testing.T) {
	now := time.Unix(1756800000, 0)
	c := NewNonceCache(2 * SkewWindow)
	c.now = func() time.Time { return now }

	c.Use("AAAAAAAAAAAAAAAAAAAAAA")
	now = now.Add(5 * SkewWindow)
	if !c.Use("AAAAAAAAAAAAAAAAAAAAAA") {
		t.Fatal("a nonce older than the ttl should be accepted again")
	}
	if got := c.Len(); got != 1 {
		t.Fatalf("cache holds %d entries, want 1 after sweep", got)
	}
}
