//go:build integration

package node

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/mesh/meshtest"
	"github.com/janit/viiwork/v2/meshapi"
)

// seeded tunes a node onto net, seeding it with the given nodes' gossip
// addresses (read when the node starts, by which time they are set).
func seeded(net *meshtest.Network, name string, seeds ...*testNode) func(*testNode) func(*mesh.Options) {
	return func(tn *testNode) func(*mesh.Options) {
		var addrs []func() netip.AddrPort
		for _, s := range seeds {
			s := s
			addrs = append(addrs, func() netip.AddrPort { return s.gossip })
		}
		return meshTune(net, name, &tn.gossip, addrs...)
	}
}

func allAlive(nodes ...*testNode) func() bool {
	return func() bool {
		for _, n := range nodes {
			m := n.mesh.Load()
			if m == nil || m.NumAlive() != len(nodes) {
				return false
			}
		}
		return true
	}
}

type chatResult struct {
	status int
	header http.Header
	body   string
}

func chat(t *testing.T, tn *testNode, model string) chatResult {
	t.Helper()
	resp, err := http.Post(tn.url(meshapi.PathChatCompletions), "application/json",
		strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello from the test"}]}`, model)))
	if err != nil {
		return chatResult{body: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return chatResult{status: resp.StatusCode, header: resp.Header, body: string(body)}
}

func modelsOf(t *testing.T, tn *testNode) map[string]string {
	t.Helper()
	var resp meshapi.ModelsResponse
	getJSON(t, tn.url(meshapi.PathModels), &resp)
	out := map[string]string{}
	for _, e := range resp.Data {
		out[e.ID] = e.OwnedBy
	}
	return out
}

func TestNodeMeshIntegration(t *testing.T) {
	net := meshtest.NewNetwork()
	a := startNode(t, net, "A", []fakeModel{{name: "m"}}, "", seeded(net, "A"))
	b := startNode(t, net, "B", nil, "", seeded(net, "B", a))
	until(t, 10*time.Second, "A and B joined", allAlive(a, b))
	until(t, 10*time.Second, "m healthy on A", func() bool { return healthyModel(a.status(t), "m") })
	until(t, 10*time.Second, "B learns m", func() bool { return modelsOf(t, b)["m"] == meshapi.OwnedByPeer })

	// NI1: B forwards to A.
	r := chat(t, b, "m")
	if r.status != 200 || r.header.Get(meshapi.HeaderNode) != "A" || r.header.Get(meshapi.HeaderOrigin) != "B" {
		t.Fatalf("NI1: %d %v %s", r.status, r.header, r.body)
	}
	until(t, 3*time.Second, "A counts the forward", func() bool {
		m, _ := modelOf(a.status(t), "m")
		return m.RequestsTotal == 1
	})
	if st := b.status(t); len(st.Models) != 0 {
		t.Errorf("NI1: B serves models: %+v", st.Models)
	}

	// NI2: B's cluster view.
	until(t, 7*time.Second, "B's cluster has both statuses", func() bool {
		var c meshapi.ClusterResponse
		getJSON(t, b.url(meshapi.PathCluster), &c)
		if len(c.Members) != 2 || c.Mesh != meshapi.MeshOpen || strings.Join(c.Models, ",") != "m" {
			return false
		}
		for _, m := range c.Members {
			if m.State != meshapi.MemberAlive || m.Status == nil {
				return false
			}
		}
		return true
	})

	// NI3: B lists m as a peer model.
	if owned := modelsOf(t, b)["m"]; owned != meshapi.OwnedByPeer {
		t.Errorf("NI3: m owned_by %q on B", owned)
	}

	// NI4: an alias written on A resolves on B.
	req, _ := http.NewRequest(http.MethodPut, a.url(meshapi.AliasPath("stable")), strings.NewReader(`{"target":"m"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("NI4: PUT alias: %d", resp.StatusCode)
	}
	until(t, 3*time.Second, "B serves the alias", func() bool {
		r := chat(t, b, "stable")
		return r.status == 200 && r.header.Get(meshapi.HeaderAlias) == "stable" && r.header.Get(meshapi.HeaderModel) == "m"
	})

	// NI5: B's mesh stream carries A's activity, the cluster and the aliases.
	events := make(chan sseEvent, 512)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	streamReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.url(meshapi.PathMeshStream), nil)
	stream, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	go func() {
		sc := bufio.NewScanner(stream.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var name string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = line[len("event: "):]
			case strings.HasPrefix(line, "data: "):
				select {
				case events <- sseEvent{name, line[len("data: "):]}:
				default:
				}
			}
		}
	}()
	time.Sleep(1500 * time.Millisecond) // B's follower of A connects on its first tick
	if r := chat(t, a, "m"); r.status != 200 {
		t.Fatalf("NI5: request to A: %d %s", r.status, r.body)
	}
	var gotActivity, gotCluster, gotAliases bool
	deadline := time.After(10 * time.Second)
	for !(gotActivity && gotCluster && gotAliases) {
		select {
		case ev := <-events:
			switch ev.name {
			case meshapi.SSEActivity:
				gotActivity = gotActivity || strings.Contains(ev.data, `"hostname":"A"`)
			case meshapi.SSECluster:
				var c meshapi.ClusterResponse
				gotCluster = gotCluster || (json.Unmarshal([]byte(ev.data), &c) == nil && len(c.Members) == 2)
			case "aliases":
				gotAliases = gotAliases || strings.Contains(ev.data, `"name":"stable"`)
			}
		case <-deadline:
			t.Fatalf("NI5: activity from A %v, cluster %v, aliases %v", gotActivity, gotCluster, gotAliases)
		}
	}

	// NI6: the prompt of the request A ran itself (NI5), looked up through B. A
	// forward leaves no prompt history on the executing node (P4 Decision 12),
	// so NI1's request has none on A.
	var activityResp struct {
		Events []meshapi.Event `json:"events"`
	}
	getJSON(t, a.url(meshapi.PathActivity), &activityResp)
	var rid int64
	for _, ev := range activityResp.Events {
		if ev.Type == meshapi.EventRequest && ev.RequestID != 0 {
			rid = ev.RequestID
		}
	}
	if rid == 0 {
		t.Fatalf("NI6: no request event on A: %+v", activityResp.Events)
	}
	var entry meshapi.PromptEntry
	if code := getJSON(t, b.url(fmt.Sprintf("%s?addr=%s&rid=%d", meshapi.PathMeshPrompt, a.APIAddr(), rid)), &entry); code != 200 || !strings.Contains(entry.Prompt, "hello from the test") {
		t.Errorf("NI6: %d %+v", code, entry)
	}
	if code := getJSON(t, b.url(meshapi.PathMeshPrompt+"?addr=127.0.0.1:1&rid=1"), nil); code != 400 {
		t.Errorf("NI6: an unknown addr gave %d", code)
	}

	// NI7: a model added on A by reload appears on B.
	writeFile(t, a.cfgPath, nodeConfig("A", a.stateDir, []fakeModel{{name: "m"}, {name: "n"}}, ""))
	if err := a.Reload(); err != nil {
		t.Fatalf("NI7: %v", err)
	}
	until(t, 5*time.Second, "B lists n", func() bool { _, ok := modelsOf(t, b)["n"]; return ok })

	// NI8: A leaves.
	a.cancel()
	select {
	case err := <-a.done:
		a.done <- err
	case <-time.After(30 * time.Second):
		t.Fatal("NI8: A did not stop")
	}
	until(t, 3*time.Second, "B sees A left", func() bool {
		var c meshapi.ClusterResponse
		getJSON(t, b.url(meshapi.PathCluster), &c)
		for _, m := range c.Members {
			if m.Node == "A" {
				return m.State == meshapi.MemberLeft
			}
		}
		return false
	})
	until(t, 10*time.Second, "B answers 404 for m", func() bool { return chat(t, b, "m").status == 404 })
}

func TestNodeMeshQueueSpread(t *testing.T) {
	net := meshtest.NewNetwork()
	model := []fakeModel{{name: "m", parallel: 1, hold: "400ms"}}
	a := startNode(t, net, "A", model, "", seeded(net, "A"))
	c := startNode(t, net, "C", model, "", seeded(net, "C", a))
	b := startNode(t, net, "B", nil, "", seeded(net, "B", a, c))
	until(t, 10*time.Second, "all joined", allAlive(a, b, c))
	until(t, 10*time.Second, "m healthy on A and C", func() bool {
		return healthyModel(a.status(t), "m") && healthyModel(c.status(t), "m")
	})
	until(t, 10*time.Second, "B has fresh reports of A and C", func() bool {
		ra, okA := b.capPoller.Report("A")
		rc, okC := b.capPoller.Report("C")
		if !okA || !okC {
			return false
		}
		ma, _ := ra.Model("m")
		mc, _ := rc.Model("m")
		return ma.HealthyBackends > 0 && mc.HealthyBackends > 0
	})

	results := make([]chatResult, 4)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = chat(t, b, "m")
		}(i)
	}
	wg.Wait()
	servedBy := map[string]int{}
	queued := 0
	for i, r := range results {
		if r.status != 200 {
			t.Fatalf("NI9: request %d: %d %s", i, r.status, r.body)
		}
		servedBy[r.header.Get(meshapi.HeaderNode)]++
		if r.header.Get(meshapi.HeaderQueuedMs) != "" {
			queued++
		}
	}
	if servedBy["A"] == 0 || servedBy["C"] == 0 || queued == 0 {
		t.Errorf("NI9: served by %v, %d queued", servedBy, queued)
	}
}
