package mesh

import (
	"fmt"
	"log"
	"net/netip"
	"time"

	"github.com/hashicorp/memberlist"
)

// deadNodeReclaimTime lets a name held by a dead member move to a new address
// after 30 s: a LAN host whose DHCP lease changed across a power cycle gets
// its name back. memberlist's default of 0 locks the name to the dead address
// forever (Decision 3).
const deadNodeReclaimTime = 30 * time.Second

// delegates is everything memberlist calls back into.
type delegates interface {
	memberlist.Delegate
	memberlist.EventDelegate
	memberlist.ConflictDelegate
	memberlist.AliveDelegate
}

// buildMemberlistConfig starts from DefaultLANConfig and binds gossip to the
// advertise address, never 0.0.0.0, so a machine with a public interface never
// exposes 7946 on it.
//
// A secured mesh gets a keyring with the secret as primary and the previous
// key installed. GossipVerifyIncoming and GossipVerifyOutgoing follow the
// enforce level, which is exactly memberlist's documented way of adding
// encryption to a running cluster: none accepts and sends plaintext while
// able to decrypt, outgoing sends encrypted but still accepts plaintext, full
// requires encryption both ways. An open mesh gets no keyring.
func buildMemberlistConfig(o Options, advertise netip.Addr, d delegates, logger *log.Logger) (*memberlist.Config, error) {
	c := memberlist.DefaultLANConfig()
	c.Name = o.Name
	c.BindAddr = advertise.String()
	c.AdvertiseAddr = advertise.String()
	c.BindPort = o.BindPort
	c.AdvertisePort = o.BindPort
	c.Label = Label
	c.DeadNodeReclaimTime = deadNodeReclaimTime
	c.Delegate = d
	c.Events = d
	c.Conflict = d
	c.Alive = d
	c.Logger = logger

	if o.SecretKey != nil {
		var others [][]byte
		if o.PreviousKey != nil {
			others = append(others, o.PreviousKey)
		}
		ring, err := memberlist.NewKeyring(others, o.SecretKey)
		if err != nil {
			return nil, fmt.Errorf("mesh: building the keyring: %w", err)
		}
		c.Keyring = ring
		c.GossipVerifyIncoming = o.Enforce == EnforceFull
		c.GossipVerifyOutgoing = o.Enforce != EnforceNone
	}

	if o.Tune != nil {
		o.Tune(c)
	}
	return c, nil
}
