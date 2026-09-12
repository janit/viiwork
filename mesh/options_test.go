package mesh

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

var (
	key1 = bytes.Repeat([]byte{1}, 32)
	key2 = bytes.Repeat([]byte{2}, 32)
)

func baseOptions() Options {
	return Options{
		Name: "gb1", Network: NetworkTailnet, BindPort: 7946, APIPort: 8086,
		Role: meshapi.RoleNode, Version: "2.0.0-rc.1", Enforce: EnforceFull,
		RejoinInterval: 60 * time.Second, Seeds: []string{"100.64.0.2:7946"},
	}
}

func TestOptionsValidate(t *testing.T) {
	if err := baseOptions().validate(); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	cases := []struct {
		change func(o *Options)
		want   string
	}{
		{func(o *Options) { o.Name = "" }, "mesh: name"},
		{func(o *Options) { o.Name = "gb 1" }, "mesh: name"},
		{func(o *Options) { o.Network = "wan" }, "mesh: network"},
		{func(o *Options) { o.BindPort = 0 }, "mesh: bind_port"},
		{func(o *Options) { o.APIPort = 70000 }, "mesh: api_port"},
		{func(o *Options) { o.Role = "collector" }, "mesh: role"},
		{func(o *Options) { o.SecretKey = make([]byte, 16) }, "32 bytes"},
		{func(o *Options) { o.PreviousKey = key2 }, "previous key requires"},
		{func(o *Options) { o.Enforce = EnforceOutgoing }, "requires a secret"},
		{func(o *Options) { o.SecretKey = key1; o.Enforce = "partial" }, "mesh: enforce"},
		{func(o *Options) { o.RejoinInterval = 0 }, "rejoin_interval"},
		{func(o *Options) { o.Seeds = []string{"gb1:7946"} }, "seeds[0]"},
	}
	for _, tc := range cases {
		o := baseOptions()
		tc.change(&o)
		if err := o.validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("err = %v, want it to contain %q", err, tc.want)
		}
	}

	secured := baseOptions()
	secured.SecretKey = key1
	secured.Enforce = EnforceNone
	if err := secured.validate(); err != nil {
		t.Errorf("a secret with enforce none is valid: %v", err)
	}
	if secured.mode() != meshapi.MeshSecured || baseOptions().mode() != meshapi.MeshOpen {
		t.Errorf("mode secured=%q open=%q", secured.mode(), baseOptions().mode())
	}
}

func TestAdvertiseSocket(t *testing.T) {
	cases := []struct {
		localAPI, tailnet, want string
	}{
		{"", "", DefaultTailnetSocket},
		{"", "/run/ts.sock", "/run/ts.sock"},
		{"/custom/ts.sock", "", "/custom/ts.sock"},
		{"/custom/ts.sock", "/run/ts.sock", "/custom/ts.sock"},
	}
	for _, c := range cases {
		if got := (Options{LocalAPISocket: c.localAPI, TailnetSocket: c.tailnet}).advertiseSocket(); got != c.want {
			t.Errorf("LocalAPISocket %q, TailnetSocket %q: advertiseSocket = %q, want %q", c.localAPI, c.tailnet, got, c.want)
		}
	}
}
