package node

import (
	"sort"
	"time"

	"github.com/janit/viiwork/v2/internal/power"
	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/meshapi"
)

// ClusterSources is everything /v1/cluster is built from.
type ClusterSources struct {
	Self    string
	Mode    func() string
	Members func() []mesh.Member
	Local   func() meshapi.NodeStatus
	Remote  interface {
		Status(node string) (meshapi.NodeStatus, time.Time, bool)
	}
	PowerControl *power.Controller // nil = none
}

// BuildCluster is the C4 cluster view: every member, its state, and the last
// status of every alive node member that has one.
func BuildCluster(s ClusterSources) meshapi.ClusterResponse {
	c := meshapi.ClusterResponse{View: s.Self, Members: []meshapi.Member{}, Models: []string{}}
	if s.Mode != nil {
		c.Mesh = s.Mode()
	}
	var members []mesh.Member
	if s.Members != nil {
		members = s.Members()
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })

	models := map[string]bool{}
	for _, m := range members {
		out := meshapi.Member{Node: m.Name, Addr: m.Addr.String(), Role: m.Meta.Role, State: m.State}
		switch {
		case m.Local && s.Local != nil:
			st := s.Local()
			out.Status = &st
		case m.State == meshapi.MemberAlive && m.Meta.Role == meshapi.RoleNode && s.Remote != nil:
			if st, _, ok := s.Remote.Status(m.Name); ok {
				out.Status = &st
			}
		}
		if out.Status != nil {
			for _, ms := range out.Status.Models {
				models[ms.Name] = true
			}
			// One node per machine: nothing left to deduplicate (Decision 12).
			if out.Status.Cost.Available {
				c.ClusterCostEURPerHour += out.Status.Cost.EURPerHour
				c.ClusterCostTodayEUR += out.Status.Cost.TodayEUR
			}
		}
		c.Members = append(c.Members, out)
	}
	for name := range models {
		c.Models = append(c.Models, name)
	}
	sort.Strings(c.Models)

	if s.PowerControl.Enabled() {
		info := &meshapi.PowerControlInfo{Hosts: s.PowerControl.Hosts()}
		for _, h := range info.Hosts {
			if s.PowerControl.HasBMC(h) {
				info.OutOfBand = append(info.OutOfBand, h)
			}
		}
		c.PowerControl = info
	}
	return c
}
