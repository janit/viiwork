package mesh

import (
	"bytes"
	"io"
	"log"
	"net/netip"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
)

// noopDelegates satisfies delegates for configuration tests.
type noopDelegates struct{}

func (noopDelegates) NodeMeta(int) []byte                  { return nil }
func (noopDelegates) NotifyMsg([]byte)                     {}
func (noopDelegates) GetBroadcasts(int, int) [][]byte      { return nil }
func (noopDelegates) LocalState(bool) []byte               { return nil }
func (noopDelegates) MergeRemoteState([]byte, bool)        {}
func (noopDelegates) NotifyJoin(*memberlist.Node)          {}
func (noopDelegates) NotifyLeave(*memberlist.Node)         {}
func (noopDelegates) NotifyUpdate(*memberlist.Node)        {}
func (noopDelegates) NotifyConflict(_, _ *memberlist.Node) {}
func (noopDelegates) NotifyAlive(*memberlist.Node) error   { return nil }

func buildConfig(t *testing.T, o Options) *memberlist.Config {
	t.Helper()
	c, err := buildMemberlistConfig(o, netip.MustParseAddr("100.64.0.105"), noopDelegates{}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestBuildMemberlistConfig(t *testing.T) {
	secured := baseOptions()
	secured.SecretKey = key1
	c := buildConfig(t, secured)
	if c.BindAddr != "100.64.0.105" || c.AdvertiseAddr != "100.64.0.105" || c.BindPort != 7946 || c.AdvertisePort != 7946 {
		t.Errorf("addresses: bind %s:%d advertise %s:%d", c.BindAddr, c.BindPort, c.AdvertiseAddr, c.AdvertisePort)
	}
	if c.Label != "viiwork-v2" || c.DeadNodeReclaimTime != 30*time.Second || c.Name != "gb1" {
		t.Errorf("label=%q reclaim=%v name=%q", c.Label, c.DeadNodeReclaimTime, c.Name)
	}
	if !c.EncryptionEnabled() || len(c.Keyring.GetKeys()) != 1 || !bytes.Equal(c.Keyring.GetPrimaryKey(), key1) {
		t.Error("secured: want one key, the secret as primary")
	}
	if !c.GossipVerifyIncoming || !c.GossipVerifyOutgoing {
		t.Error("enforce full verifies both directions")
	}

	withPrev := secured
	withPrev.PreviousKey = key2
	c = buildConfig(t, withPrev)
	if len(c.Keyring.GetKeys()) != 2 || !bytes.Equal(c.Keyring.GetPrimaryKey(), key1) {
		t.Error("with a previous key: want two keys, the secret still primary")
	}

	outgoing := secured
	outgoing.Enforce = EnforceOutgoing
	if c = buildConfig(t, outgoing); c.GossipVerifyIncoming || !c.GossipVerifyOutgoing {
		t.Error("enforce outgoing: incoming false, outgoing true")
	}

	none := secured
	none.Enforce = EnforceNone
	if c = buildConfig(t, none); c.GossipVerifyIncoming || c.GossipVerifyOutgoing || !c.EncryptionEnabled() {
		t.Error("enforce none: both false, encryption still enabled")
	}

	if c = buildConfig(t, baseOptions()); c.Keyring != nil || c.EncryptionEnabled() {
		t.Error("open mesh: no keyring")
	}

	tuned := baseOptions()
	tuned.Tune = func(c *memberlist.Config) { c.BindPort = 1 }
	if c = buildConfig(t, tuned); c.BindPort != 1 {
		t.Error("Tune must run last")
	}
}
