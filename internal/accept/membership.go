package accept

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

const (
	maxPollRequest    = 2 * time.Second
	maxBody           = 4 << 20
	detailInterrupted = "interrupted"
)

// baseURL turns host:port into an http URL and leaves a URL with a scheme as
// given.
func baseURL(addr string) string {
	if strings.Contains(addr, "://") {
		return strings.TrimRight(addr, "/")
	}
	return "http://" + addr
}

// getJSON GETs base+path within limit and decodes a 200 response into out.
func getJSON(ctx context.Context, e Env, base, path string, limit time.Duration, out any) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	addHeaders(req, e.Headers)
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	return nil
}

func addHeaders(req *http.Request, h http.Header) {
	for k, vs := range h {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
}

// pollUntil calls try every poll, passing the time elapsed since start, until
// try reports done or timeout has elapsed. It never sleeps past the timeout.
// interrupted is true when ctx ended the wait.
func pollUntil(ctx context.Context, e Env, start time.Time, timeout, poll time.Duration, try func(elapsed time.Duration) bool) (done, interrupted bool) {
	for {
		if ctx.Err() != nil {
			return false, true
		}
		if try(e.Now().Sub(start)) {
			return true, false
		}
		elapsed := e.Now().Sub(start)
		if elapsed >= timeout {
			return false, false
		}
		if err := e.Sleep(ctx, min(poll, timeout-elapsed)); err != nil {
			return false, true
		}
	}
}

func requestLimit(poll time.Duration) time.Duration { return min(poll, maxPollRequest) }

// WaitJoin waits until observer's cluster view lists node alive with a status
// that serves every expected model.
func WaitJoin(ctx context.Context, e Env, observer, node string, expect []string, timeout, poll time.Duration) Report {
	start := e.Now()
	r := Report{Command: "join", Target: node, Started: start}
	base := baseURL(observer)
	check := Check{Name: "joined " + node}

	var lastErr error
	var seen bool
	var last meshapi.Member
	var lastFound bool
	done, interrupted := pollUntil(ctx, e, start, timeout, poll, func(el time.Duration) bool {
		var c meshapi.ClusterResponse
		if err := getJSON(ctx, e, base, meshapi.PathCluster, requestLimit(poll), &c); err != nil {
			lastErr = err
			return false
		}
		lastErr, seen = nil, true
		last, lastFound = findMember(c, node)
		if !lastFound || last.State != meshapi.MemberAlive || last.Status == nil || len(missingModels(last.Status, expect)) > 0 {
			return false
		}
		check.Pass, check.Elapsed = true, el
		return true
	})
	switch {
	case done:
		if len(expect) > 0 {
			check.Detail = "models " + strings.Join(expect, ", ")
		}
	case interrupted:
		check.Detail = detailInterrupted
	default:
		var parts []string
		if seen {
			parts = append(parts, joinState(last, lastFound, expect))
		}
		if lastErr != nil {
			parts = append(parts, "last error: "+lastErr.Error())
		}
		check.Detail = strings.Join(parts, "; ")
	}
	r.Checks = []Check{check}
	return r
}

func joinState(m meshapi.Member, found bool, expect []string) string {
	if !found {
		return "not a member"
	}
	state := m.State
	if m.State == meshapi.MemberAlive && m.Status == nil {
		state += ", no status yet"
	}
	if missing := missingModels(m.Status, expect); len(missing) > 0 {
		state += ", missing " + strings.Join(missing, ", ")
	}
	return state
}

func findMember(c meshapi.ClusterResponse, node string) (meshapi.Member, bool) {
	for _, m := range c.Members {
		if m.Node == node {
			return m, true
		}
	}
	return meshapi.Member{}, false
}

// missingModels are the expected models st does not list (all of them when
// there is no status).
func missingModels(st *meshapi.NodeStatus, expect []string) []string {
	var missing []string
	for _, name := range expect {
		if st == nil || !slices.ContainsFunc(st.Models, func(m meshapi.ModelStatus) bool { return m.Name == name }) {
			missing = append(missing, name)
		}
	}
	return missing
}

// WaitReady waits until every model the node lists has a healthy backend,
// timing each model.
func WaitReady(ctx context.Context, e Env, nodeAPI string, timeout, poll time.Duration) Report {
	start := e.Now()
	r := Report{Command: "ready", Target: nodeAPI, Started: start}
	base := baseURL(nodeAPI)

	var models []string // from the first successful response
	readyAt := map[string]time.Duration{}
	var last meshapi.NodeStatus
	var lastErr error
	_, interrupted := pollUntil(ctx, e, start, timeout, poll, func(el time.Duration) bool {
		var st meshapi.NodeStatus
		if err := getJSON(ctx, e, base, meshapi.PathStatus, requestLimit(poll), &st); err != nil {
			lastErr = err
			return false
		}
		lastErr = nil
		if models == nil {
			models = []string{}
			for _, m := range st.Models {
				models = append(models, m.Name)
			}
		}
		last = st
		for _, name := range models {
			if _, ok := readyAt[name]; !ok && hasHealthyBackend(st, name) {
				readyAt[name] = el
			}
		}
		return len(readyAt) == len(models)
	})

	if models == nil {
		c := Check{Name: "status reachable", Detail: detailInterrupted}
		if !interrupted && lastErr != nil {
			c.Detail = "last error: " + lastErr.Error()
		}
		r.Checks = []Check{c}
		return r
	}
	for _, name := range models {
		c := Check{Name: "ready " + name}
		if at, ok := readyAt[name]; ok {
			c.Pass, c.Elapsed = true, at
		} else if interrupted {
			c.Detail = detailInterrupted
		} else {
			c.Detail = describeBackends(last, name, false)
		}
		r.Checks = append(r.Checks, c)
	}
	all := Check{Name: "all backends healthy", Pass: true}
	var unhealthy []string
	for _, m := range last.Models {
		if d := describeBackends(last, m.Name, true); d != "" {
			unhealthy = append(unhealthy, d)
		}
	}
	if len(unhealthy) > 0 {
		all.Pass, all.Detail = false, strings.Join(unhealthy, ", ")
	}
	r.Checks = append(r.Checks, all)
	return r
}

func hasHealthyBackend(st meshapi.NodeStatus, model string) bool {
	for _, m := range st.Models {
		if m.Name != model {
			continue
		}
		for _, b := range m.Backends {
			if b.Status == meshapi.StatusHealthy {
				return true
			}
		}
	}
	return false
}

// describeBackends lists a model's backends as "id status (phase)", only the
// unhealthy ones when onlyUnhealthy is set.
func describeBackends(st meshapi.NodeStatus, model string, onlyUnhealthy bool) string {
	for _, m := range st.Models {
		if m.Name != model {
			continue
		}
		var parts []string
		for _, b := range m.Backends {
			if onlyUnhealthy && b.Status == meshapi.StatusHealthy {
				continue
			}
			s := b.ID + " " + b.Status
			if b.Phase != "" {
				s += " (" + b.Phase + ")"
			}
			parts = append(parts, s)
		}
		if len(parts) == 0 && !onlyUnhealthy {
			return "no backends"
		}
		return strings.Join(parts, ", ")
	}
	return "not in status"
}

// WaitGone waits until observer no longer lists node as alive. expectState
// left or dead also checks the state it settles in. suspect does not count as
// settled: viiwork 2 never reports it, but an implementation that does passes
// through it on the way to dead.
func WaitGone(ctx context.Context, e Env, observer, node, expectState string, timeout, poll time.Duration) Report {
	start := e.Now()
	r := Report{Command: "gone", Target: node, Started: start}
	base := baseURL(observer)
	wantState := expectState == meshapi.MemberLeft || expectState == meshapi.MemberDead

	var (
		lastErr    error
		started    bool
		startState string
		gone       bool
		goneAt     time.Duration
		goneState  string
		settled    bool
		settledAt  time.Duration
		state      string // the latest state seen after the start
	)
	_, interrupted := pollUntil(ctx, e, start, timeout, poll, func(el time.Duration) bool {
		var c meshapi.ClusterResponse
		if err := getJSON(ctx, e, base, meshapi.PathCluster, requestLimit(poll), &c); err != nil {
			lastErr = err
			return false
		}
		lastErr = nil
		m, found := findMember(c, node)
		state = "absent"
		if found {
			state = m.State
		}
		if !started {
			if state != meshapi.MemberAlive {
				startState = state
				return true
			}
			started = true
			return false
		}
		if state == meshapi.MemberAlive {
			gone = false // a refuted suspicion: the member is back
			return false
		}
		if !gone {
			gone, goneAt, goneState = true, el, state
		}
		if !wantState {
			return true
		}
		if state != meshapi.MemberSuspect {
			settled, settledAt = true, el
			return true
		}
		return false
	})

	errDetail := func(fallback string) string {
		switch {
		case interrupted:
			return detailInterrupted
		case lastErr != nil:
			return fallback + "; last error: " + lastErr.Error()
		}
		return fallback
	}
	if !started {
		d := "not a member"
		switch {
		case startState != "" && startState != "absent":
			d = startState
		case startState == "" && lastErr != nil && !interrupted:
			d = "last error: " + lastErr.Error()
		case startState == "" && interrupted:
			d = detailInterrupted
		}
		r.Checks = []Check{{Name: "member at start", Detail: d}}
		return r
	}

	goneCheck := Check{Name: "gone " + node}
	if gone {
		goneCheck.Pass, goneCheck.Elapsed, goneCheck.Detail = true, goneAt, goneState
	} else {
		goneCheck.Detail = errDetail("still alive")
	}
	r.Checks = []Check{goneCheck}
	if !wantState {
		return r
	}
	sc := Check{Name: "state " + expectState}
	switch {
	case settled:
		sc.Pass, sc.Elapsed, sc.Detail = state == expectState, settledAt, state
	case gone:
		sc.Detail = errDetail(state)
	default:
		sc.Detail = errDetail("still alive")
	}
	r.Checks = append(r.Checks, sc)
	return r
}
