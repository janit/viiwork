package mesh

import (
	"fmt"
	"io"
	"net/netip"
	"strings"
	"time"
	"unicode"

	"github.com/hashicorp/memberlist"
	"github.com/janit/viiwork/v2/meshapi"
)

// Enforce levels and the gossip label.
const (
	EnforceFull     = "full"
	EnforceOutgoing = "outgoing"
	EnforceNone     = "none"

	// Label is on every packet and stream. 7946 is the default port of every
	// memberlist and Serf cluster, and the label keeps an unrelated cluster
	// from ever merging with the mesh; in a secured mesh it is authenticated
	// data. Changing it splits the fleet, so it is wire format (Decision 2).
	Label = "viiwork-v2"
)

// Payload is user state carried by the mesh: P5's alias table. Callers never
// import memberlist.
type Payload interface {
	NotifyMsg(msg []byte)                   // one broadcast body; the slice is the callee's
	LocalState(join bool) []byte            // whole state for push/pull
	MergeRemoteState(buf []byte, join bool) // a peer's whole state
}

// Options configures a mesh member. It takes plain values rather than
// config.MeshConfig because the gateway, which imports this package from
// another module, has no viiwork config.
type Options struct {
	Name           string
	Network        string // NetworkTailnet | NetworkLAN
	BindPort       int
	Advertise      netip.Addr // zero = resolve from the network
	APIPort        int
	Role           string // meshapi.RoleNode | meshapi.RoleGateway
	Version        string
	SecretKey      []byte // nil = open mesh
	PreviousKey    []byte // rotation; requires SecretKey
	Enforce        string // EnforceFull | EnforceOutgoing | EnforceNone
	TailnetSocket  string // "" = no tailnet feeder
	LocalAPISocket string // resolves this node's tailnet address, discovery on or off; see advertiseSocket
	MDNS           bool
	Seeds          []string
	RejoinInterval time.Duration
	Payload        Payload           // nil = none
	OnChange       func(MemberEvent) // nil = none; called from one goroutine, in order
	Log            io.Writer         // nil = os.Stdout
	Debug          bool              // pass memberlist [DEBUG] lines through

	// Seams for tests (P3-P6 integration tests, gateway tests).
	Tune           func(*memberlist.Config) // applied last
	Feeders        []Feeder                 // nil = built from TailnetSocket, MDNS and Seeds
	AddrCheck      func(netip.Addr) error   // nil = CheckMemberAddr(Network, ·) in an open mesh
	ConflictWindow time.Duration            // 0 = 60s
}

// validate repeats C1's rules, because the gateway does not use viiwork's
// config.
func (o Options) validate() error {
	if o.Name == "" || strings.IndexFunc(o.Name, unicode.IsSpace) >= 0 {
		return fmt.Errorf("mesh: name %q must be non-empty and contain no whitespace", o.Name)
	}
	if o.Network != NetworkTailnet && o.Network != NetworkLAN {
		return fmt.Errorf("mesh: network %q must be tailnet or lan", o.Network)
	}
	if o.BindPort < 1 || o.BindPort > 65535 {
		return fmt.Errorf("mesh: bind_port must be 1-65535, got %d", o.BindPort)
	}
	if o.APIPort < 1 || o.APIPort > 65535 {
		return fmt.Errorf("mesh: api_port must be 1-65535, got %d", o.APIPort)
	}
	if o.Role != meshapi.RoleNode && o.Role != meshapi.RoleGateway {
		return fmt.Errorf("mesh: role %q must be node or gateway", o.Role)
	}
	if o.SecretKey != nil && len(o.SecretKey) != 32 {
		return fmt.Errorf("mesh: secret key is %d bytes, need exactly 32 bytes", len(o.SecretKey))
	}
	if o.PreviousKey != nil {
		if o.SecretKey == nil {
			return fmt.Errorf("mesh: previous key requires a secret key")
		}
		if len(o.PreviousKey) != 32 {
			return fmt.Errorf("mesh: previous key is %d bytes, need exactly 32 bytes", len(o.PreviousKey))
		}
	}
	switch o.Enforce {
	case EnforceFull:
	case EnforceOutgoing, EnforceNone:
		if o.SecretKey == nil {
			return fmt.Errorf("mesh: enforce %q requires a secret: levels below full exist only for adding a secret to a running open mesh", o.Enforce)
		}
	default:
		return fmt.Errorf("mesh: enforce %q must be full, outgoing or none", o.Enforce)
	}
	if o.RejoinInterval <= 0 {
		return fmt.Errorf("mesh: rejoin_interval must be positive")
	}
	for i, s := range o.Seeds {
		if _, err := netip.ParseAddrPort(s); err != nil {
			return fmt.Errorf("mesh: seeds[%d] %q must be ip:port: %w", i, s, err)
		}
	}
	return nil
}

func (o Options) mode() string {
	if o.SecretKey != nil {
		return meshapi.MeshSecured
	}
	return meshapi.MeshOpen
}

// advertiseSocket is the tailscaled socket advertise resolution asks for this
// node's own tailnet address. A tailnet node needs it even with discovery off
// (TailnetSocket empty): LocalAPISocket, else TailnetSocket, else
// DefaultTailnetSocket.
func (o Options) advertiseSocket() string {
	switch {
	case o.LocalAPISocket != "":
		return o.LocalAPISocket
	case o.TailnetSocket != "":
		return o.TailnetSocket
	}
	return DefaultTailnetSocket
}
