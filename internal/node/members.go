package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

const (
	statusTimeout  = 2 * time.Second
	maxStatusBytes = 4 << 20
)

type polledStatus struct {
	status   meshapi.NodeStatus
	received time.Time
}

// StatusPoller holds every alive node member's last /v1/status (P6
// Decision 1). It follows the capacity poller: polls to one member never
// overlap, a failure keeps the last status, a status naming another node is
// refused, and a member that leaves the list is forgotten.
type StatusPoller struct {
	self     string
	members  capacity.Members
	interval time.Duration
	client   *http.Client
	logf     func(string, ...any)

	mu       sync.Mutex
	ctx      context.Context
	statuses map[string]polledStatus
	wanted   map[string]bool
	inflight map[string]bool
	logged   map[string]bool
}

func NewStatusPoller(self string, members capacity.Members, interval time.Duration, client *http.Client, logf func(string, ...any)) *StatusPoller {
	if logf == nil {
		logf = log.Printf
	}
	if client == nil {
		client = &http.Client{Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: statusTimeout}).DialContext,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
		}}
	}
	return &StatusPoller{
		self: self, members: members, interval: interval, client: client, logf: logf,
		ctx: context.Background(), statuses: map[string]polledStatus{}, wanted: map[string]bool{},
		inflight: map[string]bool{}, logged: map[string]bool{},
	}
}

// Run polls every interval until ctx ends.
func (p *StatusPoller) Run(ctx context.Context) {
	p.mu.Lock()
	p.ctx = ctx
	p.mu.Unlock()
	p.tick()
	t := time.NewTicker(p.interval)
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

// Status is a member's last accepted status and when it arrived.
func (p *StatusPoller) Status(node string) (meshapi.NodeStatus, time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.statuses[node]
	return s.status, s.received, ok
}

// HandleMemberEvent polls a member that joined or changed at once, and forgets
// one that left or failed.
func (p *StatusPoller) HandleMemberEvent(ev mesh.MemberEvent) {
	switch ev.Kind {
	case mesh.EventJoin, mesh.EventUpdate:
		if p.pollable(ev.Member) {
			p.mu.Lock()
			p.wanted[ev.Member.Name] = true
			p.mu.Unlock()
			p.poll(ev.Member)
		}
	case mesh.EventLeave, mesh.EventFail:
		p.forget(ev.Member.Name)
	}
}

func (p *StatusPoller) forget(node string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.statuses, node)
	delete(p.wanted, node)
}

func (p *StatusPoller) pollable(m mesh.Member) bool {
	return m.State == meshapi.MemberAlive && !m.Local && m.Name != p.self && m.Meta.Role == meshapi.RoleNode
}

func (p *StatusPoller) tick() {
	wanted := map[string]bool{}
	var targets []mesh.Member
	for _, m := range p.members.Members() {
		if p.pollable(m) {
			wanted[m.Name] = true
			targets = append(targets, m)
		}
	}
	p.mu.Lock()
	p.wanted = wanted
	for name := range p.statuses {
		if !wanted[name] {
			delete(p.statuses, name)
		}
	}
	p.mu.Unlock()
	for _, m := range targets {
		p.poll(m)
	}
}

func (p *StatusPoller) poll(m mesh.Member) {
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
		st, err := p.fetch(ctx, m.Name, addr)
		if err != nil {
			p.mu.Lock()
			first := !p.logged[m.Name]
			p.logged[m.Name] = true
			p.mu.Unlock()
			if first {
				p.logf("status poll of %s (%s): %v", m.Name, addr, err)
			}
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if !p.wanted[m.Name] {
			return // left the polling set while the poll was in flight
		}
		p.statuses[m.Name] = polledStatus{status: st, received: time.Now()}
		delete(p.logged, m.Name)
	}()
}

func (p *StatusPoller) fetch(ctx context.Context, name, addr string) (meshapi.NodeStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+meshapi.PathStatus, nil)
	if err != nil {
		return meshapi.NodeStatus{}, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return meshapi.NodeStatus{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxStatusBytes+1))
	switch {
	case err != nil:
		return meshapi.NodeStatus{}, fmt.Errorf("reading the status: %w", err)
	case resp.StatusCode != http.StatusOK:
		return meshapi.NodeStatus{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	case len(body) > maxStatusBytes:
		return meshapi.NodeStatus{}, fmt.Errorf("status larger than %d bytes", maxStatusBytes)
	}
	var st meshapi.NodeStatus
	if err := json.Unmarshal(body, &st); err != nil {
		return meshapi.NodeStatus{}, fmt.Errorf("decoding the status: %w", err)
	}
	if st.Node != name {
		// An address that changed hands must not attach one machine's status
		// to another's name.
		return meshapi.NodeStatus{}, fmt.Errorf("status names %q, expected %s", st.Node, name)
	}
	return st, nil
}
