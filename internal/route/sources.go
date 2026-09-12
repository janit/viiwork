// Package route picks where a request runs: a local backend with a free slot,
// else the peer with the most free slots by its fresh capacity report, else
// the origin queue (spec section 4).
package route

import (
	"github.com/janit/viiwork/v2/internal/supervisor"
	"github.com/janit/viiwork/v2/mesh/capacity"
)

// LocalBackend is what the router needs of a backend; *supervisor.Backend
// satisfies it.
type LocalBackend interface {
	ID() string
	State() supervisor.State
	Addr() string
	Slots() int
	InFlight() int
	Acquire()
	Release()
	NoteHardFailure()
}

var _ LocalBackend = (*supervisor.Backend)(nil)

// LocalModels answers which backends serve a model on this node, and whether
// the model still admits requests (it stops while draining).
type LocalModels interface {
	Backends(model string) (backends []LocalBackend, admitting bool, ok bool)
}

// Reports is the capacity reports of other members; *capacity.Poller
// satisfies it.
type Reports interface {
	Reports() []capacity.Report
}

type supervisorModels struct{ s *supervisor.Supervisor }

// SupervisorModels adapts the node supervisor to LocalModels.
func SupervisorModels(s *supervisor.Supervisor) LocalModels { return supervisorModels{s: s} }

func (a supervisorModels) Backends(model string) ([]LocalBackend, bool, bool) {
	m, ok := a.s.Model(model)
	if !ok {
		return nil, false, false
	}
	bs := m.Backends()
	out := make([]LocalBackend, len(bs))
	for i, b := range bs {
		out[i] = b
	}
	return out, m.Admitting(), true
}
