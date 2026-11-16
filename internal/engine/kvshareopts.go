package engine

import (
	"fmt"
	"maps"
	"net"
	"path/filepath"
	"strconv"
	"strings"
)

// KVShareOptions is the kv_share: block of an engine that shares sessions
// (SessionSharer or SlotSharer), inside that engine's own block. The keys are
// the same for every engine; an engine adds its own rules and keys around it.
type KVShareOptions struct {
	// Group names the members that replicate to each other. They must serve
	// the same model files with the same engine configuration.
	Group string `yaml:"group"`
	// Transport is how the files travel; "http" (the default) is the
	// node's own API port, the only one a node serves.
	Transport string `yaml:"transport"`
	// Dir holds the replicas this node receives; "" is
	// <state_dir>/kvshare/<model>.
	Dir string `yaml:"dir"`
	// MaxGiB bounds Dir; the least recently received replicas go first.
	MaxGiB float64 `yaml:"max_gib"`
	// MinTokens: a shorter conversation is neither sent nor imported, since
	// its cold prefill costs little.
	MinTokens *int64 `yaml:"min_tokens"`
	// Local keeps this node's own copy of every session it saves, in Dir,
	// so that a session whose slot went to another conversation, or whose
	// backend restarted, is restored from it instead of prefilled again.
	// With local, group may be left out: the node then keeps its copies
	// and sends none.
	Local bool `yaml:"local"`
	// Saves is when a session is saved: "turn" (the default) after each
	// of its turns; "lazy" only when its slot is about to be given to
	// another conversation and when the node stops, which costs nothing
	// while sessions keep their slots. Lazy needs local and no group: a
	// replica host must hold a session before it spills there. A crashed
	// backend's lazy sessions are lost.
	Saves string `yaml:"saves"`
	// Peers names the address this node sends a member's replicas to, by
	// member name, in place of the member's mesh API address: two hosts on
	// one LAN move replicas over it rather than through the tailnet. A
	// member it does not name gets its mesh address. A secured mesh's receiver
	// checks a replica's signature, not where it came from; an open mesh's
	// checks the source address against the member's mesh address, so a LAN
	// address works there only if the sender's source is that address.
	Peers map[string]string `yaml:"peers"`
}

const (
	DefaultKVShareGiB       = 32
	DefaultKVShareMinTokens = 8192
)

// Validate checks the keys every engine shares; path names the block.
func (o *KVShareOptions) Validate(path string) error {
	if o == nil {
		return nil
	}
	if strings.TrimSpace(o.Group) == "" && !o.Local {
		return fmt.Errorf("%s.group is required unless local is true: the members that share this model's sessions", path)
	}
	if o.Transport != "" && o.Transport != "http" {
		return fmt.Errorf(`%s.transport must be "http" (got %q): a node serves the replicas on its API port only`, path, o.Transport)
	}
	if o.Dir != "" && !filepath.IsAbs(o.Dir) {
		return fmt.Errorf("%s.dir must be absolute (got %q)", path, o.Dir)
	}
	switch o.Saves {
	case "", "turn":
	case "lazy":
		if !o.Local || strings.TrimSpace(o.Group) != "" {
			return fmt.Errorf("%s.saves: lazy needs local: true and no group (a replica host must hold a session before it spills there)", path)
		}
	default:
		return fmt.Errorf(`%s.saves must be "turn" or "lazy" (got %q)`, path, o.Saves)
	}
	for name, addr := range o.Peers {
		if strings.TrimSpace(o.Group) == "" {
			return fmt.Errorf("%s.peers needs a group: it names where members' replicas go", path)
		}
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, " \t") {
			return fmt.Errorf("%s.peers: %q is not a node name", path, name)
		}
		host, port, err := net.SplitHostPort(addr)
		p, perr := strconv.Atoi(port)
		if err != nil || host == "" || perr != nil || p < 1 || p > 65535 {
			return fmt.Errorf("%s.peers.%s must be host:port, such as 192.168.1.20:8086 (got %q)", path, name, addr)
		}
	}
	if o.MaxGiB < 0 {
		return fmt.Errorf("%s.max_gib must not be negative (got %g)", path, o.MaxGiB)
	}
	if o.MinTokens != nil && *o.MinTokens < 0 {
		return fmt.Errorf("%s.min_tokens must not be negative (got %d)", path, *o.MinTokens)
	}
	return nil
}

// Config is the block as the node runs it, with the defaults filled in:
// saveDir is where the engine saves and restores files, maxFileBytes the
// largest file the model can write.
func (o *KVShareOptions) Config(saveDir string, maxFileBytes int64) SessionShareConfig {
	cfg := SessionShareConfig{
		Group: o.Group, Transport: o.Transport, StoreDir: o.Dir, SaveDir: saveDir,
		MaxBytes:     int64(DefaultKVShareGiB) << 30,
		MinTokens:    DefaultKVShareMinTokens,
		MaxFileBytes: maxFileBytes,
		Local:        o.Local,
		LazySaves:    o.Saves == "lazy",
		Peers:        maps.Clone(o.Peers),
	}
	if cfg.Transport == "" {
		cfg.Transport = "http"
	}
	if o.MaxGiB > 0 {
		cfg.MaxBytes = int64(o.MaxGiB * (1 << 30))
	}
	if o.MinTokens != nil {
		cfg.MinTokens = *o.MinTokens
	}
	return cfg
}
