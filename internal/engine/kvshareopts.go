package engine

import (
	"fmt"
	"path/filepath"
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
