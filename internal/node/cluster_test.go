package node

import (
	"encoding/json"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/power"
	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/meshapi"
)

type fakeRemote map[string]meshapi.NodeStatus

func (f fakeRemote) Status(node string) (meshapi.NodeStatus, time.Time, bool) {
	st, ok := f[node]
	return st, time.Now(), ok
}

func clusterMember(name, addr, state, role string, local bool) mesh.Member {
	return mesh.Member{Name: name, Addr: netip.MustParseAddr(addr), Port: 7946,
		Meta: mesh.NodeMeta{V: mesh.MetaVersion, API: 8086, Ver: "2.0.0", Role: role}, State: state, Local: local}
}

func clusterFixture(remote fakeRemote) ClusterSources {
	members := []mesh.Member{
		clusterMember("gw", "100.64.0.9", meshapi.MemberAlive, meshapi.RoleGateway, false),
		clusterMember("gb3", "100.64.0.3", meshapi.MemberDead, meshapi.RoleNode, false),
		clusterMember("gb1", "100.64.0.1", meshapi.MemberAlive, meshapi.RoleNode, true),
		clusterMember("gb2", "100.64.0.2", meshapi.MemberAlive, meshapi.RoleNode, false),
	}
	ctl := power.NewController(power.ControlConfig{
		Enabled: true,
		Hosts:   []string{"gb3", "gb1", "gb2"},
		BMCs:    map[string]power.BMC{"gb3": {Addr: "192.0.2.3", Username: "u", Password: "p"}},
	}, "gb1")
	return ClusterSources{
		Self:    "gb1",
		Mode:    func() string { return meshapi.MeshSecured },
		Members: func() []mesh.Member { return members },
		Local: func() meshapi.NodeStatus {
			return meshapi.NodeStatus{Node: "gb1", Models: []meshapi.ModelStatus{{Name: "m1"}}, Cost: meshapi.CostInfo{Available: true, EURPerHour: 0.25, TodayEUR: 2}}
		},
		Remote:       remote,
		PowerControl: ctl,
	}
}

func gb2Status() meshapi.NodeStatus {
	return meshapi.NodeStatus{Node: "gb2", Models: []meshapi.ModelStatus{{Name: "m2"}, {Name: "m1"}}, Cost: meshapi.CostInfo{Available: true, EURPerHour: 0.10, TodayEUR: 1}}
}

func TestBuildCluster(t *testing.T) {
	c := BuildCluster(clusterFixture(fakeRemote{"gb2": gb2Status(), "gb3": {Node: "gb3", Models: []meshapi.ModelStatus{{Name: "ghost"}}}}))
	var names []string
	for _, m := range c.Members {
		names = append(names, m.Node)
	}
	if strings.Join(names, ",") != "gb1,gb2,gb3,gw" || c.View != "gb1" || c.Mesh != meshapi.MeshSecured {
		t.Fatalf("C1: view=%s mesh=%s members=%v", c.View, c.Mesh, names)
	}
	if c.Members[0].Status == nil || c.Members[1].Status == nil || c.Members[2].Status != nil || c.Members[3].Status != nil {
		t.Errorf("C1: statuses %v %v %v %v", c.Members[0].Status, c.Members[1].Status, c.Members[2].Status, c.Members[3].Status)
	}
	if m := c.Members[1]; m.Addr != "100.64.0.2" || m.Role != meshapi.RoleNode || m.State != meshapi.MemberAlive {
		t.Errorf("C1: gb2 = %+v", m)
	}
	if c.Members[3].Role != meshapi.RoleGateway {
		t.Errorf("C1: gw role %q", c.Members[3].Role)
	}
	if !reflect.DeepEqual(c.Models, []string{"m1", "m2"}) {
		t.Errorf("C2: models %v", c.Models)
	}
	if c.ClusterCostEURPerHour < 0.3499 || c.ClusterCostEURPerHour > 0.3501 || c.ClusterCostTodayEUR != 3 {
		t.Errorf("C3: cost %v / %v", c.ClusterCostEURPerHour, c.ClusterCostTodayEUR)
	}
	if c.PowerControl == nil || !reflect.DeepEqual(c.PowerControl.Hosts, []string{"gb1", "gb2", "gb3"}) || !reflect.DeepEqual(c.PowerControl.OutOfBand, []string{"gb3"}) {
		t.Errorf("C4: %+v", c.PowerControl)
	}
	raw, _ := json.Marshal(c.Members[2])
	if !strings.Contains(string(raw), `"status":null`) {
		t.Errorf("C6: %s", raw)
	}
}

func TestBuildClusterNotYetPolled(t *testing.T) {
	s := clusterFixture(fakeRemote{})
	s.PowerControl = nil
	c := BuildCluster(s)
	if c.Members[1].Status != nil || !reflect.DeepEqual(c.Models, []string{"m1"}) || c.PowerControl != nil {
		t.Errorf("C5: gb2 status %v, models %v, power %v", c.Members[1].Status, c.Models, c.PowerControl)
	}
}

// agedRemote answers every status as received at one fixed time.
type agedRemote struct {
	st  meshapi.NodeStatus
	age time.Time
}

func (a agedRemote) Status(string) (meshapi.NodeStatus, time.Time, bool) { return a.st, a.age, true }

// A member alive in gossip whose API has stopped answering keeps its last
// status in the poller. Past StatusStaleAfter that status is dropped from
// the view — absent, "cannot say" — rather than shown frozen, and its cost
// no longer counts toward the cluster's.
func TestBuildClusterDropsAStaleMemberStatus(t *testing.T) {
	s := clusterFixture(nil)
	s.Remote = agedRemote{st: gb2Status(), age: time.Now().Add(-time.Minute)}
	s.StatusStaleAfter = 20 * time.Second
	c := BuildCluster(s)
	for _, m := range c.Members {
		if m.Node == "gb2" && m.Status != nil {
			t.Errorf("gb2's minute-old status is shown: %+v", m.Status)
		}
	}
	if c.ClusterCostEURPerHour != 0.25 {
		t.Errorf("cluster cost %v, want gb1's 0.25 only", c.ClusterCostEURPerHour)
	}
}

// A voice member's status reaches the dashboards with its modality, through
// the poller's decode and the cluster view alike.
func TestClusterCarriesAMembersModality(t *testing.T) {
	var st meshapi.NodeStatus
	if err := json.Unmarshal([]byte(`{"node":"gb2","models":[{"name":"haru-4b","engine":"haru","slots":2,"busy":0,"queued":0,"ctx":0,"backends":[],"modality":"voice"}]}`), &st); err != nil {
		t.Fatal(err)
	}
	c := BuildCluster(clusterFixture(fakeRemote{"gb2": st}))
	b, _ := json.Marshal(c)
	if !strings.Contains(string(b), `"name":"haru-4b"`) || !strings.Contains(string(b), `"modality":"voice"`) {
		t.Errorf("cluster view lost the modality: %s", b)
	}
	if strings.Count(string(b), `"modality"`) != 1 {
		t.Errorf("a text model spelled its modality: %s", b)
	}
}
