package node

import (
	"sync"
	"sync/atomic"
)

// engineVersions is the installed version of each engine this node's models
// use, read once per process: reading one runs the engine's binary, which a
// status poll must never do or wait for. /v1/update waits for the read
// (Wait); /v1/status takes what is known (Known) and says nothing until then.
type engineVersions struct {
	read func() map[string]string
	once sync.Once
	v    atomic.Pointer[map[string]string]
}

// Wait runs the read if nobody has, waits for it, and returns its result.
func (e *engineVersions) Wait() map[string]string {
	e.once.Do(func() {
		v := e.read()
		if v == nil {
			v = map[string]string{}
		}
		e.v.Store(&v)
	})
	return *e.v.Load()
}

// Known is the result of the read, or nil while there is none. It never
// blocks.
func (e *engineVersions) Known() map[string]string {
	if p := e.v.Load(); p != nil {
		return *p
	}
	return nil
}
