package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/janit/viiwork/energy"
	"github.com/janit/viiwork/internal/activity"
	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/config"
	"github.com/janit/viiwork/internal/cost"
	"github.com/janit/viiwork/internal/gpu"
	"github.com/janit/viiwork/internal/logging"
	"github.com/janit/viiwork/internal/meshauth"
	"github.com/janit/viiwork/internal/model"
	"github.com/janit/viiwork/internal/peer"
	"github.com/janit/viiwork/internal/pipeline"
	"github.com/janit/viiwork/internal/power"
	"github.com/janit/viiwork/internal/process"
	"github.com/janit/viiwork/internal/proxy"
)

var version = "dev"

func generateNodeID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("viiwork-%x", b)
}

func main() {
	proxy.Version = version

	cost.LoadDotEnv(".env")

	configPath := flag.String("config", "", "path to viiwork.yaml")
	flag.Parse()

	overrides := parseDotpathArgs(os.Args[1:])

	var cfg *config.Config
	var err error

	if *configPath != "" {
		cfg, err = config.Load(*configPath)
		if err != nil {
			log.Fatalf("loading config: %v", err)
		}
	} else {
		d := config.Defaults()
		cfg = &d
	}

	if len(overrides) > 0 {
		if err := cfg.ApplyOverrides(overrides); err != nil {
			log.Fatalf("applying overrides: %v", err)
		}
	}

	if err := cfg.Validate(); err != nil {
		log.Fatalf("config validation: %v", err)
	}

	// Load pipelines
	var pipelines []*pipeline.Pipeline
	for name, rawCfg := range cfg.Pipelines {
		p, err := pipeline.LoadPipeline(name, rawCfg)
		if err != nil {
			log.Fatalf("loading pipeline %s: %v", name, err)
		}
		pipelines = append(pipelines, p)
		log.Printf("pipeline '%s' loaded with %d locales, %d steps", name, len(p.Locales), len(p.Steps))
	}

	nodeID := generateNodeID()
	log.Printf("viiwork %s starting with %d GPUs, model: %s", nodeID, cfg.GPUs.Count, cfg.Model.Path)

	sampler := power.NewSampler(cfg.Power.Source)

	// Build peer list
	var peers []*peer.PeerState
	for _, host := range cfg.Peers.Hosts {
		peers = append(peers, peer.NewPeerState(host))
	}

	// Resolved before anything starts: a node configured to gossip without a
	// usable secret is a misconfiguration to fix, not a state to run in.
	secret, err := cfg.MeshSecret(os.LookupEnv)
	if err != nil {
		log.Fatalf("mesh secret: %v", err)
	}
	var signer *meshauth.Signer
	if secret != nil {
		signer, err = meshauth.NewSigner(secret, nodeID)
		if err != nil {
			log.Fatalf("mesh secret: %v", err)
		}
		log.Printf("[mesh] membership proof enabled (gossip adoption: %v)", cfg.Peers.Gossip.Enabled)
	}

	var tracker *cost.Tracker
	apiKey := os.Getenv("ENTSOE_API_KEY")
	if cfg.Cost.BiddingZone != "" && apiKey != "" {
		fetcher := cost.NewSpotFetcher(apiKey, cfg.Cost.BiddingZone, "https://web-api.tp.entsoe.eu/api")
		costCfg := cost.CostConfig{
			Transfer: cost.TransferConfig{
				Winter: cost.WinterTransferConfig{
					PeakCentsKWh:    cfg.Cost.Transfer.Winter.PeakCentsKWh,
					OffpeakCentsKWh: cfg.Cost.Transfer.Winter.OffpeakCentsKWh,
				},
				Summer: cost.SummerTransferConfig{
					FlatCentsKWh: cfg.Cost.Transfer.Summer.FlatCentsKWh,
				},
			},
			ElectricityTaxCentsKWh: cfg.Cost.ElectricityTaxCentsKWh,
			VATPercent:             cfg.Cost.VATPercent,
			Timezone:               cfg.Cost.Timezone,
		}
		tracker = cost.NewTracker(fetcher, costCfg, sampler)
		log.Printf("cost tracking enabled (zone: %s)", cfg.Cost.BiddingZone)
	} else if cfg.Cost.BiddingZone != "" {
		log.Println("WARNING: cost section configured but ENTSOE_API_KEY not set, cost tracking disabled")
	}

	var costTracker process.CostTracker
	if tracker != nil {
		costTracker = tracker
	}

	hist := gpu.NewHistory(720)
	bcast := gpu.NewBroadcaster()
	collector := gpu.NewStatCollector(hist, bcast)

	actLog := activity.NewLogWithPromptHistory(cfg.Activity.PromptHistory)

	mgr := process.NewManager(cfg, nil, sampler, costTracker, collector, actLog)
	for _, b := range mgr.Backends {
		b.LogWriter = logging.NewPrefixWriter(os.Stdout, fmt.Sprintf("[gpu-%d] ", b.GPUID))
	}

	bal := balancer.New(
		mgr.States(),
		cfg.Balancer.HighLoadThreshold,
		cfg.Balancer.MaxInFlightPerGPU,
	)

	localModel := model.IDFromPath(cfg.Model.Path)
	reg := peer.NewRegistry(nodeID, localModel, mgr.States(), peers, cfg.Peers.Timeout.Duration)
	if signer != nil {
		reg.SetSigner(signer)
	}
	reg.SetGossip(peer.GossipOptions{
		Enabled:         cfg.Peers.Gossip.Enabled,
		DiscoveryEvery:  cfg.Peers.Gossip.DiscoveryEvery,
		MaxLearnedPeers: cfg.Peers.Gossip.MaxLearnedPeers,
		AllowPrivate:    cfg.Peers.Gossip.AllowPrivate,
	})
	reg.SetPowerReader(sampler)
	if tracker != nil {
		reg.SetCostReader(tracker)
	}
	hostname, _ := os.Hostname()
	reg.SetLocation(hostname, fmt.Sprintf("%s:%d", hostname, cfg.Server.Port))
	reg.SetPromptHistory(actLog.PromptHistoryMax())
	handler := proxy.NewMeshHandler(bal, reg, cfg.Balancer.LatencyWindow.Duration)
	handler.SetMeshSigner(signer)
	handler.SetRequireForwardProof(cfg.Peers.Gossip.RequireForwardProof)
	handler.SetMetrics(hist, bcast, collector.Available)
	if ctl := newPowerController(cfg, hostname); ctl != nil {
		handler.SetPowerControl(ctl)
		proxy.SetStatusPowerControl(ctl)
	}
	// Publish GPU load to peers as well as locally. Without this the /mesh view
	// can show GPU% for this node only, because peers learn everything they know
	// about us from /v1/status.
	proxy.SetStatusGPUSource(hist)
	handler.SetActivity(actLog)
	handler.SetEvictOnHardFailure(cfg.Health.EvictOnHardFailure)
	if len(cfg.Server.CORS.AllowOrigins) > 0 {
		tailnetIPs := cfg.Server.CORS.AllowTailnetIPs == nil || *cfg.Server.CORS.AllowTailnetIPs
		handler.SetCORS(&proxy.CORS{Origins: cfg.Server.CORS.AllowOrigins, TailnetIPs: tailnetIPs})
		log.Printf("CORS enabled for origins %v (tailnet IPs: %v)", cfg.Server.CORS.AllowOrigins, tailnetIPs)
	}

	if len(pipelines) > 0 {
		resolver := proxy.NewPipelineResolver(pipelines)
		execURL := fmt.Sprintf("http://localhost:%d", cfg.Server.Port)
		client := &http.Client{}
		exec := pipeline.NewExecutor(execURL, client)
		handler.SetPipelines(resolver, exec)
		log.Printf("pipeline virtual models: %v", resolver.VirtualModelNames())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	if err := mgr.StartAll(ctx); err != nil {
		log.Fatalf("starting backends: %v", err)
	}

	go mgr.RunHealthLoop(ctx)
	go reg.Run(ctx, cfg.Peers.PollInterval.Duration)

	if cfg.Energy.Enabled {
		startEnergyRecorder(ctx, cfg, hist, sampler, reg)
	}

	if len(peers) > 0 {
		log.Printf("mesh enabled with %d peer(s)", len(peers))
	}

	server := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Printf("listening on %s:%d", cfg.Server.Host, cfg.Server.Port)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// A second, well-known port whose "/" is the mesh dashboard. Every
	// instance on the host asks for it and one gets it, so the address is the
	// same everywhere and does not depend on knowing which model this node
	// serves. Skipped when it would be the node's own port, which already
	// answers.
	if mp := cfg.Server.MeshPort; mp > 0 {
		if mp == cfg.Server.Port {
			log.Printf("server.mesh_port equals server.port (%d), not starting a second listener", mp)
		} else {
			go proxy.ServeMeshPort(ctx, fmt.Sprintf("%s:%d", cfg.Server.Host, mp), handler)
		}
	}

	<-sigCh
	log.Println("shutdown signal received")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	server.Shutdown(shutdownCtx)
	cancel()
	bcast.Close()
	mgr.Shutdown(shutdownCtx)

	log.Println("viiwork stopped")
}

func parseDotpathArgs(args []string) map[string]string {
	overrides := make(map[string]string)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			continue
		}
		key := strings.TrimPrefix(arg, "--")
		if key == "config" {
			i++
			continue
		}
		if strings.Contains(key, ".") && i+1 < len(args) {
			i++
			overrides[key] = args[i]
		}
	}
	return overrides
}

// startEnergyRecorder wires the durable kWh store to the power sampler and the
// GPU collector. A failure here disables energy tracking and leaves the node
// serving, matching how power, cost and GPU metrics already degrade: a node
// that cannot write history is still a node that can answer requests.
func startEnergyRecorder(ctx context.Context, cfg *config.Config, hist *gpu.History, sampler *power.Sampler, reg *peer.Registry) {
	// Every GPU rocm-smi reports, not just this instance's. Attribution divides
	// by total marginal draw across the host, so omitting a co-tenant's cards
	// would charge their load to this instance's models.
	gpuIDs := hist.GPUIDs()
	if len(gpuIDs) == 0 {
		gpuIDs = cfg.GPUs.ResolvedDevices()
	}

	loc, err := time.LoadLocation(cfg.Cost.Timezone)
	if err != nil {
		loc = time.UTC
	}

	// Stamp the history with which reading it was measured from. The sampler
	// has already probed by now (NewSampler does it synchronously), so this is
	// the source actually adopted rather than the one configured. When power
	// is unavailable the label is left empty: nothing will be recorded anyway,
	// and writing SourceName()'s "ipmitool" placeholder would claim a
	// provenance no reading ever had.
	powerSource := ""
	if sampler.Available() {
		powerSource = sampler.SourceName()
	}

	store, err := energy.Open(energy.Config{
		Dir:         cfg.Energy.Dir,
		GPUIDs:      gpuIDs,
		MinuteSlots: cfg.Energy.MinuteSlots,
		HourSlots:   cfg.Energy.HourSlots,
		DaySlots:    cfg.Energy.DaySlots,
		Location:    loc,
		Source:      powerSource,
	}, nil)
	if err != nil {
		log.Printf("[energy] disabled: %v", err)
		return
	}

	nodeWatts := func() (float64, bool) { return sampler.Watts(), sampler.Available() }
	readings := func() []energy.GPUReading {
		// Resolved per sample, not once: peers may not have been polled yet
		// at startup, and the host's GPU-to-model layout changes.
		owned := reg.GPUModels()
		out := make([]energy.GPUReading, 0, len(gpuIDs))
		for _, id := range gpuIDs {
			samples := hist.Samples(id)
			if len(samples) == 0 {
				continue
			}
			latest := samples[len(samples)-1]
			if latest.PowerW <= 0 {
				continue
			}
			out = append(out, energy.GPUReading{GPUID: id, Watts: latest.PowerW, Model: owned[id]})
		}
		return out
	}

	// Publish the cumulative total on /v1/status and /v1/cluster, so the mesh
	// view can show kWh beside live watts without a second endpoint to poll.
	proxy.SetStatusEnergySource(store)

	recorder := energy.NewRecorder(store, cfg.Energy.SampleInterval.Duration, nodeWatts, readings, nil)
	go func() {
		recorder.Run(ctx)
		store.Close()
	}()
	log.Printf("[energy] recording every %s for %d GPUs (node wattage is per-host: enable this on one instance per host)",
		cfg.Energy.SampleInterval.Duration, len(gpuIDs))
}

// newPowerController builds the chassis power controller, or nil when power
// control is not configured. Nil rather than a disabled instance so the
// endpoints answer "not enabled on this node" from one check.
func newPowerController(cfg *config.Config, hostname string) *power.Controller {
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
		pass = os.Getenv(env)
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

	ctl := power.NewController(power.ControlConfig{
		Enabled: true, Hosts: pc.Hosts, BMCs: bmcs,
	}, hostname)

	// Learn this host's own BMC address and publish it, so peers can reach it
	// once this node is no longer running to be asked.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if addr := power.LocalBMCAddr(ctx); addr != "" {
		ctl.LearnBMCAddr(hostname, addr)
		log.Printf("[power] chassis control enabled for %v (own BMC at %s)", pc.Hosts, addr)
	} else {
		log.Printf("[power] chassis control enabled for %v (own BMC address unknown)", pc.Hosts)
	}
	if pass == "" {
		log.Printf("[power] no BMC password set: hosts that are powered off cannot be reached (set BMC_PASSWORD)")
	}
	return ctl
}
