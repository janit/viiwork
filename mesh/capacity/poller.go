// Package capacity polls mesh members' /v1/capacity reports. It is public so
// viiwork-gateway routes by the same reports and the same code as the nodes
// (P4 Decision 1): a private poller would force the gateway to write the fifth
// copy of a registry, the problem the viiwork 2 design starts from.
package capacity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/meshapi"
)

const (
	maxReportBytes = 1 << 20
	dialTimeout    = 500 * time.Millisecond
	rttAlpha       = 0.3
)

// Members is the member list the poller follows; *mesh.Mesh satisfies it.
type Members interface {
	Members() []mesh.Member
}

// Report is one member's last accepted capacity report.
type Report struct {
	Node     string
	APIAddr  string
	Received time.Time
	RTT      time.Duration // EWMA of successful polls
	Models   []meshapi.ModelCapacity
}

// Model returns the report's entry for name.
func (r Report) Model(name string) (meshapi.ModelCapacity, bool) {
	for _, m := range r.Models {
		if m.Name == name {
			return m, true
		}
	}
	return meshapi.ModelCapacity{}, false
}

// Fresh reports whether r is young enough to route by.
func Fresh(r Report, now time.Time, staleAfter time.Duration) bool {
	return now.Sub(r.Received) < staleAfter
}

type Config struct {
	Self     string
	Members  Members
	Interval time.Duration                    // mesh.capacity_poll
	Client   *http.Client                     // nil = the default below
	OnReport func(node string)                // after each accepted report; must not block; nil = none
	Logf     func(format string, args ...any) // nil = log.Printf
	Now      func() time.Time                 // nil = time.Now
}

// Poller holds members' capacity reports, refreshed every Interval.
type Poller struct {
	c      Config
	client *http.Client

	mu       sync.Mutex
	ctx      context.Context
	reports  map[string]Report
	wanted   map[string]bool // members currently polled; a late answer for anyone else is dropped
	inflight map[string]bool
	logged   map[string]bool // a rejection was logged since the last accepted report
}

func NewPoller(c Config) *Poller {
	if c.Logf == nil {
		c.Logf = log.Printf
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	client := c.Client
	if client == nil {
		// Mesh traffic ignores proxy variables: one inherited by a container
		// must not route tailnet polls elsewhere. The dial is short because a
		// member that does not answer within half a second is not one to route to.
		client = &http.Client{Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: dialTimeout}).DialContext,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
		}}
	}
	return &Poller{
		c: c, client: client, ctx: context.Background(),
		reports: map[string]Report{}, wanted: map[string]bool{},
		inflight: map[string]bool{}, logged: map[string]bool{},
	}
}

// Run polls every Interval until ctx ends; in-flight polls use ctx.
func (p *Poller) Run(ctx context.Context) {
	p.mu.Lock()
	p.ctx = ctx
	p.mu.Unlock()
	p.tick()
	t := time.NewTicker(p.c.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.tick()
		}
	}
}

// pollable is an alive, remote member of role node: a gateway serves no models.
func (p *Poller) pollable(m mesh.Member) bool {
	return m.State == meshapi.MemberAlive && !m.Local && m.Name != p.c.Self && m.Meta.Role == meshapi.RoleNode
}

func (p *Poller) tick() {
	members := p.c.Members.Members()
	wanted := map[string]bool{}
	var targets []mesh.Member
	for _, m := range members {
		if p.pollable(m) {
			wanted[m.Name] = true
			targets = append(targets, m)
		}
	}
	p.mu.Lock()
	p.wanted = wanted
	for name := range p.reports {
		if !wanted[name] {
			delete(p.reports, name)
		}
	}
	p.mu.Unlock()
	for _, m := range targets {
		p.poll(m)
	}
}

// PollNow polls one member at once, unless a poll of it is already in flight.
func (p *Poller) PollNow(node string) {
	for _, m := range p.c.Members.Members() {
		if m.Name == node && p.pollable(m) {
			p.mu.Lock()
			p.wanted[node] = true
			p.mu.Unlock()
			p.poll(m)
			return
		}
	}
}

// HandleMemberEvent polls a member that joined or changed at once, and forgets
// one that left or failed.
func (p *Poller) HandleMemberEvent(ev mesh.MemberEvent) {
	switch ev.Kind {
	case mesh.EventJoin, mesh.EventUpdate:
		p.PollNow(ev.Member.Name)
	case mesh.EventLeave, mesh.EventFail:
		p.Forget(ev.Member.Name)
	}
}

// Forget drops a member's report.
func (p *Poller) Forget(node string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.reports, node)
	delete(p.wanted, node)
}

func (p *Poller) Report(node string) (Report, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.reports[node]
	return r, ok
}

// Reports returns every report, sorted by node.
func (p *Poller) Reports() []Report {
	p.mu.Lock()
	out := make([]Report, 0, len(p.reports))
	for _, r := range p.reports {
		out = append(out, r)
	}
	p.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

// poll fetches one member's report in its own goroutine. Polls to one member
// never overlap, so a slow member never stacks requests (Decision 2).
func (p *Poller) poll(m mesh.Member) {
	p.mu.Lock()
	if p.inflight[m.Name] {
		p.mu.Unlock()
		return
	}
	p.inflight[m.Name] = true
	ctx := p.ctx
	p.mu.Unlock()

	go func() {
		defer func() {
			p.mu.Lock()
			delete(p.inflight, m.Name)
			p.mu.Unlock()
		}()
		addr := m.APIAddr()
		rep, err := p.fetch(ctx, m.Name, addr)
		if err != nil {
			p.reject(m.Name, addr, err)
			return
		}
		p.mu.Lock()
		if !p.wanted[m.Name] {
			p.mu.Unlock()
			return // left the polling set while the poll was in flight
		}
		if prev, ok := p.reports[m.Name]; ok && prev.RTT > 0 {
			rep.RTT = time.Duration(rttAlpha*float64(rep.RTT) + (1-rttAlpha)*float64(prev.RTT))
		}
		p.reports[m.Name] = rep
		delete(p.logged, m.Name)
		p.mu.Unlock()
		if p.c.OnReport != nil {
			p.c.OnReport(m.Name)
		}
	}()
}

func (p *Poller) fetch(ctx context.Context, name, addr string) (Report, error) {
	ctx, cancel := context.WithTimeout(ctx, p.c.Interval)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+meshapi.PathCapacity, nil)
	if err != nil {
		return Report{}, err
	}
	start := p.c.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		return Report{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReportBytes+1))
	received := p.c.Now()
	switch {
	case err != nil:
		return Report{}, fmt.Errorf("reading the report: %w", err)
	case resp.StatusCode != http.StatusOK:
		return Report{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	case len(body) > maxReportBytes:
		return Report{}, fmt.Errorf("report larger than %d bytes", maxReportBytes)
	}
	var cr meshapi.CapacityResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return Report{}, fmt.Errorf("decoding the report: %w", err)
	}
	if cr.Node != name {
		// An address that changed hands (DHCP, a reused tailnet IP) must not
		// attach one machine's slots to another's name (Decision 3).
		return Report{}, fmt.Errorf("report names %q, expected %s", cr.Node, name)
	}
	rtt := received.Sub(start)
	if rtt <= 0 {
		rtt = time.Nanosecond
	}
	return Report{Node: name, APIAddr: addr, Received: received, RTT: rtt, Models: cr.Models}, nil
}

// reject keeps the previous report, which simply ages, and logs once per
// member until its next accepted report.
func (p *Poller) reject(name, addr string, err error) {
	p.mu.Lock()
	first := !p.logged[name]
	p.logged[name] = true
	p.mu.Unlock()
	if first {
		p.c.Logf("capacity poll of %s (%s): %v", name, addr, err)
	}
}
