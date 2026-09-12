package mesh

import (
	"bytes"
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/hashicorp/memberlist"
	"github.com/janit/viiwork/v2/mesh/meshtest"
)

func TestStartInvalidOptions(t *testing.T) {
	o := baseOptions()
	o.Name = ""
	o.Advertise = netip.MustParseAddr("100.64.0.1")
	o.Feeders = []Feeder{}
	if _, err := Start(context.Background(), o); err == nil || !strings.Contains(err.Error(), "mesh: name") {
		t.Errorf("err = %v, want the validate error", err)
	}
}

func TestStartAdvertiseOutOfRange(t *testing.T) {
	o := baseOptions()
	o.Advertise = netip.MustParseAddr("192.0.2.10")
	o.Feeders = []Feeder{}
	o.Seeds = nil
	if _, err := Start(context.Background(), o); err == nil || !strings.Contains(err.Error(), "outside the tailnet range") {
		t.Errorf("open mesh: err = %v", err)
	}

	net := meshtest.NewNetwork()
	o.SecretKey = key1
	o.Log = &bytes.Buffer{}
	o.Tune = func(c *memberlist.Config) { c.Transport = net.Transport("gb1") }
	m, err := Start(context.Background(), o)
	if err != nil {
		t.Fatalf("a secured mesh does not check its own address: %v", err)
	}
	if err := m.Shutdown(); err != nil {
		t.Error(err)
	}
	if err := m.Shutdown(); err != nil {
		t.Error("Shutdown must be idempotent")
	}
}
