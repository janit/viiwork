package route

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

// ErrNotText: the model exists in the fleet, but no host serves it for text
// inference; every host that lists it publishes another modality (a voice
// session model, an image generator). A *ModalityError carries the detail.
var ErrNotText = errors.New("route: model is not served for text inference")

// ModalityError is ErrNotText with what a client needs to go elsewhere: the
// modality the model is served with and the nodes that serve it. With a pin
// it names the pinned node alone, which is the whole question then.
type ModalityError struct {
	Model    string
	Modality string
	Nodes    []string // sorted
	Host     string   // the ?host= pin, "" = none
}

func (e *ModalityError) Error() string {
	return fmt.Sprintf("route: model %q is %s-only on %s", e.Model, e.Modality, strings.Join(e.Nodes, ", "))
}

func (e *ModalityError) Unwrap() error { return ErrNotText }

// textModel reports whether a report's entry is a model this router may send
// text inference to. Every peer rule goes through it, so a voice or image
// model never becomes a candidate however its slots look. Absent is text; an
// unknown modality is not (meshapi.IsTextModality).
func textModel(mc meshapi.ModelCapacity) bool { return meshapi.IsTextModality(mc.Modality) }

// modalityError builds the refusal for a model that is listed only with
// another modality: by every report naming it, or by the pinned host. Only on
// the error path, so it may allocate.
func (r *Router) modalityError(req Request, reports []capacity.Report) error {
	e := &ModalityError{Model: req.Model, Host: req.Host}
	for _, rep := range reports {
		if rep.Node == r.c.Self || (req.Host != "" && rep.Node != req.Host) {
			continue
		}
		mc, has := rep.Model(req.Model)
		if !has || textModel(mc) {
			continue
		}
		e.Nodes = append(e.Nodes, rep.Node)
		// Deterministic when hosts disagree: the smallest name, the fleet
		// view's rule.
		if e.Modality == "" || mc.Modality < e.Modality {
			e.Modality = mc.Modality
		}
	}
	slices.Sort(e.Nodes)
	return e
}
