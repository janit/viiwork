package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/janit/viiwork/v2/energy"
	"github.com/janit/viiwork/v2/internal/activity"
	"github.com/janit/viiwork/v2/internal/alias"
	"github.com/janit/viiwork/v2/internal/catalog"
	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/cost"
	"github.com/janit/viiwork/v2/internal/discovery"
	_ "github.com/janit/viiwork/v2/internal/engine/all" // registers every engine (contract C7)
	"github.com/janit/viiwork/v2/internal/gpu"
	"github.com/janit/viiwork/v2/internal/modelinfo"
	"github.com/janit/viiwork/v2/internal/parrot"
	"github.com/janit/viiwork/v2/internal/perf"
	"github.com/janit/viiwork/v2/internal/pipeline"
	"github.com/janit/viiwork/v2/internal/power"
	"github.com/janit/viiwork/v2/internal/proxy"
	"github.com/janit/viiwork/v2/internal/route"
	"github.com/janit/viiwork/v2/internal/supervisor"
	"github.com/janit/viiwork/v2/internal/update"
	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/mesh/capacity"
	"github.com/janit/viiwork/v2/meshapi"
)

const (
	// statusPollInterval is how often members' /v1/status is polled (P6
	// Decision 1).
	statusPollInterval = 5 * time.Second
	// gpuHistorySamples is an hour of GPU samples at 5 s (v1).
	gpuHistorySamples = 720
	leaveTimeout      = 5 * time.Second
	supervisorStop    = 10 * time.Second
)

type Options struct {
	ConfigPath string
	Version    string
	Log        io.Writer                                        // nil = os.Stdout
	LookupEnv  func(string) (string, bool)                      // nil = os.LookupEnv
	Hostname   func() (string, error)                           // nil = os.Hostname
	Listen     func(network, addr string) (net.Listener, error) // nil = net.Listen
	GPURunner  gpu.Runner                                       // nil = gpu.ExecRunner
	MeshTune   func(*mesh.Options)                              // test seam, applied last
	// MagicDNS reads this tailnet's MagicDNS suffix for the derived CORS
	// allowlist. nil = mesh.ReadMagicDNSSuffix.
	MagicDNS         func(ctx context.Context, socket string) (string, error)
	SupervisorTiming supervisor.Timing // test seam; zero fields take the defaults
	// ReleaseKeys verifies staged releases. Test seam; nil = release.Keys().
	ReleaseKeys []ed25519.PublicKey
	// UpdateSource replaces update.source (GitHub). Test seam; "" = the config's.
	UpdateSource string
	// LlamaPin is the llama.cpp release this binary carries (--build-info).
	LlamaPin string
	// InstallManifest is the setup wizard's install.json on a host where it
	// installs natively (a Mac); "" on any other. Its llama root is where a
	// model's managed llama-server follows LlamaPin.
	InstallManifest string
	// Managed is a Docker install whose host helper makes the image follow
	// the release (install.ManagedInstall): the release is the image, and
	// activate and rollback leave the restart to the helper.
	Managed bool
	// ImageHelperWait bounds a stage's wait for the helper's image. Test
	// seam; 0 = update.ImageHelper's default.
	ImageHelperWait time.Duration
}

// Node is one viiwork 2 node: its models, its mesh membership, its router and
// its HTTP API.
type Node struct {
	o       Options
	logger  *log.Logger
	name    string
	nodeID  string
	started time.Time

	mu  sync.Mutex
	cfg *config.Config // the running configuration; Reload replaces Models

	// applyMu orders every change to what the supervisor runs: Run's first
	// apply, Reload and Park. It is taken before mu, never after.
	applyMu       sync.Mutex
	parked        map[string]bool // models parked by viiwork down; applyMu
	modelsApplied bool            // Run has applied the models; applyMu
	parkedView    atomic.Pointer[map[string]bool]

	vendor      gpu.Vendor
	history     *gpu.History
	broadcaster *gpu.Broadcaster
	collector   gpu.Collector
	inventory   []gpu.Identity
	sampler     *power.Sampler
	power       NodePower
	costTracker *cost.Tracker
	energyStore *energy.Store
	activity    *activity.Log

	sup          *supervisor.Supervisor
	counters     *proxy.Counters
	perf         *perf.Tracker // this node's measured speed per model; saved under state_dir
	capPoller    *capacity.Poller
	router       *route.Router
	statusPoller *StatusPoller
	aliasService *alias.Service
	resolver     *alias.Resolver
	powerCtl     *power.Controller
	meshOpts     mesh.Options

	mesh         atomic.Pointer[mesh.Mesh]
	apiPort      atomic.Int32
	apiAddr      atomic.Value // string
	selfAddr     atomic.Value // string: where this node dials its own API
	ready        chan struct{}
	streamCtx    context.Context
	streamCancel context.CancelFunc
	handler      http.Handler
	restart      chan struct{}
	restartOnce  sync.Once
	confirmer    *update.Confirmer
	llama        llamaFollow
	engines      engineVersions
	updateSvc    *update.Service
	stopping     chan struct{} // closed when Run starts shutting down
	stoppingOnce sync.Once
}

// members is the mesh's member list once it has started, else empty. The
// poller, forward authentication and the server are built before the mesh.
type members struct{ n *Node }

func (m members) Members() []mesh.Member {
	if mm := m.n.mesh.Load(); mm != nil {
		return mm.Members()
	}
	return nil
}

func generateNodeID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("viiwork-%x", b)
}

// New builds a node from a loaded configuration. Nothing runs until Run.
func New(cfg *config.Config, o Options) (*Node, error) {
	if o.Log == nil {
		o.Log = os.Stdout
	}
	if o.LookupEnv == nil {
		o.LookupEnv = os.LookupEnv
	}
	if o.Hostname == nil {
		o.Hostname = os.Hostname
	}
	if o.Listen == nil {
		o.Listen = net.Listen
	}
	if o.GPURunner == nil {
		o.GPURunner = gpu.ExecRunner
	}
	if o.MagicDNS == nil {
		o.MagicDNS = mesh.ReadMagicDNSSuffix
	}
	running := *cfg
	n := &Node{o: o, logger: log.New(o.Log, "", log.LstdFlags), cfg: &running, started: time.Now(), ready: make(chan struct{}),
		parked: map[string]bool{}}
	n.streamCtx, n.streamCancel = context.WithCancel(context.Background())
	n.llama = newLlamaFollow(o, cfg.Models, n.logf)
	n.apiAddr.Store("")
	n.selfAddr.Store("")

	// 1. Identity.
	n.name = cfg.Node.Name
	if n.name == "" {
		host, err := o.Hostname()
		if err != nil || host == "" {
			return nil, fmt.Errorf("node.name is empty and the hostname is unknown: %v", err)
		}
		n.name = host
	}
	n.nodeID = generateNodeID()

	// 2. Pipelines and mesh keys.
	var pipelines []*pipeline.Pipeline
	names := make([]string, 0, len(cfg.Pipelines))
	for name := range cfg.Pipelines {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p, err := pipeline.LoadPipeline(name, cfg.Pipelines[name])
		if err != nil {
			return nil, fmt.Errorf("loading pipeline %s: %w", name, err)
		}
		pipelines = append(pipelines, p)
		n.logf("pipeline '%s' loaded with %d locales, %d steps", name, len(p.Locales), len(p.Steps))
	}
	keys, err := cfg.MeshKeys(o.LookupEnv)
	if err != nil {
		return nil, err
	}

	// 3. GPUs and IPMI.
	if cfg.GPU.Vendor == config.VendorAuto {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		n.vendor = gpu.Detect(ctx, o.GPURunner)
		cancel()
	} else if n.vendor, err = gpu.ParseVendor(cfg.GPU.Vendor); err != nil {
		return nil, err
	}
	n.history = gpu.NewHistory(gpuHistorySamples)
	n.broadcaster = gpu.NewBroadcaster()
	n.collector = gpu.NewCollector(n.vendor, n.history, n.broadcaster)
	if n.vendor != gpu.VendorNone {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		n.inventory, err = gpu.Inventory(ctx, n.vendor, o.GPURunner)
		cancel()
		if err != nil {
			n.logf("gpu inventory: %v", err)
		}
	}
	n.sampler = power.NewSampler(cfg.Power.Source)

	// 4. Power, cost, energy and activity.
	gpuPower := n.collector != nil && n.collector.PowerAvailable()
	var gpus gpuLatest
	if n.collector != nil {
		gpus = n.history
	}
	n.power = NewNodePower(n.sampler, gpus, n.vendor, gpuPower)
	apiKey, _ := o.LookupEnv("ENTSOE_API_KEY")
	n.costTracker = newCostTracker(cfg.Cost, apiKey, n.power, n.logf)
	if cfg.Energy.Enabled {
		ids := n.history.GPUIDs()
		if len(ids) == 0 {
			ids = modelGPUIDs(cfg.Models)
		}
		if n.energyStore, err = openEnergy(cfg.Energy, cfg.Cost.Timezone, ids, n.power, n.logf); err != nil {
			n.logf("[energy] disabled: %v", err)
			n.energyStore = nil
		}
	}
	n.activity = activity.NewLogWithHistory(cfg.Activity.PromptHistory, cfg.Activity.EventHistory)

	// 5. The alias store; a damaged table stops startup (P5 Decision 13).
	store, err := alias.Open(cfg.Node.StateDir, n.name, nil)
	if err != nil {
		return nil, err
	}

	// 6. The supervisor.
	n.sup = supervisor.New(supervisor.Deps{
		Vendor: n.vendor, Run: o.GPURunner, Health: cfg.Health,
		PowerLimitWatts: cfg.GPU.PowerLimitWatts, Log: o.Log, Events: n.activity,
		// A client makes no connection until a sourced model asks, so a
		// node with only path models never talks to viiwork-parrot.
		Resolver: parrot.New(cfg.ViiworkParrot.API),
		Timing:   o.SupervisorTiming,
		Environ:  engineEnviron(cfg),
	})

	// 7. Routing, aliases and the inference handler.
	mem := members{n}
	n.counters = proxy.NewCounters()
	auth, err := proxy.NewForwardAuth(n.name, keys.Primary, keys.Previous, mem)
	if err != nil {
		return nil, err
	}
	n.capPoller = capacity.NewPoller(capacity.Config{
		Self: n.name, Members: mem, Interval: cfg.Mesh.CapacityPoll.Duration,
		OnReport: func(string) { n.router.Wake() }, Logf: n.logf,
	})
	// A corrupt perf.json or an unreadable state_dir costs minutes of
	// learning, never a start.
	n.perf = perf.New(time.Now)
	if err := n.perf.Load(cfg.Node.StateDir); err != nil {
		n.logf("%v; starting without saved performance baselines", err)
	}
	n.perf.SetKeys(perfKeys(cfg.Models))
	n.router = route.New(route.Config{
		Self: n.name, Local: parkedLocal{inner: route.SupervisorModels(n.sup), n: n}, Remote: n.capPoller,
		StaleAfter: cfg.Routing.StaleAfter.Duration, QueueMax: cfg.Routing.QueueMax,
		QueueTimeout: cfg.Routing.QueueTimeout.Duration,
		Performance:  cfg.Routing.PerformanceOn(),
		LocalScore: func(model string) (int, int, bool) {
			s, ok := n.perf.Score(model)
			return s.OverheadMs, s.MsPer1k, ok
		},
	})
	resolverPipelines, err := proxy.NewPipelineResolver(pipelines)
	if err != nil {
		return nil, err
	}
	pipelineNames := func() []string {
		out := append([]string(nil), names...)
		return append(out, resolverPipelines.VirtualModelNames()...)
	}
	served := alias.NewServedView(n.name, localCapacity{n}, n.capPoller, cfg.Routing.StaleAfter.Duration, nil)
	n.resolver = alias.NewResolver(store, served, pipelineNames, n.logf)
	n.aliasService = alias.NewService(store, n.resolver, func(key string, msg []byte) {
		if m := n.mesh.Load(); m != nil {
			m.Broadcast(key, msg)
		}
	}, n.activity, n.logf)
	aliasAuth, err := alias.NewAuthorizer(n.name, keys.Primary, keys.Previous)
	if err != nil {
		return nil, err
	}
	n.restart = make(chan struct{})
	n.stopping = make(chan struct{})
	updateSvc, confirmer, err := n.buildUpdate(cfg, aliasAuth)
	if err != nil {
		return nil, err
	}
	n.confirmer, n.updateSvc = confirmer, updateSvc
	var executor *pipeline.Executor
	if len(pipelines) > 0 {
		// Steps call back into this node's own API. The host in the base URL
		// is a placeholder: selfTransport dials the address the listener
		// actually bound, which is only known once Run has listened.
		executor = pipeline.NewExecutor("http://"+selfHost, n.selfClient())
	}
	inference := proxy.NewHandler(proxy.Deps{
		Self: n.name, Version: o.Version, Router: n.router, Reports: n.capPoller, Local: localCapacity{n},
		Auth: auth, Counters: n.counters, ForwardRetry: cfg.Routing.ForwardRetry, Activity: n.activity,
		StaleAfter: cfg.Routing.StaleAfter.Duration,
		Pipelines:  resolverPipelines, PipelineExec: executor,
		Resolve: n.resolver.Resolve, ExtraModels: n.resolver.ModelEntries,
		Perf: n.perf, PublishPerf: cfg.Routing.PerformanceOn(), Usage: n.usageReporting,
		SeparateReasoning: n.separatesReasoning,
	})

	// 8. Member statuses and chassis power control.
	n.statusPoller = NewStatusPoller(n.name, mem, statusPollInterval, nil, n.logf)
	n.powerCtl = newPowerController(cfg, n.name, o.LookupEnv, n.logf)

	// 9. The server and the mesh options.
	cors := n.buildCORS(cfg)
	n.handler = NewServer(ServerDeps{
		Self: n.name, Version: o.Version, Started: n.started, StreamCtx: n.streamCtx,
		Catalog: buildCatalog(cfg, inference),
		// Unconditional, unlike the OpenCode catalogue: this renders exactly
		// what /v1/models already publishes, in another client's spelling, so
		// there is no exposure to opt out of. api.catalog.enabled exists
		// because /api.json REPLACES a client's whole catalogue, which is a
		// decision about that client rather than about this node.
		ModelInfo: modelinfo.NewHandler(inference),
		Inference: inference, Aliases: alias.NewHandler(n.aliasService, aliasAuth),
		AliasInfo: func() meshapi.AliasesResponse { return meshapi.AliasesResponse{Aliases: n.resolver.Info()} },
		Status:    n.status, Cluster: n.cluster, Members: mem.Members, Activity: n.activity,
		GPUHistory: n.history, GPUBroadcaster: n.broadcaster,
		GPUAvailable: func() bool { return n.collector != nil && n.collector.Available() },
		PowerControl: n.powerCtl, CORS: cors, Health: n.health,
		Update: updateSvc,
		Park:   &parkHandler{auth: aliasAuth, park: n.Park},
	})
	n.meshOpts = n.buildMeshOptions(cfg, keys)
	return n, nil
}

// magicDNSTimeout bounds the startup question to tailscaled. A daemon that
// does not answer in that time is treated as absent.
const magicDNSTimeout = 3 * time.Second

// buildCORS returns the origin allowlist, or nil for none. An explicit
// api.cors.allow_origins is used exactly as written ([] meaning no CORS). Left
// unset, the list is derived: this tailnet's own MagicDNS domain as reported
// by tailscaled, plus localhost and 127.0.0.1. Never "*.ts.net", which is
// every Tailscale customer's domain, Funnel pages included; when tailscaled
// cannot say, the node falls back to the local origins (and tailnet IP
// literals) and says so.
func (n *Node) buildCORS(cfg *config.Config) *CORS {
	c := cfg.API.CORS
	tailnetIPs := c.AllowTailnetIPs == nil || *c.AllowTailnetIPs
	if !c.Derive() {
		if len(c.AllowOrigins) == 0 {
			return nil
		}
		return &CORS{Origins: c.AllowOrigins, TailnetIPs: tailnetIPs}
	}
	origins := config.LocalCORSOrigins()
	ctx, cancel := context.WithTimeout(context.Background(), magicDNSTimeout)
	suffix, err := n.o.MagicDNS(ctx, cfg.Mesh.Tailnet.Socket)
	cancel()
	literals := ""
	if tailnetIPs {
		literals = " and tailnet IP literals"
	}
	if err == nil && suffix != "" {
		origins = append(origins, "*."+suffix)
		n.logf("api.cors: allowing browser origins %v%s (this tailnet's MagicDNS domain, from tailscaled); set api.cors.allow_origins to choose", origins, literals)
	} else {
		n.logf("api.cors: this tailnet's MagicDNS domain is unknown (%v); allowing only browser origins %v%s. Set api.cors.allow_origins to add your tailnet's domain, e.g. \"*.tailXXXX.ts.net\"", err, origins, literals)
	}
	return &CORS{Origins: origins, TailnetIPs: tailnetIPs}
}

func (n *Node) buildMeshOptions(cfg *config.Config, keys config.MeshKeys) mesh.Options {
	o := mesh.Options{
		Name: n.name, Network: cfg.Mesh.Network, BindPort: cfg.Mesh.BindPort,
		APIPort: cfg.API.Port, Role: meshapi.RoleNode, Version: n.o.Version,
		SecretKey: keys.Primary, PreviousKey: keys.Previous, Enforce: cfg.Mesh.SecretEnforce,
		LocalAPISocket: cfg.Mesh.Tailnet.Socket,
		MDNS:           cfg.Mesh.LAN.MDNS.Resolve(cfg.Mesh.Network == config.NetworkLAN),
		Seeds:          cfg.Mesh.Seeds, RejoinInterval: cfg.Mesh.RejoinInterval.Duration,
		Payload: n.aliasService,
		OnChange: func(ev mesh.MemberEvent) {
			n.capPoller.HandleMemberEvent(ev)
			n.statusPoller.HandleMemberEvent(ev)
			n.router.Wake()
		},
		Log: n.o.Log,
	}
	if cfg.Mesh.Advertise != "" {
		o.Advertise, _ = netip.ParseAddr(cfg.Mesh.Advertise) // validated by config.Load
	}
	if cfg.Mesh.Tailnet.Enabled.Resolve(cfg.Mesh.Network == config.NetworkTailnet) {
		o.TailnetSocket = cfg.Mesh.Tailnet.Socket
	}
	return o
}

func (n *Node) logf(format string, args ...any) { n.logger.Printf(format, args...) }

// Name is the node's mesh name.
func (n *Node) Name() string { return n.name }

// APIAddr is the API listener's actual address, once Run has started serving.
func (n *Node) APIAddr() string { return n.apiAddr.Load().(string) }

// Ready is closed once the API listens and the mesh has started.
func (n *Node) Ready() <-chan struct{} { return n.ready }

func (n *Node) runningConfig() *config.Config {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cfg
}

func (n *Node) status() meshapi.NodeStatus {
	cfg := n.runningConfig()
	var gpus gpuLatest
	if n.collector != nil {
		gpus = n.history
	}
	var energyReader EnergyReader
	if n.energyStore != nil {
		energyReader = n.energyStore
	}
	var costReader CostReader
	if n.costTracker != nil {
		costReader = n.costTracker
	}
	return BuildStatus(StatusSources{
		Name: n.name, NodeID: n.nodeID, Version: n.o.Version, APIPort: n.actualAPIPort(cfg),
		Advertise: func() netip.Addr {
			if m := n.mesh.Load(); m != nil {
				return m.Advertise()
			}
			return netip.Addr{}
		},
		Started: n.started, Models: n.modelStatuses, QueueLen: n.router.QueueLen, Counters: n.counters.Get,
		Perf: n.perf.Score, EngineVersions: n.engines.Known,
		GPUs: gpus, Inventory: n.inventory, Vendor: n.vendor, Power: n.power,
		Energy: energyReader, Cost: costReader, PromptHistory: n.activity.PromptHistoryMax(),
	})
}

func (n *Node) actualAPIPort(cfg *config.Config) int {
	if p := n.apiPort.Load(); p > 0 {
		return int(p)
	}
	return cfg.API.Port
}

func (n *Node) cluster() meshapi.ClusterResponse {
	return BuildCluster(ClusterSources{
		Self: n.name,
		Mode: func() string {
			if m := n.mesh.Load(); m != nil {
				return m.Mode()
			}
			return ""
		},
		Members: members{n}.Members, Local: n.status, Remote: n.statusPoller, PowerControl: n.powerCtl,
		// Four missed polls: a member alive in gossip whose API is wedged.
		StatusStaleAfter: 4 * statusPollInterval,
	})
}

// health counts healthy backends for /health (P6 Decision 5).
// health counts the models from the running configuration and the backends
// from the supervisor. The two sources are deliberate: Run serves the API
// before mesh.Start and only reaches Supervisor.Apply after it, so a node that
// is still joining — or one that cannot reach tailscaled, and so never will —
// has a supervisor with no models in it. Counting models there let /health
// answer 200 "ok" indefinitely for a node serving nothing, which is what
// Decision 5's 503 exists to prevent (found by the gb1 trial, 2026-09-12).
//
// A parked model is not counted: a node whose models are all parked serves
// nothing by choice, like a router, and must not read as broken.
func (n *Node) health() (healthy, total, models int) {
	for _, m := range n.runningConfig().Models {
		if !n.isParked(m.Name) {
			models++
		}
	}
	for _, m := range n.sup.Status() {
		for _, b := range m.Backends {
			total++
			if b.Status == meshapi.StatusHealthy {
				healthy++
			}
		}
	}
	return healthy, total, models
}

// owners is the GPU-to-model layout of the running configuration.
func (n *Node) owners() map[int]string { return gpuOwners(n.runningConfig().Models) }

// Run serves until ctx ends or the mesh reports a fatal error, then shuts down
// in order (P6 Decision 7).
func (n *Node) Run(ctx context.Context) error {
	cfg := n.runningConfig()
	ln, err := n.o.Listen("tcp", net.JoinHostPort(cfg.API.Host, strconv.Itoa(cfg.API.Port)))
	if err != nil {
		return fmt.Errorf("api listen: %w", err)
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		n.apiPort.Store(int32(tcp.Port))
	}
	n.apiAddr.Store(ln.Addr().String())
	n.selfAddr.Store(selfDialAddr(ln.Addr()))
	if wildcardHost(cfg.API.Host) {
		n.logf("api: listening on every network interface (api.host %q). The API authenticates nothing — "+
			"inference, prompt history, aliases and power control are open to every network this machine is on. "+
			"Set api.host to this machine's tailnet or LAN address to narrow it", cfg.API.Host)
	}
	// No write timeout: streams would break. v1's header and idle timeouts.
	srv := &http.Server{Handler: n.handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			n.logf("api: %v", err)
		}
	}()

	opts := n.meshOpts
	opts.APIPort = int(n.apiPort.Load())
	if n.o.MeshTune != nil {
		n.o.MeshTune(&opts)
	}
	m, err := mesh.Start(ctx, opts)
	if err != nil {
		srv.Close()
		<-serveDone
		return err
	}
	n.mesh.Store(m)
	close(n.ready)
	n.logf("viiwork %s: node %s serving on %s, %s mesh", n.o.Version, n.name, ln.Addr(), m.Mode())
	// Not a loop and not waited for at shutdown: it ends within its own
	// minute, and a status built before then carries no engine version.
	go n.engines.Wait()

	loopCtx, stopLoops := context.WithCancel(context.Background())
	var loops sync.WaitGroup
	goLoop := func(f func(context.Context)) {
		loops.Add(1)
		go func() { defer loops.Done(); f(loopCtx) }()
	}
	goLoop(n.capPoller.Run)
	goLoop(n.statusPoller.Run)
	goLoop(n.router.Run)
	goLoop(n.aliasService.Run)
	goLoop(n.sampleLoop)
	goLoop(n.perfSaveLoop)
	if n.energyStore != nil {
		ids := n.history.GPUIDs()
		if len(ids) == 0 {
			ids = modelGPUIDs(cfg.Models)
		}
		recorder := newRecorder(n.energyStore, cfg.Energy.SampleInterval.Duration, n.power, gpuReadings(n.history, ids, n.owners))
		goLoop(func(ctx context.Context) {
			recorder.Run(ctx) // flushes the bucket in progress on the way out
			n.energyStore.Close()
		})
	}

	if n.confirmer != nil {
		goLoop(func(ctx context.Context) { n.confirmer.Run(ctx, confirmEvery) })
	}
	if cfg.Update.Enabled {
		n.updateSvc.WatchPending()
	}

	var runErr error
	if err := n.applyRunning(); err != nil {
		runErr = fmt.Errorf("applying models: %w", err)
	} else {
		select {
		case <-ctx.Done():
		case runErr = <-m.Fatal():
		case <-n.restart:
			runErr = ErrRestart
		}
	}
	// On a managed install this is the helper's compose up at work: an
	// update's wait for it stands down.
	n.stoppingOnce.Do(func() { close(n.stopping) })
	n.shutdown(srv, m, cfg)
	stopLoops()
	loops.Wait()
	<-serveDone
	return runErr
}

// shutdown is P6 Decision 7's order: members stop routing here first, streams
// that never finish on their own are cancelled, in-flight requests get
// respawn_grace, then the backends (idle by now) stop, the mesh closes, and the
// loops (with the energy flush) end in the caller.
func (n *Node) shutdown(srv *http.Server, m *mesh.Mesh, cfg *config.Config) {
	n.logf("shutting down")
	if err := m.Leave(leaveTimeout); err != nil {
		n.logf("mesh leave: %v", err)
	}
	n.streamCancel()
	httpCtx, cancel := context.WithTimeout(context.Background(), cfg.Health.RespawnGrace.Duration)
	if err := srv.Shutdown(httpCtx); err != nil {
		n.logf("api shutdown: %v", err)
		srv.Close()
	}
	cancel()
	supCtx, cancel := context.WithTimeout(context.Background(), supervisorStop)
	n.sup.Shutdown(supCtx)
	cancel()
	if err := m.Shutdown(); err != nil {
		n.logf("mesh shutdown: %v", err)
	}
}

// sampleLoop is v1's health-loop extras: IPMI, cost and GPU samples every
// health.interval.
func (n *Node) sampleLoop(ctx context.Context) {
	interval := n.runningConfig().Health.Interval.Duration
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.sampler.Sample(ctx)
			if n.costTracker != nil {
				n.costTracker.Update(ctx)
			}
			if n.collector != nil {
				n.collector.Sample(ctx)
			}
		}
	}
}

// newPowerController builds the chassis power controller, or nil when power
// control is not configured. Nil rather than a disabled instance so the
// endpoints answer "not enabled on this node" from one check. Ported from v1,
// with the node name as the host name.
func newPowerController(cfg *config.Config, hostname string, lookupEnv func(string) (string, bool), logf func(string, ...any)) *power.Controller {
	pc := cfg.Power.Control
	if !pc.Enabled {
		return nil
	}

	pass := pc.BMC.Password
	if pass == "" {
		env := pc.BMC.PasswordEnv
		if env == "" {
			env = "BMC_PASSWORD"
		}
		pass, _ = lookupEnv(env)
	}

	bmcs := make(map[string]power.BMC, len(pc.BMC.Addresses))
	for host, addr := range pc.BMC.Addresses {
		bmcs[host] = power.BMC{Addr: addr, Username: pc.BMC.Username, Password: pass}
	}
	// A host named for control but given no address still gets an entry, so it
	// can pick up the address it reports for itself while it is up. Written
	// addresses go stale here -- these BMCs are on DHCP.
	for _, host := range pc.Hosts {
		if _, ok := bmcs[host]; !ok {
			bmcs[host] = power.BMC{Username: pc.BMC.Username, Password: pass}
		}
	}

	ctl := power.NewController(power.ControlConfig{Enabled: true, Hosts: pc.Hosts, BMCs: bmcs}, hostname)

	// Learn this host's own BMC address, so members can reach it once this
	// node is no longer running to be asked.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if addr := power.LocalBMCAddr(ctx); addr != "" {
		ctl.LearnBMCAddr(hostname, addr)
		logf("[power] chassis control enabled for %v (own BMC at %s)", pc.Hosts, addr)
	} else {
		logf("[power] chassis control enabled for %v (own BMC address unknown)", pc.Hosts)
	}
	if pass == "" {
		logf("[power] no BMC password set: hosts that are powered off cannot be reached (set BMC_PASSWORD)")
	}
	return ctl
}

// buildCatalog returns the OpenCode catalogue handler, or nil when the node
// serves none. The source is the inference handler, which already holds both
// views the catalogue renders.
func buildCatalog(cfg *config.Config, src discovery.Source) http.Handler {
	c := cfg.API.Catalog
	if !c.Enabled.Resolve(true) {
		return nil
	}
	return catalog.NewHandler(catalog.Config{
		ProviderID:   c.ProviderID,
		ProviderName: c.ProviderName,
		BaseURL:      c.BaseURL,
		Upstream:     c.Upstream,
		UpstreamTTL:  c.UpstreamTTL.Duration,
	}, src)
}

// engineEnviron is the environment an engine process starts from: the node's
// own, without the node's secrets. An engine needs none of them, and one
// that prints its environment into a log or a crash report (a third-party
// server, and whatever it starts in turn) would publish the secret that
// signs every write in the mesh. A model that does want one of these names
// sets it in its own env, which is added afterwards.
func engineEnviron(cfg *config.Config) func() []string {
	secret := cfg.Mesh.SecretEnv
	if secret == "" {
		secret = config.DefaultMeshSecretEnv
	}
	bmc := cfg.Power.Control.BMC.PasswordEnv
	if bmc == "" {
		bmc = "BMC_PASSWORD"
	}
	drop := []string{secret, cfg.Mesh.SecretPrevEnv, bmc, "ENTSOE_API_KEY"}
	return func() []string {
		env := os.Environ()
		out := make([]string, 0, len(env))
		for _, kv := range env {
			name, _, _ := strings.Cut(kv, "=")
			if name != "" && slices.Contains(drop, name) {
				continue
			}
			out = append(out, kv)
		}
		return out
	}
}
