// internal/peer/route_host_test.go
package peer

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/balancer"
)

func routeKeys(rs []Route) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		if r.Type == RouteLocal {
			out = append(out, fmt.Sprintf("local:gpu-%d", r.Backend.GPUID))
		} else {
			out = append(out, "peer:"+r.Addr)
		}
	}
	return out
}

func TestFilterByHost(t *testing.T) {
	local := balancer.NewBackendState(0, "localhost:9001")
	routes := []Route{
		{Type: RouteLocal, Backend: local, Host: "gb1"},
		{Type: RoutePeer, Addr: "192.168.1.42:9404", Host: "gb2"},
		{Type: RoutePeer, Addr: "192.168.1.42:9302", Host: "gb2"},          // co-located instance, same hostname
		{Type: RoutePeer, Addr: "192.168.1.43:9404", Host: "192.168.1.43"}, // peer too old to report a hostname
		{Type: RoutePeer, Addr: "192.168.1.44:9404"},                       // hostname unknown: matches nothing
	}
	cases := []struct {
		name, host string
		want       []string
	}{
		{"local route by the node's own hostname", "gb1", []string{"local:gpu-0"}},
		{"peer by reported hostname keeps every co-located instance", "gb2", []string{"peer:192.168.1.42:9404", "peer:192.168.1.42:9302"}},
		{"case-insensitive", "GB2", []string{"peer:192.168.1.42:9404", "peer:192.168.1.42:9302"}},
		{"address fallback when no hostname was reported", "192.168.1.43", []string{"peer:192.168.1.43:9404"}},
		{"unknown host yields nothing, never a new route", "gb9", nil},
		{"empty is a rejected pin, not a wildcard", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := routeKeys(FilterByHost(routes, tc.host))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("FilterByHost(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
	if len(routes) != 5 {
		t.Error("FilterByHost must not mutate its input")
	}
}

func TestFindRoutesForModelNamesTheHost(t *testing.T) {
	local := balancer.NewBackendState(0, "localhost:9001")
	local.SetStatus(balancer.StatusHealthy)

	named := NewPeerState("192.168.1.42:9404")
	named.Update(StatusResponse{NodeID: "viiwork-gb2", Hostname: "gb2", Models: []string{"m"}})
	unnamed := NewPeerState("192.168.1.43:9404")
	unnamed.Update(StatusResponse{NodeID: "viiwork-old", Models: []string{"m"}})

	reg := NewRegistry("viiwork-gb1", "m", []*balancer.BackendState{local}, []*PeerState{named, unnamed}, time.Second)
	reg.SetLocation("gb1", "gb1:8080")

	got := map[string]string{}
	for _, r := range reg.FindRoutesForModel("m") {
		key := "peer:" + r.Addr
		if r.Type == RouteLocal {
			key = "local"
		}
		got[key] = r.Host
	}
	want := map[string]string{
		"local":                  "gb1",
		"peer:192.168.1.42:9404": "gb2",
		"peer:192.168.1.43:9404": "192.168.1.43", // same fallback the cluster snapshot publishes
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("route hosts = %v, want %v", got, want)
	}
}
