package node

import (
	"reflect"
	"strings"

	"github.com/janit/viiwork/v2/internal/config"
)

// Reload re-reads the config file (SIGHUP). Models are applied; any other
// section that changed is named in one log line and waits for a restart,
// because its values were captured at start (P6 Decision 6). A file that does
// not load changes nothing. Safe to call while Run is serving.
func (n *Node) Reload() error {
	next, err := config.Load(n.o.ConfigPath, n.o.LookupEnv)
	if err != nil {
		n.logf("reload: %v; keeping the running configuration", err)
		return err
	}
	current := n.runningConfig()

	sections := []struct {
		name     string
		old, new any
	}{
		{"Node", current.Node, next.Node},
		{"API", current.API, next.API},
		{"Mesh", current.Mesh, next.Mesh},
		{"Routing", current.Routing, next.Routing},
		{"GPU", current.GPU, next.GPU},
		{"Health", current.Health, next.Health},
		{"Activity", current.Activity, next.Activity},
		{"Power", current.Power, next.Power},
		{"Energy", current.Energy, next.Energy},
		{"Cost", current.Cost, next.Cost},
		{"Pipelines", current.Pipelines, next.Pipelines},
	}
	var restart []string
	for _, s := range sections {
		if !reflect.DeepEqual(s.old, s.new) {
			restart = append(restart, s.name)
		}
	}
	if len(restart) > 0 {
		n.logf("reload: changes to %s need a restart and were not applied", strings.Join(restart, ", "))
	}

	if reflect.DeepEqual(current.Models, next.Models) {
		return nil
	}
	if err := n.sup.Apply(next.Models); err != nil {
		n.logf("reload: applying models: %v", err)
		return err
	}
	n.mu.Lock()
	updated := *n.cfg
	updated.Models = next.Models
	n.cfg = &updated
	n.mu.Unlock()
	n.logf("reload: models applied (%d configured)", len(next.Models))
	return nil
}
