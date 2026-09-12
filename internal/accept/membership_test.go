package accept

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

const testPoll = 100 * time.Millisecond

func TestJoinFromThirdPoll(t *testing.T) { // J1
	obs := newFakeNode(t, "node-b")
	obs.SetCluster(clusterOf("node-b", member("node-b", meshapi.MemberAlive, nil)))
	clock := newFakeClock()
	st := nodeStatus("node-a", backendModel("a", "healthy"), backendModel("b", "starting"))
	clock.onSleep = func(el time.Duration) {
		if el == 200*time.Millisecond {
			obs.SetCluster(clusterOf("node-b", member("node-b", meshapi.MemberAlive, nil), member("node-a", meshapi.MemberAlive, &st)))
		}
	}
	r := WaitJoin(context.Background(), clock.env(), obs.URL(), "node-a", []string{"a", "b"}, 15*time.Second, testPoll)
	if r.Command != "join" || r.Target != "node-a" || len(r.Checks) != 1 {
		t.Fatalf("report: %+v", r)
	}
	c := r.Checks[0]
	if c.Name != "joined node-a" || !c.Pass || c.Elapsed != 200*time.Millisecond {
		t.Errorf("check: %+v", c)
	}
}

func TestJoinMissingModel(t *testing.T) { // J2
	obs := newFakeNode(t, "node-b")
	st := nodeStatus("node-a", backendModel("a", "healthy"))
	obs.SetCluster(clusterOf("node-b", member("node-a", meshapi.MemberAlive, &st)))
	clock := newFakeClock()
	r := WaitJoin(context.Background(), clock.env(), obs.URL(), "node-a", []string{"a", "b"}, time.Second, testPoll)
	c := r.Checks[0]
	if c.Pass || !strings.Contains(c.Detail, "missing b") {
		t.Errorf("check: %+v", c)
	}
	if got := clock.Now().Sub(clock.start); got != time.Second {
		t.Errorf("waited %v, want the 1s timeout", got)
	}
}

func TestJoinMemberDead(t *testing.T) { // J3
	obs := newFakeNode(t, "node-b")
	obs.SetCluster(clusterOf("node-b", member("node-a", meshapi.MemberDead, nil)))
	clock := newFakeClock()
	r := WaitJoin(context.Background(), clock.env(), obs.URL(), "node-a", nil, time.Second, testPoll)
	if c := r.Checks[0]; c.Pass || !strings.Contains(c.Detail, "dead") {
		t.Errorf("check: %+v", c)
	}
}

func TestJoinObserverRefusesThenAnswers(t *testing.T) { // J4
	addr, reopen := closedAddr(t)
	clock := newFakeClock()
	st := nodeStatus("node-a", backendModel("a", "healthy"), backendModel("b", "healthy"))
	clock.onSleep = func(el time.Duration) {
		if el == 200*time.Millisecond {
			obs := startFakeNode(t, "node-b", reopen())
			obs.SetCluster(clusterOf("node-b", member("node-a", meshapi.MemberAlive, &st)))
		}
	}
	r := WaitJoin(context.Background(), clock.env(), addr, "node-a", []string{"a", "b"}, 15*time.Second, testPoll)
	if c := r.Checks[0]; !c.Pass || c.Elapsed != 200*time.Millisecond {
		t.Errorf("check: %+v", c)
	}
}

func TestJoinObserverNeverAnswers(t *testing.T) { // J5
	addr, _ := closedAddr(t)
	clock := newFakeClock()
	r := WaitJoin(context.Background(), clock.env(), "http://"+addr, "node-a", nil, 500*time.Millisecond, testPoll)
	if c := r.Checks[0]; c.Pass || !strings.Contains(c.Detail, "last error") {
		t.Errorf("check: %+v", c)
	}
}

func TestReadyModelsInTurn(t *testing.T) { // RD1
	node := newFakeNode(t, "node-a")
	node.SetStatus(nodeStatus("node-a", backendModel("a", "healthy", "starting"), backendModel("b", "starting")))
	clock := newFakeClock()
	clock.onSleep = func(el time.Duration) {
		if el == 300*time.Millisecond {
			node.SetStatus(nodeStatus("node-a", backendModel("a", "healthy", "starting"), backendModel("b", "healthy")))
		}
	}
	r := WaitReady(context.Background(), clock.env(), node.URL(), 60*time.Minute, testPoll)
	if r.Command != "ready" {
		t.Errorf("command %q", r.Command)
	}
	if got := strings.Join(checkNames(r), "|"); got != "ready a|ready b|all backends healthy" {
		t.Fatalf("checks: %+v", r.Checks)
	}
	if c := r.Checks[0]; !c.Pass || c.Elapsed != 0 {
		t.Errorf("ready a: %+v", c)
	}
	if c := r.Checks[1]; !c.Pass || c.Elapsed != 300*time.Millisecond {
		t.Errorf("ready b: %+v", c)
	}
	if c := r.Checks[2]; c.Pass || !strings.Contains(c.Detail, "a/1") {
		t.Errorf("all backends healthy: %+v", c)
	}
}

func TestReadyTimeoutNamesBackends(t *testing.T) { // RD2
	node := newFakeNode(t, "node-a")
	b := backendModel("b", "starting", "unhealthy")
	b.Backends[1].Phase = "respawn grace"
	node.SetStatus(nodeStatus("node-a", backendModel("a", "healthy"), b))
	clock := newFakeClock()
	r := WaitReady(context.Background(), clock.env(), node.URL(), 500*time.Millisecond, testPoll)
	c := checkByName(t, r, "ready b")
	if c.Pass || !strings.Contains(c.Detail, "b/0 starting (loading)") || !strings.Contains(c.Detail, "b/1 unhealthy (respawn grace)") {
		t.Errorf("ready b: %+v", c)
	}
	if c := checkByName(t, r, "ready a"); !c.Pass {
		t.Errorf("ready a: %+v", c)
	}
}

func TestReadyStatusUnreachable(t *testing.T) { // RD3
	addr, _ := closedAddr(t)
	clock := newFakeClock()
	r := WaitReady(context.Background(), clock.env(), addr, 500*time.Millisecond, testPoll)
	if len(r.Checks) != 1 || r.Checks[0].Name != "status reachable" || r.Checks[0].Pass {
		t.Errorf("checks: %+v", r.Checks)
	}
}

// goneScenario serves node-a alive and then, from the given elapsed time on,
// the given state ("" = absent).
func goneScenario(t *testing.T, states map[time.Duration]string) (*fakeNode, *fakeClock) {
	t.Helper()
	obs := newFakeNode(t, "node-b")
	st := nodeStatus("node-a")
	set := func(state string) {
		members := []meshapi.Member{member("node-b", meshapi.MemberAlive, nil)}
		if state != "" {
			members = append(members, member("node-a", state, &st))
		}
		obs.SetCluster(clusterOf("node-b", members...))
	}
	set(meshapi.MemberAlive)
	clock := newFakeClock()
	clock.onSleep = func(el time.Duration) {
		if state, ok := states[el]; ok {
			set(state)
		}
	}
	return obs, clock
}

func TestGoneLeft(t *testing.T) { // G1
	obs, clock := goneScenario(t, map[time.Duration]string{200 * time.Millisecond: meshapi.MemberLeft})
	r := WaitGone(context.Background(), clock.env(), obs.URL(), "node-a", "left", 120*time.Second, testPoll)
	if got := strings.Join(checkNames(r), "|"); got != "gone node-a|state left" {
		t.Fatalf("checks: %+v", r.Checks)
	}
	if c := r.Checks[0]; !c.Pass || c.Elapsed != 200*time.Millisecond || c.Detail != "left" {
		t.Errorf("gone: %+v", c)
	}
	if c := r.Checks[1]; !c.Pass {
		t.Errorf("state left: %+v", c)
	}
}

func TestGoneAbsentIsNotDead(t *testing.T) { // G2
	obs, clock := goneScenario(t, map[time.Duration]string{200 * time.Millisecond: ""})
	r := WaitGone(context.Background(), clock.env(), obs.URL(), "node-a", "dead", 120*time.Second, testPoll)
	if c := checkByName(t, r, "gone node-a"); !c.Pass || c.Detail != "absent" {
		t.Errorf("gone: %+v", c)
	}
	if c := checkByName(t, r, "state dead"); c.Pass {
		t.Errorf("state dead: %+v", c)
	}
}

func TestGoneNeverListed(t *testing.T) { // G3
	obs := newFakeNode(t, "node-b")
	obs.SetCluster(clusterOf("node-b", member("node-b", meshapi.MemberAlive, nil)))
	clock := newFakeClock()
	r := WaitGone(context.Background(), clock.env(), obs.URL(), "node-a", "any", 120*time.Second, testPoll)
	if len(r.Checks) != 1 || r.Checks[0].Name != "member at start" || r.Checks[0].Pass {
		t.Errorf("checks: %+v", r.Checks)
	}
}

func TestGoneStillAlive(t *testing.T) { // G4
	obs, clock := goneScenario(t, nil)
	r := WaitGone(context.Background(), clock.env(), obs.URL(), "node-a", "any", 400*time.Millisecond, testPoll)
	if len(r.Checks) != 1 || r.Checks[0].Pass {
		t.Errorf("checks: %+v", r.Checks)
	}
}

// A hard-stopped member goes alive, suspect, dead. Gone is timed at suspect,
// and the state check waits for the state to settle.
func TestGoneThroughSuspect(t *testing.T) { // G5
	obs, clock := goneScenario(t, map[time.Duration]string{
		100 * time.Millisecond: meshapi.MemberSuspect,
		300 * time.Millisecond: meshapi.MemberDead,
	})
	r := WaitGone(context.Background(), clock.env(), obs.URL(), "node-a", "dead", 120*time.Second, testPoll)
	if c := checkByName(t, r, "gone node-a"); !c.Pass || c.Elapsed != 100*time.Millisecond || c.Detail != "suspect" {
		t.Errorf("gone: %+v", c)
	}
	if c := checkByName(t, r, "state dead"); !c.Pass || c.Elapsed != 300*time.Millisecond {
		t.Errorf("state dead: %+v", c)
	}
}

func TestGoneStuckSuspect(t *testing.T) { // G6
	obs, clock := goneScenario(t, map[time.Duration]string{100 * time.Millisecond: meshapi.MemberSuspect})
	r := WaitGone(context.Background(), clock.env(), obs.URL(), "node-a", "dead", 500*time.Millisecond, testPoll)
	if c := checkByName(t, r, "gone node-a"); !c.Pass {
		t.Errorf("gone: %+v", c)
	}
	if c := checkByName(t, r, "state dead"); c.Pass || !strings.Contains(c.Detail, "suspect") {
		t.Errorf("state dead: %+v", c)
	}
}

// A suspicion the member refutes is not a departure: gone is timed from the
// suspicion that sticks.
func TestGoneRefutedSuspicion(t *testing.T) { // G7
	obs, clock := goneScenario(t, map[time.Duration]string{
		100 * time.Millisecond: meshapi.MemberSuspect,
		200 * time.Millisecond: meshapi.MemberAlive,
		400 * time.Millisecond: meshapi.MemberDead,
	})
	r := WaitGone(context.Background(), clock.env(), obs.URL(), "node-a", "dead", 120*time.Second, testPoll)
	if c := checkByName(t, r, "gone node-a"); !c.Pass || c.Elapsed != 400*time.Millisecond || c.Detail != "dead" {
		t.Errorf("gone: %+v", c)
	}
	if c := checkByName(t, r, "state dead"); !c.Pass {
		t.Errorf("state dead: %+v", c)
	}
}

func TestWaitInterrupted(t *testing.T) {
	obs, _ := goneScenario(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	r := WaitGone(ctx, DefaultEnv(), obs.URL(), "node-a", "left", 120*time.Second, 50*time.Millisecond)
	if c := checkByName(t, r, "gone node-a"); c.Pass || c.Detail != "interrupted" {
		t.Errorf("gone: %+v", c)
	}
}
