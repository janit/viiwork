package node

import (
	"testing"
	"time"
)

func TestFlushBudget(t *testing.T) {
	for _, c := range []struct{ elapsed, want time.Duration }{
		{5 * time.Second, 65 * time.Second},  // idle: only the mesh leave ran
		{40 * time.Second, 30 * time.Second}, // requests drained: what is left
		{65 * time.Second, sessionFlush},     // grace used up: the floor
	} {
		if got := flushBudget(c.elapsed); got != c.want {
			t.Errorf("after %v: %v, want %v", c.elapsed, got, c.want)
		}
	}
}
