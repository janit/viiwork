package node

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/parrot"
	"github.com/janit/viiwork/v2/internal/release"
	"github.com/janit/viiwork/v2/internal/setup/install"
	"github.com/janit/viiwork/v2/internal/update"
	"github.com/janit/viiwork/v2/meshapi"
)

// ErrRestart is Run's return when an update asked for a restart: the node
// has done its ordered shutdown, and main now execs the launcher.
var ErrRestart = errors.New("restart requested by an update")

// confirmEvery is how often a pending release checks its baseline.
const confirmEvery = 5 * time.Second

// RequestRestart makes Run shut down in order and return ErrRestart. Only the
// first call counts.
func (n *Node) RequestRestart() { n.restartOnce.Do(func() { close(n.restart) }) }

// buildUpdate builds /v1/update and, on a node that takes part in updates,
// the confirmer for a pending release.
func (n *Node) buildUpdate(cfg *config.Config, auth update.Authorizer) (*update.Service, *update.Confirmer, error) {
	dir := update.ReleasesDir(cfg.Node.StateDir)
	keys := n.o.ReleaseKeys
	if keys == nil {
		var err error
		if keys, err = release.Keys(); err != nil {
			return nil, nil, err
		}
	}
	models := func() []config.Model { return n.runningConfig().Models }
	// Read once: running every engine's --version on each GET would cost a
	// process per engine per poll. Run starts the read; /v1/update waits for
	// it and /v1/status does not.
	n.engines.read = func() map[string]string {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		return update.InstalledEngines(ctx, n.llama.follow(models(), n.llama.pin))
	}
	// One store for the API and the confirmer: every read-modify-write of
	// state.json in this process goes through its lock.
	store := update.NewStore(dir)
	stager := &update.Stager{
		Dir: dir, Source: cfg.Update.Source, Client: &http.Client{}, Keys: keys,
		Target: release.Target{OS: runtime.GOOS, Arch: runtime.GOARCH},
		Engines: func(ctx context.Context, required map[string]string) error {
			return update.CheckEngines(ctx, models(), required)
		},
	}
	if n.o.UpdateSource != "" {
		stager.Source = n.o.UpdateSource
	}
	// The signed files come from GitHub whenever it answers; the archive from
	// the host's viiwork-parrot, checked against them. GitHub is the fallback
	// whenever parrot cannot provide it.
	pc := parrot.New(cfg.ViiworkParrot.API)
	stager.Local = func(ctx context.Context, version string) (string, error) {
		return pc.AwaitRelease(ctx, config.UpdateRepo(), version, n.meshPeers(), parrot.Await{})
	}
	stager.Log = n.logf
	if n.o.Managed {
		// The release is the image: the host's helper pulls and verifies it
		// at stage, so activation never waits on a pull, and the engine it
		// carries is what the requirements are checked against.
		helper := update.ImageHelper{Dir: dir, Wait: n.o.ImageHelperWait}
		stager.PrepareImage = func(ctx context.Context, version string, required map[string]string) error {
			engines, err := helper.Prepare(ctx, version)
			if err != nil {
				return err
			}
			return update.CheckEngineVersions(models(), required, engines)
		}
	}
	svc := &update.Service{
		Enabled: cfg.Update.Enabled, Running: n.o.Version, Store: store, Auth: auth,
		Stager:  stager,
		Managed: n.o.Managed, Stopping: n.stopping,
		Backends: n.sup.Status,
		Engines:  n.engines.Wait,
		Restart:  n.RequestRestart,
		Log:      n.logf,
	}
	if n.llama.root != "" {
		svc.Stager.PrepareLlama = n.prepareLlama(n.llamaFetch(), models)
	}
	var c *update.Confirmer
	if cfg.Update.Enabled {
		c = &update.Confirmer{
			Store: store, Running: n.o.Version, Window: cfg.ConfirmWindow(),
			Backends: n.sup.Status, Wanted: n.wantsBackend,
			Restart: n.RequestRestart, Now: time.Now, Log: n.logf,
			Managed: n.o.Managed,
		}
		svc.Deadline = c.Deadline
		if n.llama.root != "" {
			c.PruneEngines = n.pruneLlama(dir)
		}
		if cli := n.installedCLI(); cli != "" {
			c.FollowCLI = func(confirmed update.State) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if err := store.FollowCLI(ctx, confirmed.Current, cli, n.logf); err != nil {
					n.logf("update: %s does not follow %s: %v", cli, confirmed.Current, err)
				}
			}
		}
	}
	return svc, c, nil
}

// installedCLI is the CLI a Mac wizard install recorded in its manifest,
// which is also its LaunchAgent's binary: the node runs as the user who owns
// it, so a confirmed release installs itself there (update.Store.FollowCLI).
// "" without such a manifest — on Linux, where the host's engine helper or
// `viiwork update cli` does it as root, and the node never writes the host.
func (n *Node) installedCLI() string {
	if n.o.InstallManifest == "" {
		return ""
	}
	man, err := install.ReadManifest(n.o.InstallManifest)
	if err != nil || man.OS != "darwin" || !filepath.IsAbs(man.Binary) {
		return ""
	}
	return man.Binary
}

// meshPeers is the address of every other alive member: likely holders of a
// release, handed to viiwork-parrot as hints.
func (n *Node) meshPeers() []string {
	m := n.mesh.Load()
	if m == nil {
		return nil
	}
	var out []string
	for _, mem := range m.Members() {
		if mem.State == meshapi.MemberAlive && !mem.Local && mem.Meta.Role == meshapi.RoleNode {
			out = append(out, mem.Addr.String())
		}
	}
	return out
}

// wantsBackend reports whether the backend of that ID belongs to a model
// this node is configured with and has not parked.
func (n *Node) wantsBackend(id string) bool {
	i := strings.LastIndexByte(id, '/')
	if i < 0 {
		return true
	}
	model := id[:i]
	if n.isParked(model) {
		return false
	}
	return slices.ContainsFunc(n.runningConfig().Models, func(m config.Model) bool { return m.Name == model })
}
