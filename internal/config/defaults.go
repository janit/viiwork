package config

import (
	"time"

	"github.com/janit/viiwork/internal/activity"
	"github.com/janit/viiwork/internal/energy"
	"github.com/janit/viiwork/internal/power"
)

func Defaults() Config {
	return Config{
		Server: ServerConfig{
			Host: "0.0.0.0",
			Port: 8080,
			// Defaults cover the deployment viiwork documents — nodes on a
			// tailnet — plus localhost for development. Your own application's
			// origin is deployment-specific and belongs in your viiwork.yaml,
			// not here.
			//
			// The API authenticates nothing, so this list is the only thing
			// standing between it and any page a browser on your network
			// happens to open. Narrow it rather than widen it; set
			// allow_origins: [] to turn CORS off entirely.
			CORS: CORSConfig{
				AllowOrigins: []string{"*.ts.net", "localhost", "127.0.0.1"},
			},
		},
		Activity: ActivityConfig{
			PromptHistory: activity.DefaultPromptHistory,
		},
		Model: ModelConfig{
			ContextSize: 13337,
			NGPULayers:  -1,
			Parallel:    1,
		},
		GPUs: GPUConfig{
			Count:    10,
			BasePort: 9001,
		},
		Backend: BackendConfig{
			Binary:    "llama-server",
			ExtraArgs: []string{"--reasoning-format", "deepseek"},
		},
		Health: HealthConfig{
			Interval: Duration{5 * time.Second},
			// 30s, not 3s: on CPU-bound hosts a busy prompt-eval starves the
			// llama-server's /health responder, and an aggressive timeout
			// triggers 3/3 failures → respawn → cold reload → cascade. Field
			// report on 4-core EPYC 3151 took 502 rate from 40% (3s) to 0%
			// (30s) at concurrency 16.
			Timeout:            Duration{30 * time.Second},
			MaxFailures:        3,
			RespawnGrace:       Duration{60 * time.Second},
			EvictOnHardFailure: true,
		},
		Balancer: BalancerConfig{
			LatencyWindow:     Duration{30 * time.Second},
			HighLoadThreshold: 7,
			MaxInFlightPerGPU: 4,
		},
		Peers: PeersConfig{
			PollInterval: Duration{10 * time.Second},
			Timeout:      Duration{3 * time.Second},
		},
		Cost: CostConfig{
			Timezone: "Europe/Helsinki",
		},
		Power: PowerConfig{
			Source: power.SourceAuto,
		},
		Energy: EnergyConfig{
			Dir:            "/var/lib/viiwork/energy",
			SampleInterval: Duration{30 * time.Second},
			MinuteSlots:    energy.DefaultMinuteSlots,
			HourSlots:      energy.DefaultHourSlots,
			DaySlots:       energy.DefaultDaySlots,
		},
	}
}
