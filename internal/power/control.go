package power

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// Chassis power control, over the same ipmitool the wattage readings already
// go through.
//
// There are two ways to reach a BMC and they are not interchangeable:
//
//   - **In-band** (/dev/ipmi0) needs no credentials and no network hop, and is
//     what every node already uses to read its own wattage. It can only be
//     issued from a machine that is running, which makes it useless for the one
//     action an operator most wants: powering a dead host back on.
//   - **Out-of-band** (ipmitool -I lanplus) reaches a BMC that is standing by
//     while the host is off. It is the only way to power a host *on*, and it
//     costs what in-band deliberately avoided — an address, a username and a
//     password for every host.
//
// So a node controls its own host in-band, the mesh forwards a request to the
// node that owns the target host, and out-of-band exists solely for hosts with
// nobody home. That last path stays disabled until it is configured, and a
// Controller with no BMC entry for a host says so rather than failing obscurely.
const (
	ActionStatus = "status"
	ActionOn     = "on"
	ActionOff    = "off"
	ActionCycle  = "cycle"
)

// controlBudget bounds one chassis command. Generous next to the DCMI read
// (73-113ms measured) because an out-of-band session sets up an RMCP+ handshake
// first, and a BMC that is not answering should fail rather than hang the
// request for the caller's whole timeout.
const controlBudget = 20 * time.Second

// ValidAction reports whether a is a chassis action this package will issue.
// Rejecting anything else by name — rather than passing it to ipmitool — is
// what keeps a request body from choosing the command that runs.
func ValidAction(a string) bool {
	switch a {
	case ActionStatus, ActionOn, ActionOff, ActionCycle:
		return true
	}
	return false
}

// BMC is one host's out-of-band endpoint.
type BMC struct {
	Addr     string
	Username string
	Password string
}

// ControlConfig is what a Controller needs to know. Hosts is an allowlist: a
// host absent from it cannot be targeted at all, which is the guard standing in
// for authentication on an API that has none.
type ControlConfig struct {
	Enabled bool
	Hosts   []string
	// BMCs maps hostname to its out-of-band endpoint. Optional and usually
	// partial: a host is reachable in-band whenever it is up, so an entry is
	// only needed for hosts that must be controllable while off.
	BMCs map[string]BMC
}

type Controller struct {
	cfg      ControlConfig
	logger   *log.Logger
	run      runner
	selfHost string

	mu         sync.RWMutex
	discovered map[string]string // hostname -> BMC address learned in-band
}

func NewController(cfg ControlConfig, selfHost string) *Controller {
	return &Controller{
		cfg:        cfg,
		selfHost:   selfHost,
		logger:     log.New(os.Stdout, "[power] ", log.LstdFlags),
		run:        execIpmitool,
		discovered: make(map[string]string),
	}
}

func (c *Controller) Enabled() bool { return c != nil && c.cfg.Enabled }

// Allowed reports whether host may be targeted. An empty allowlist permits
// nothing: a feature that can switch machines off should require someone to
// have named them, not default to the whole mesh.
func (c *Controller) Allowed(host string) bool {
	if !c.Enabled() || host == "" {
		return false
	}
	for _, h := range c.cfg.Hosts {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

// Hosts returns the allowlist, sorted. The dashboard renders a row per entry so
// a host that is powered off — and therefore absent from the mesh entirely —
// still has a button to bring it back.
func (c *Controller) Hosts() []string {
	if !c.Enabled() {
		return nil
	}
	out := append([]string(nil), c.cfg.Hosts...)
	sort.Strings(out)
	return out
}

// HasBMC reports whether host can be reached while it is powered off.
func (c *Controller) HasBMC(host string) bool {
	_, ok := c.bmcFor(host)
	return ok
}

func (c *Controller) bmcFor(host string) (BMC, bool) {
	if !c.Enabled() {
		return BMC{}, false
	}
	for name, b := range c.cfg.BMCs {
		if !strings.EqualFold(name, host) {
			continue
		}
		if b.Addr == "" {
			// Configured without an address: fall back to whatever the host
			// reported for itself while it was up. BMCs here are on DHCP, so a
			// learned address is often fresher than a written one.
			c.mu.RLock()
			addr := c.discovered[strings.ToLower(host)]
			c.mu.RUnlock()
			if addr == "" {
				return BMC{}, false
			}
			b.Addr = addr
		}
		if b.Username == "" || b.Password == "" {
			return BMC{}, false
		}
		return b, true
	}
	return BMC{}, false
}

// LearnBMCAddr records the address a host reported for its own BMC. Addresses
// here are handed out by DHCP, so a written one drifts; the host itself always
// knows the current value while it is up, and that is exactly when there is
// still time to learn it.
func (c *Controller) LearnBMCAddr(host, addr string) {
	if c == nil || host == "" || addr == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.discovered[strings.ToLower(host)] = addr
}

// LocalBMCAddr asks this host's own BMC what LAN address it has, in-band.
func LocalBMCAddr(ctx context.Context) string {
	out, err := execIpmitool(ctx, "lan", "print", "1")
	if err != nil {
		return ""
	}
	return parseLANAddr(string(out))
}

func parseLANAddr(s string) string {
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "IP Address") {
			continue
		}
		addr := strings.TrimSpace(v)
		if addr == "" || addr == "0.0.0.0" {
			return ""
		}
		return addr
	}
	return ""
}

// Local issues a chassis action against this host's own BMC, in-band.
//
// Powering the machine this is running on off would take the request's own
// answer with it, so `off` and `cycle` are refused here and the caller is
// expected to reach the owning node instead. `status` and `on` are harmless:
// a running host is already on.
func (c *Controller) Local(ctx context.Context, action string) (string, error) {
	if !c.Enabled() {
		return "", fmt.Errorf("power control is not enabled")
	}
	if !ValidAction(action) {
		return "", fmt.Errorf("unknown action %q", action)
	}
	if !c.Allowed(c.selfHost) {
		return "", fmt.Errorf("host %q is not in power.control.hosts", c.selfHost)
	}
	return c.exec(ctx, nil, action)
}

// Remote issues a chassis action against another host's BMC over the network.
// This is the only path that works on a host that is powered off.
func (c *Controller) Remote(ctx context.Context, host, action string) (string, error) {
	if !c.Enabled() {
		return "", fmt.Errorf("power control is not enabled")
	}
	if !ValidAction(action) {
		return "", fmt.Errorf("unknown action %q", action)
	}
	if !c.Allowed(host) {
		return "", fmt.Errorf("host %q is not in power.control.hosts", host)
	}
	bmc, ok := c.bmcFor(host)
	if !ok {
		return "", fmt.Errorf("no out-of-band BMC configured for %q: a host that is powered off cannot be reached in-band", host)
	}
	return c.exec(ctx, &bmc, action)
}

func (c *Controller) exec(ctx context.Context, bmc *BMC, action string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, controlBudget)
	defer cancel()

	var args []string
	if bmc != nil {
		args = append(args, "-I", "lanplus", "-H", bmc.Addr, "-U", bmc.Username, "-P", bmc.Password)
	}
	args = append(args, "chassis", "power", action)

	out, err := c.run(ctx, args...)
	if err != nil {
		// ipmitool puts the useful part on stderr — "Unable to establish IPMI
		// v2 / RMCP+ session" and the like — which Output() captures into
		// ExitError.Stderr. Surfacing it is the difference between a caller
		// seeing a wrong password and seeing "exit status 1".
		return "", fmt.Errorf("ipmitool chassis power %s: %w%s", action, err, stderrOf(err))
	}
	return strings.TrimSpace(string(out)), nil
}

// stderrOf extracts ipmitool's diagnostics from a failed run. Never include the
// arguments: they carry the BMC password.
func stderrOf(err error) string {
	var ee *exec.ExitError
	if !errors.As(err, &ee) || len(ee.Stderr) == 0 {
		return ""
	}
	msg := strings.TrimSpace(string(ee.Stderr))
	if msg == "" {
		return ""
	}
	if i := strings.IndexByte(msg, '\n'); i > 0 {
		msg = msg[:i]
	}
	return ": " + msg
}
