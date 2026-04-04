package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/janit/viiwork/internal/balancer"
	"github.com/janit/viiwork/internal/config"
	"github.com/janit/viiwork/internal/cost"
	"github.com/janit/viiwork/internal/gpu"
	"github.com/janit/viiwork/internal/identity"
	"github.com/janit/viiwork/internal/logging"
	"github.com/janit/viiwork/internal/model"
	"github.com/janit/viiwork/internal/peer"
	"github.com/janit/viiwork/internal/power"
	"github.com/janit/viiwork/internal/process"
	"github.com/janit/viiwork/internal/proxy"
)

var version = "dev"

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

	nodeID := identity.GenerateNodeID()
	log.Printf("viiwork %s starting with %d GPUs, model: %s", nodeID, cfg.GPUs.Count, cfg.Model.Path)

	sampler := power.NewSampler()

	// Build peer list
	var peers []*peer.PeerState
	for _, host := range cfg.Peers.Hosts {
		peers = append(peers, peer.NewPeerState(host))
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

	mgr := process.NewManager(cfg, nil, sampler, costTracker, collector)
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
	reg.SetPowerReader(sampler)
	if tracker != nil {
		reg.SetCostReader(tracker)
	}
	handler := proxy.NewMeshHandler(bal, reg, cfg.Balancer.LatencyWindow.Duration)
	handler.SetMetrics(hist, bcast, collector.Available)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	if err := mgr.StartAll(ctx); err != nil {
		log.Fatalf("starting backends: %v", err)
	}

	go mgr.RunHealthLoop(ctx)
	go reg.Run(ctx, cfg.Peers.PollInterval.Duration)

	if len(peers) > 0 {
		log.Printf("mesh enabled with %d peer(s)", len(peers))
	}

	server := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler: handler,
	}

	go func() {
		log.Printf("listening on %s:%d", cfg.Server.Host, cfg.Server.Port)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

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
