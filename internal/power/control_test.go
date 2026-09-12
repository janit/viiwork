package power

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// newTestController builds a controller whose ipmitool is a function, so the
// authorization checks can be exercised without a BMC anywhere near them.
func newTestController(cfg ControlConfig, self string, run func(context.Context, ...string) ([]byte, error)) (*Controller, *[][]string) {
	c := NewController(cfg, self)
	var calls [][]string
	c.run = func(ctx context.Context, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if run != nil {
			return run(ctx, args...)
		}
		return []byte("Chassis Power is on"), nil
	}
	return c, &calls
}

func TestValidAction(t *testing.T) {
	for _, a := range []string{ActionStatus, ActionOn, ActionOff, ActionCycle} {
		if !ValidAction(a) {
			t.Errorf("%q should be a valid action", a)
		}
	}
	// The action names the command that runs, so anything outside the set is
	// rejected by name rather than handed to ipmitool.
	for _, a := range []string{"", "reset", "RM -RF /", "status; reboot", "on\noff", "Status", "off "} {
		if ValidAction(a) {
			t.Errorf("%q must not be a valid action", a)
		}
	}
}

func TestAllowedIsTheWholeAuthorizationStory(t *testing.T) {
	c, _ := newTestController(ControlConfig{Enabled: true, Hosts: []string{"node-a", "node-b"}}, "node-a", nil)
	if !c.Allowed("node-a") || !c.Allowed("NODE-B") {
		t.Error("a listed host must be allowed, case-insensitively")
	}
	// No wildcard, no implicit membership: being in the mesh is not consent.
	for _, h := range []string{"", "*", "node-c", "node-a.example.com", " node-a", "node-a "} {
		if c.Allowed(h) {
			t.Errorf("%q must not be allowed: the list has no wildcard and no fuzzy matching", h)
		}
	}
	// An empty list permits nothing, rather than defaulting to everything.
	empty, _ := newTestController(ControlConfig{Enabled: true}, "node-a", nil)
	if empty.Allowed("node-a") {
		t.Error("an empty allowlist must permit nothing")
	}
	// Disabled permits nothing either, and a nil controller must not panic.
	off, _ := newTestController(ControlConfig{Hosts: []string{"node-a"}}, "node-a", nil)
	if off.Allowed("node-a") || off.Enabled() {
		t.Error("a disabled controller must allow nothing")
	}
	var nilC *Controller
	if nilC.Enabled() || nilC.Allowed("node-a") {
		t.Error("a nil controller must be safely disabled, not a panic")
	}
}

func TestLocalRefusesToCutItsOwnPower(t *testing.T) {
	c, calls := newTestController(ControlConfig{Enabled: true, Hosts: []string{"node-a"}}, "node-a", nil)
	// status and on are harmless on a host that is by definition running.
	for _, a := range []string{ActionStatus, ActionOn} {
		if _, err := c.Local(context.Background(), a); err != nil {
			t.Errorf("Local(%q) = %v, want it allowed", a, err)
		}
	}
	if n := len(*calls); n != 2 {
		t.Errorf("ipmitool called %d times, want 2", n)
	}
	// An unknown action never reaches ipmitool.
	if _, err := c.Local(context.Background(), "reboot"); err == nil {
		t.Error("Local must reject an unknown action")
	}
	if n := len(*calls); n != 2 {
		t.Errorf("a rejected action still invoked ipmitool: %d calls", n)
	}
	// A host that is not listed cannot be targeted, even by itself.
	unlisted, _ := newTestController(ControlConfig{Enabled: true, Hosts: []string{"node-b"}}, "node-a", nil)
	if _, err := unlisted.Local(context.Background(), ActionStatus); err == nil ||
		!strings.Contains(err.Error(), "not in power.control.hosts") {
		t.Errorf("Local on an unlisted self = %v, want an allowlist refusal", err)
	}
}

func TestRemoteNeedsAllowlistAndBMC(t *testing.T) {
	cfg := ControlConfig{
		Enabled: true,
		Hosts:   []string{"node-a", "node-b"},
		BMCs:    map[string]BMC{"node-b": {Addr: "198.51.100.9", Username: "u", Password: "p"}},
	}
	c, calls := newTestController(cfg, "node-a", nil)

	if _, err := c.Remote(context.Background(), "node-c", ActionOff); err == nil ||
		!strings.Contains(err.Error(), "not in power.control.hosts") {
		t.Errorf("an unlisted host = %v, want an allowlist refusal", err)
	}
	// Listed but with no out-of-band endpoint: refused with the reason, rather
	// than silently trying in-band, which cannot work on a host that is off.
	if _, err := c.Remote(context.Background(), "node-a", ActionOn); err == nil ||
		!strings.Contains(err.Error(), "no out-of-band BMC") {
		t.Errorf("listed host without a BMC = %v, want a BMC refusal", err)
	}
	if n := len(*calls); n != 0 {
		t.Fatalf("a refused Remote invoked ipmitool %d times", n)
	}

	if _, err := c.Remote(context.Background(), "node-b", ActionOff); err != nil {
		t.Fatalf("Remote on a listed host with a BMC: %v", err)
	}
	got := strings.Join((*calls)[0], " ")
	for _, want := range []string{"-I lanplus", "-H 198.51.100.9", "chassis power off"} {
		if !strings.Contains(got, want) {
			t.Errorf("ipmitool args %q missing %q", got, want)
		}
	}
}

// stderrOf surfaces ipmitool's diagnostics, which is the difference between an
// operator seeing "wrong password" and seeing "exit status 1". It must never
// include the arguments, because they carry the BMC password.
func TestStderrOfNeverLeaksArguments(t *testing.T) {
	if got := stderrOf(errors.New("plain")); got != "" {
		t.Errorf("a non-exec error should produce nothing, got %q", got)
	}
	ee := &exec.ExitError{Stderr: []byte("Error: Unable to establish IPMI v2 session\nsecond line\n")}
	got := stderrOf(ee)
	if !strings.Contains(got, "Unable to establish IPMI") {
		t.Errorf("stderrOf = %q, want the first diagnostic line", got)
	}
	if strings.Contains(got, "second line") {
		t.Errorf("only the first line should be surfaced, got %q", got)
	}
	if strings.Contains(got, "-P") || strings.Contains(got, "hunter2") {
		t.Errorf("stderrOf leaked argument-shaped content: %q", got)
	}
}

func TestParseLANAddr(t *testing.T) {
	const out = `Set in Progress         : Set Complete
IP Address Source       : DHCP Address
IP Address              : 198.51.100.9
Subnet Mask             : 255.255.255.0`
	if got := parseLANAddr(out); got != "198.51.100.9" {
		t.Errorf("parseLANAddr = %q, want the IP Address line", got)
	}
	// An unconfigured BMC reports 0.0.0.0, which is not an address to dial.
	if got := parseLANAddr("IP Address : 0.0.0.0"); got != "" {
		t.Errorf("0.0.0.0 should read as no address, got %q", got)
	}
	for _, s := range []string{"", "no colon here", "IP Address :   ", "Subnet Mask : 255.255.255.0"} {
		if got := parseLANAddr(s); got != "" {
			t.Errorf("parseLANAddr(%q) = %q, want empty", s, got)
		}
	}
}

func TestHostsIsSortedAndCopied(t *testing.T) {
	cfg := ControlConfig{Enabled: true, Hosts: []string{"node-c", "node-a", "node-b"}}
	c, _ := newTestController(cfg, "node-a", nil)
	got := c.Hosts()
	if strings.Join(got, ",") != "node-a,node-b,node-c" {
		t.Errorf("Hosts = %v, want them sorted", got)
	}
	got[0] = "mutated"
	if c.Hosts()[0] == "mutated" {
		t.Error("Hosts handed out the config's own slice")
	}
	off, _ := newTestController(ControlConfig{Hosts: []string{"node-a"}}, "node-a", nil)
	if off.Hosts() != nil {
		t.Error("a disabled controller must advertise no hosts")
	}
}
