package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/janit/viiwork/internal/peer"
	"github.com/janit/viiwork/internal/power"
)

// Chassis power control, in two endpoints that do deliberately different jobs.
//
//	POST /v1/power       act on THIS host, in-band. The executor.
//	POST /v1/mesh/power  act on any host in the fleet. The entry point.
//
// The split is what makes "even if it is powered off" work at all. A running
// host is controlled by the node living on it, over /dev/ipmi0 with no
// credentials; a host with nobody home has no such node, so the entry node
// reaches its BMC over the network instead. One request shape, two routes to
// the same BMC, chosen by whether anyone answers.
//
// Two guards, neither of which is authentication — viiwork has none, and this
// does not invent any:
//
//   - **The allowlist.** A host absent from power.control.hosts cannot be
//     targeted by either endpoint. Being peered with a node is not consent to
//     switch its machine off.
//   - **Self-refusal at the entry point.** /v1/mesh/power will not act on the
//     host serving it, because powering that machine off destroys the answer to
//     the request and the dashboard asking it. The executor does not carry this
//     rule: being asked by another node is exactly how a host is meant to be
//     switched off.

type powerRequest struct {
	Host   string `json:"host,omitempty"`
	Action string `json:"action"`
}

type powerResponse struct {
	Host   string `json:"host"`
	Action string `json:"action"`
	Result string `json:"result,omitempty"`
	Via    string `json:"via,omitempty"` // "in-band" | "out-of-band" | "peer"
	Error  string `json:"error,omitempty"`
}

func writePowerErr(w http.ResponseWriter, code int, host, action string, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(powerResponse{Host: host, Action: action, Error: err.Error()})
}

func decodePowerRequest(w http.ResponseWriter, r *http.Request) (powerRequest, bool) {
	var req powerRequest
	// Bounded: this body is two short strings, and an unbounded read on an
	// unauthenticated endpoint is a free memory sink.
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		writePowerErr(w, http.StatusBadRequest, req.Host, req.Action, fmt.Errorf("malformed request body"))
		return req, false
	}
	req.Host = strings.TrimSpace(req.Host)
	req.Action = strings.TrimSpace(strings.ToLower(req.Action))
	if !power.ValidAction(req.Action) {
		writePowerErr(w, http.StatusBadRequest, req.Host, req.Action,
			fmt.Errorf("action must be one of status, on, off, cycle"))
		return req, false
	}
	return req, true
}

// handlePower executes a chassis action against this node's own host.
func (h *Handler) handlePower(w http.ResponseWriter, r *http.Request) {
	if h.powerCtl == nil || !h.powerCtl.Enabled() {
		writePowerErr(w, http.StatusServiceUnavailable, "", "", fmt.Errorf("power control is not enabled on this node"))
		return
	}
	req, ok := decodePowerRequest(w, r)
	if !ok {
		return
	}
	self := h.localHostname()
	// A host parameter is accepted so the executor and entry point take the
	// same body, but it may only name this host: forwarding is the entry
	// point's job, and silently ignoring a mismatch would let a request aimed
	// at gb2 switch off gb1.
	if req.Host != "" && !strings.EqualFold(req.Host, self) {
		writePowerErr(w, http.StatusBadRequest, req.Host, req.Action,
			fmt.Errorf("this endpoint only acts on %s; use /v1/mesh/power to reach another host", self))
		return
	}
	out, err := h.powerCtl.Local(r.Context(), req.Action)
	if err != nil {
		writePowerErr(w, http.StatusInternalServerError, self, req.Action, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(powerResponse{Host: self, Action: req.Action, Result: out, Via: "in-band"})
}

// handleMeshPower routes a chassis action to whichever path can reach the host.
func (h *Handler) handleMeshPower(w http.ResponseWriter, r *http.Request) {
	if h.powerCtl == nil || !h.powerCtl.Enabled() {
		writePowerErr(w, http.StatusServiceUnavailable, "", "", fmt.Errorf("power control is not enabled on this node"))
		return
	}
	req, ok := decodePowerRequest(w, r)
	if !ok {
		return
	}
	if req.Host == "" {
		writePowerErr(w, http.StatusBadRequest, "", req.Action, fmt.Errorf("host is required"))
		return
	}
	if !h.powerCtl.Allowed(req.Host) {
		writePowerErr(w, http.StatusForbidden, req.Host, req.Action,
			fmt.Errorf("%s is not listed in power.control.hosts", req.Host))
		return
	}

	// The node serving this request will not act on its own machine. Reading
	// the answer requires the machine to still be running, and so does the page
	// that asked. Another node may switch this host off; this one may not.
	if self := h.localHostname(); strings.EqualFold(req.Host, self) && req.Action != power.ActionStatus {
		writePowerErr(w, http.StatusForbidden, req.Host, req.Action,
			fmt.Errorf("%s is serving this dashboard and will not %s itself; do it from another node", self, req.Action))
		return
	}

	// The target is this very host: act in-band directly. Only status reaches
	// here — everything mutating was refused above — but without this the
	// request would go looking for a peer and then out to a BMC over the
	// network, which is absurd for the machine the process is running on, and
	// fails outright when no credentials are configured.
	if strings.EqualFold(req.Host, h.localHostname()) {
		out, err := h.powerCtl.Local(r.Context(), req.Action)
		if err != nil {
			writePowerErr(w, http.StatusInternalServerError, req.Host, req.Action, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(powerResponse{Host: req.Host, Action: req.Action, Result: out, Via: "in-band"})
		return
	}

	// A reachable node on the target host is the preferred path: in-band needs
	// no credentials and cannot be pointed at the wrong machine.
	//
	// Note this can only be a node on ANOTHER host: a mutating action aimed at
	// this host was refused above, so a co-located sibling instance -- which
	// shares this hostname -- can never be used to route around that refusal.
	if addr, ok := h.peerAddrForHost(req.Host); ok {
		out, err := forwardPower(r.Context(), addr, req.Action)
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(powerResponse{Host: req.Host, Action: req.Action, Result: out, Via: "peer"})
			return
		}
		// Fall through to out-of-band. A node that cannot be reached is
		// indistinguishable from a host that is going down, which is precisely
		// when the BMC is the thing that still answers.
	}

	out, err := h.powerCtl.Remote(r.Context(), req.Host, req.Action)
	if err != nil {
		writePowerErr(w, http.StatusBadGateway, req.Host, req.Action, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(powerResponse{Host: req.Host, Action: req.Action, Result: out, Via: "out-of-band"})
}

func (h *Handler) localHostname() string {
	if h.registry != nil {
		return h.registry.Hostname()
	}
	return ""
}

// peerAddrForHost finds a reachable peer living on host. Only peers already in
// this node's registry are considered — the same rule /v1/mesh/prompt follows,
// and for the same reason: an address taken from the request body would make
// this a proxy for anything on the LAN, which here also carries the BMCs.
func (h *Handler) peerAddrForHost(host string) (string, bool) {
	if h.registry == nil {
		return "", false
	}
	for _, p := range h.registry.Peers() {
		if p.Status() != peer.StatusReachable {
			continue
		}
		if strings.EqualFold(p.Hostname(), host) {
			return p.Addr, true
		}
	}
	return "", false
}

func forwardPower(ctx context.Context, addr, action string) (string, error) {
	body, _ := json.Marshal(powerRequest{Action: action})
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", "http://"+addr+"/v1/power", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out powerResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", fmt.Errorf("peer %s: malformed response", addr)
	}
	if out.Error != "" {
		return "", fmt.Errorf("peer %s: %s", addr, out.Error)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("peer %s: status %d", addr, resp.StatusCode)
	}
	return out.Result, nil
}
