package engine

import (
	"fmt"
	"sort"
	"sync"
)

var (
	mu       sync.RWMutex
	registry = map[string]Engine{}
)

// Register makes an engine available by name. An empty or duplicate name is a
// programming error, so it panics at init time rather than at first use.
func Register(e Engine) {
	name := e.Name()
	if name == "" {
		panic("engine: Register with empty name")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("engine: %q registered twice", name))
	}
	registry[name] = e
}

// Lookup returns the engine registered under name.
func Lookup(name string) (Engine, bool) {
	mu.RLock()
	defer mu.RUnlock()
	e, ok := registry[name]
	return e, ok
}

// Names returns every registered engine name, sorted.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
