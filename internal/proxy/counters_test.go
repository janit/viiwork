package proxy

import (
	"sync"
	"testing"
)

func TestCounters(t *testing.T) {
	c := NewCounters()
	c.Add("a", 42)
	c.Add("a", 0)
	c.Add("b", 5)
	if got := c.Get("a"); got != (ModelCounters{Requests: 2, Tokens: 42}) {
		t.Errorf("a = %+v", got)
	}
	if got := c.Get("b"); got != (ModelCounters{Requests: 1, Tokens: 5}) {
		t.Errorf("b = %+v", got)
	}
	if got := c.Get("zzz"); got != (ModelCounters{}) {
		t.Errorf("zzz = %+v", got)
	}
	if snap := c.Snapshot(); len(snap) != 2 || snap["a"].Requests != 2 || snap["b"].Tokens != 5 {
		t.Errorf("snapshot = %+v", snap)
	}
}

func TestCountersConcurrent(t *testing.T) {
	c := NewCounters()
	var wg sync.WaitGroup
	for g := 0; g < 100; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				c.Add("a", 1)
			}
		}()
	}
	wg.Wait()
	if got := c.Get("a"); got != (ModelCounters{Requests: 10000, Tokens: 10000}) {
		t.Errorf("a = %+v", got)
	}
}
