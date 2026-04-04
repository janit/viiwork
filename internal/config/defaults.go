package config

import "time"

func Defaults() Config {
	return Config{
		Server: ServerConfig{
			Host: "0.0.0.0",
			Port: 8080,
		},
		Model: ModelConfig{
			ContextSize: 8192,
			NGPULayers:  -1,
		},
		GPUs: GPUConfig{
			Count:    10,
			BasePort: 9001,
		},
		Backend: BackendConfig{
			Binary: "llama-server",
		},
		Health: HealthConfig{
			Interval:    Duration{5 * time.Second},
			Timeout:     Duration{3 * time.Second},
			MaxFailures: 3,
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
	}
}
