package accept

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

const (
	saturatePrompt    = "Count from 1 to 200, separated by spaces."
	saturateExtra     = 4
	saturateMaxTokens = 200
)

// SaturateOptions tune Saturate.
type SaturateOptions struct {
	Extra     int           // 0 = 4
	MaxTokens int           // 0 = 200
	Timeout   time.Duration // per request; 0 = 10m
}

// Saturate sends the node more concurrent requests for model than it has
// local slots, and checks that nothing is refused while the mesh has room
// and that the overflow reaches other members.
func Saturate(ctx context.Context, e Env, nodeAPI, model string, o SaturateOptions) Report {
	r := Report{Command: "saturate", Target: nodeAPI, Started: e.Now()}
	extra, maxTokens, timeout := o.Extra, o.MaxTokens, o.Timeout
	if extra <= 0 {
		extra = saturateExtra
	}
	if maxTokens <= 0 {
		maxTokens = saturateMaxTokens
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	base := baseURL(nodeAPI)

	st, err := readStatus(ctx, e, base, timeout)
	if err != nil {
		r.Checks = []Check{{Name: "status reachable", Detail: err.Error()}}
		return r
	}
	local := 0
	if ms, ok := modelOf(st, model); ok {
		local = ms.Slots
	}
	if local == 0 {
		r.Checks = []Check{{Name: "served locally", Detail: fmt.Sprintf("%s has no healthy backend for %s", st.Node, model)}}
		return r
	}

	var cluster meshapi.ClusterResponse
	if err := getJSON(ctx, e, base, meshapi.PathCluster, min(timeout, statusTimeout), &cluster); err != nil {
		r.Checks = []Check{{Name: "cluster reachable", Detail: err.Error()}}
		return r
	}
	meshFree := 0
	var others []string
	for _, m := range cluster.Members {
		if m.State != meshapi.MemberAlive || m.Status == nil {
			continue
		}
		ms, ok := modelOf(*m.Status, model)
		if !ok {
			continue
		}
		meshFree += max(0, ms.Slots-ms.Busy)
		if m.Node != st.Node && hasHealthyBackend(*m.Status, model) {
			others = append(others, m.Node)
		}
	}

	n := local + extra
	results := make([]chatResult, n)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = postChat(ctx, e, base, "", chatRequest(model, saturatePrompt, maxTokens), timeout)
		}()
	}
	wg.Wait()

	byStatus := map[int]int{}
	byNode := map[string]int{}
	queued, spilled := 0, false
	var firstErr string
	for _, res := range results {
		byStatus[res.status]++
		if res.status == 0 && firstErr == "" && res.err != nil {
			firstErr = res.err.Error()
		}
		if res.status != http.StatusOK {
			continue
		}
		node := res.header.Get(meshapi.HeaderNode)
		byNode[node]++
		spilled = spilled || node != st.Node
		if res.header.Get(meshapi.HeaderQueuedMs) != "" {
			queued++
		}
	}

	var counts []string
	statuses := make([]int, 0, len(byStatus))
	for s := range byStatus {
		statuses = append(statuses, s)
	}
	slices.Sort(statuses)
	unexpected := false
	for _, s := range statuses {
		label := fmt.Sprint(s)
		if s == 0 {
			label = "error"
		}
		counts = append(counts, fmt.Sprintf("%s:%d", label, byStatus[s]))
		unexpected = unexpected || (s != http.StatusOK && s != http.StatusTooManyRequests)
	}
	noRefusal := Check{
		Name:   "no 429 with free slots",
		Pass:   !unexpected && !(byStatus[http.StatusTooManyRequests] > 0 && meshFree >= n),
		Detail: fmt.Sprintf("n=%d %s meshFree=%d", n, strings.Join(counts, " "), meshFree),
	}
	if firstErr != "" {
		noRefusal.Detail += "; " + firstErr
	}

	spill := Check{Name: "spilled to a peer", Pass: true}
	switch {
	case len(others) == 0:
		spill.Detail = "not applicable: no other member serves " + model
	case meshFree <= local:
		spill.Detail = fmt.Sprintf("not applicable: meshFree %d is not above local %d", meshFree, local)
	case spilled:
		spill.Detail = "other members: " + strings.Join(others, ", ")
	default:
		spill.Pass = false
		spill.Detail = "every response came from " + st.Node
	}

	nodes := make([]string, 0, len(byNode))
	for name := range byNode {
		nodes = append(nodes, name)
	}
	slices.Sort(nodes)
	var dist []string
	for _, name := range nodes {
		dist = append(dist, fmt.Sprintf("%s:%d", name, byNode[name]))
	}
	dist = append(dist, fmt.Sprintf("queued:%d", queued))

	r.Checks = []Check{noRefusal, spill, {Name: "distribution", Pass: true, Detail: strings.Join(dist, " ")}}
	return r
}

func modelOf(st meshapi.NodeStatus, model string) (meshapi.ModelStatus, bool) {
	for _, m := range st.Models {
		if m.Name == model {
			return m, true
		}
	}
	return meshapi.ModelStatus{}, false
}
