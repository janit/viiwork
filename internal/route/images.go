package route

import (
	"github.com/janit/viiwork/v2/internal/supervisor"
	"github.com/janit/viiwork/v2/mesh/capacity"
)

// imageClass is what a host says about reading images with a model. imgAny,
// the zero value, is a request without an image: no filter.
type imageClass int8

const (
	imgAny imageClass = iota
	imgYes
	imgUnknown
	imgNo
)

// classOf reads the published answer: absent is "cannot say", never "no".
func classOf(images *bool) imageClass {
	switch {
	case images == nil:
		return imgUnknown
	case *images:
		return imgYes
	}
	return imgNo
}

func (r *Router) localImageClass(model string) imageClass {
	if r.c.LocalImages == nil {
		return imgUnknown
	}
	reads, known := r.c.LocalImages(model)
	switch {
	case !known:
		return imgUnknown
	case reads:
		return imgYes
	}
	return imgNo
}

// imageClassLocked decides which hosts a request with an image may use, from
// this node and every fresh report that lists the model with a healthy
// backend (the pinned host alone, with a pin):
//
//   - a host says yes: only hosts that say yes. A full one is waited for; a
//     host that cannot say is not tried in its place, because its likely
//     answer is the engine's own error after the wait.
//   - none says yes and one cannot say (an older version, an engine without
//     the answer, a model still loading): those hosts, as before v2.10.
//   - every host says no: ErrNoImageInput.
//
// With no fresh host at all nothing is ruled out and the request queues, as
// any request does: only fresh evidence rules a route in or out.
//
// localHealthy says this node has a healthy backend of the model. Like a
// peer without one, a node whose backends are loading, down or parked says
// nothing: it neither holds a request back from a 400 nor causes one.
func (r *Router) imageClassLocked(req Request, localHealthy bool, reports []capacity.Report) (imageClass, error) {
	now := r.c.Now()
	yes, unknown, any := false, false, false
	note := func(c imageClass) {
		any = true
		switch c {
		case imgYes:
			yes = true
		case imgUnknown:
			unknown = true
		}
	}
	if localHealthy && (req.Host == "" || req.Host == r.c.Self) {
		note(r.localImageClass(req.Model))
	}
	if req.Host != r.c.Self {
		for _, rep := range reports {
			if rep.Node == r.c.Self || (req.Host != "" && rep.Node != req.Host) || !capacity.Fresh(rep, now, r.c.StaleAfter) {
				continue
			}
			if mc, has := rep.Model(req.Model); has && mc.HealthyBackends > 0 && textModel(mc) {
				note(classOf(mc.Images))
			}
		}
	}
	switch {
	case yes:
		return imgYes, nil
	case unknown || !any:
		return imgUnknown, nil
	}
	return imgAny, ErrNoImageInput
}

// anyHealthy reports whether one of backends is healthy.
func anyHealthy(backends []LocalBackend) bool {
	for _, b := range backends {
		if b.State() == supervisor.StateHealthy {
			return true
		}
	}
	return false
}
