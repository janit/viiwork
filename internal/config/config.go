package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/janit/viiwork/internal/pipeline"
	"gopkg.in/yaml.v3"
)

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = parsed
	return nil
}

type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type ModelConfig struct {
	Path        string `yaml:"path"`
	ContextSize int    `yaml:"context_size"`
	NGPULayers  int    `yaml:"n_gpu_layers"`
	Parallel    int    `yaml:"parallel"`
}

type GPUConfig struct {
	Count           int               `yaml:"count"`
	Devices         []int             `yaml:"devices"`
	BasePort        int               `yaml:"base_port"`
	Offset          int               `yaml:"offset"`
	PowerLimitWatts int               `yaml:"power_limit_watts"`
	TensorSplit     TensorSplitConfig `yaml:"tensor_split"`
}

// TensorSplitConfig configures llama.cpp tensor parallelism: a llama-server
// process spans multiple GPUs and the model is split across them. When
// Enabled, viiwork partitions GPUConfig.Devices into one or more groups of
// GroupSize and spawns one backend per group. With GroupSize=0 (default) all
// devices form a single group, giving the original "one big backend across
// every GPU" behavior. GroupSize>=2 produces multiple tensor-split backends,
// each replicating the full model across its group — used when the model
// fits in a group's combined VRAM and you want several concurrent streams
// with the single-stream speed of tensor parallelism.
type TensorSplitConfig struct {
	Enabled   bool      `yaml:"enabled"`
	Mode      string    `yaml:"mode"`       // "layer" (default) or "row"
	Weights   []float64 `yaml:"weights"`    // optional per-group split fractions; default: even. Length must equal the group size when set; the same pattern applies to every group.
	MainGPU   int       `yaml:"main_gpu"`   // only used when mode="row"; default 0. Index within a group (0..group_size-1).
	GroupSize int       `yaml:"group_size"` // 0 = all devices in one group (legacy). >=2 = split devices into consecutive groups of this size, each becoming its own backend on base_port+i.
}

// ResolvedDevices returns the explicit GPU device IDs to use.
// If devices is set, it takes priority. Otherwise falls back to count+offset.
func (g *GPUConfig) ResolvedDevices() []int {
	if len(g.Devices) > 0 {
		return g.Devices
	}
	ids := make([]int, g.Count)
	for i := range ids {
		ids[i] = g.Offset + i
	}
	return ids
}

type BackendConfig struct {
	Binary    string   `yaml:"binary"`
	ExtraArgs []string `yaml:"extra_args"`
}

type HealthConfig struct {
	Interval    Duration `yaml:"interval"`
	Timeout     Duration `yaml:"timeout"`
	MaxFailures int      `yaml:"max_failures"`
}

type BalancerConfig struct {
	LatencyWindow     Duration `yaml:"latency_window"`
	HighLoadThreshold int      `yaml:"high_load_threshold"`
	MaxInFlightPerGPU int      `yaml:"max_in_flight_per_gpu"`
}

type PeersConfig struct {
	Hosts        []string `yaml:"hosts"`
	PollInterval Duration `yaml:"poll_interval"`
	Timeout      Duration `yaml:"timeout"`
}

type WinterTransferConfig struct {
	PeakCentsKWh    float64 `yaml:"peak_cents_kwh"`
	OffpeakCentsKWh float64 `yaml:"offpeak_cents_kwh"`
}

type SummerTransferConfig struct {
	FlatCentsKWh float64 `yaml:"flat_cents_kwh"`
}

type TransferConfig struct {
	Winter WinterTransferConfig `yaml:"winter"`
	Summer SummerTransferConfig `yaml:"summer"`
}

type CostConfig struct {
	BiddingZone            string         `yaml:"bidding_zone"`
	Timezone               string         `yaml:"timezone"`
	Transfer               TransferConfig `yaml:"transfer"`
	ElectricityTaxCentsKWh float64        `yaml:"electricity_tax_cents_kwh"`
	VATPercent             float64        `yaml:"vat_percent"`
}

type Config struct {
	Server    ServerConfig                        `yaml:"server"`
	Model     ModelConfig                         `yaml:"model"`
	GPUs      GPUConfig                           `yaml:"gpus"`
	Backend   BackendConfig                       `yaml:"backend"`
	Health    HealthConfig                        `yaml:"health"`
	Balancer  BalancerConfig                      `yaml:"balancer"`
	Peers     PeersConfig                         `yaml:"peers"`
	Cost      CostConfig                          `yaml:"cost"`
	Pipelines map[string]pipeline.PipelineConfig `yaml:"pipelines"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	cfg := Defaults()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) Validate() error {
	if c.Model.Path == "" {
		return fmt.Errorf("model.path is required")
	}
	if len(c.GPUs.Devices) > 0 {
		c.GPUs.Count = len(c.GPUs.Devices)
	}
	if c.GPUs.Count < 1 {
		return fmt.Errorf("gpus.count or gpus.devices is required")
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be 1-65535")
	}
	if c.Balancer.MaxInFlightPerGPU < 1 {
		return fmt.Errorf("balancer.max_in_flight_per_gpu must be >= 1")
	}
	if c.Health.Interval.Duration <= 0 {
		return fmt.Errorf("health.interval must be positive")
	}
	if c.Health.Timeout.Duration <= 0 {
		return fmt.Errorf("health.timeout must be positive")
	}
	if c.Health.MaxFailures < 1 {
		return fmt.Errorf("health.max_failures must be >= 1")
	}
	if c.GPUs.TensorSplit.Enabled {
		// Tensor-split mode: one or more backends, each spanning GroupSize
		// devices on consecutive ports starting at BasePort.
		devices := c.GPUs.ResolvedDevices()
		if len(devices) < 2 {
			return fmt.Errorf("gpus.tensor_split.enabled requires at least 2 devices")
		}
		groupSize := c.GPUs.TensorSplit.GroupSize
		if groupSize < 0 {
			return fmt.Errorf("gpus.tensor_split.group_size must be >= 0 (0 = single group across all devices)")
		}
		if groupSize == 0 {
			groupSize = len(devices)
		}
		if groupSize < 2 {
			return fmt.Errorf("gpus.tensor_split.group_size must be 0 or >= 2, got %d", groupSize)
		}
		if groupSize > len(devices) {
			return fmt.Errorf("gpus.tensor_split.group_size (%d) exceeds device count (%d)", groupSize, len(devices))
		}
		if len(devices)%groupSize != 0 {
			return fmt.Errorf("gpus.tensor_split.group_size (%d) must evenly divide device count (%d)", groupSize, len(devices))
		}
		nGroups := len(devices) / groupSize
		if c.GPUs.BasePort < 1 || c.GPUs.BasePort+nGroups-1 > 65535 {
			return fmt.Errorf("gpus.base_port must be 1-65535 and base_port+nGroups-1 must not exceed 65535")
		}
		if c.GPUs.TensorSplit.Mode == "" {
			c.GPUs.TensorSplit.Mode = "layer"
		}
		if c.GPUs.TensorSplit.Mode != "layer" && c.GPUs.TensorSplit.Mode != "row" {
			return fmt.Errorf("gpus.tensor_split.mode must be 'layer' or 'row', got %q", c.GPUs.TensorSplit.Mode)
		}
		// Weights apply within a group (same pattern replicated to every group).
		if len(c.GPUs.TensorSplit.Weights) > 0 && len(c.GPUs.TensorSplit.Weights) != groupSize {
			return fmt.Errorf("gpus.tensor_split.weights length (%d) must match group_size (%d)",
				len(c.GPUs.TensorSplit.Weights), groupSize)
		}
		if c.GPUs.TensorSplit.MainGPU < 0 || c.GPUs.TensorSplit.MainGPU >= groupSize {
			return fmt.Errorf("gpus.tensor_split.main_gpu (%d) must be a valid index 0..%d",
				c.GPUs.TensorSplit.MainGPU, groupSize-1)
		}
		// llama.cpp tensor-split serves a single slot per backend — concurrent
		// requests queue at the slot. Force parallel=1 regardless of user
		// setting; the proxy still queues at the front via balancer
		// max_in_flight. With multiple groups the balancer routes across
		// groups exactly like it does across replica backends.
		c.Model.Parallel = 1
	} else {
		// Replica mode: one process per GPU on consecutive ports.
		if c.GPUs.BasePort < 1 || c.GPUs.BasePort+c.GPUs.Count-1 > 65535 {
			return fmt.Errorf("gpus.base_port must be 1-65535 and base_port+count must not exceed 65535")
		}
	}
	return nil
}

func (c *Config) ApplyOverrides(overrides map[string]string) error {
	for key, val := range overrides {
		switch key {
		case "server.host":
			c.Server.Host = val
		case "server.port":
			v, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("invalid server.port: %w", err)
			}
			c.Server.Port = v
		case "model.path":
			c.Model.Path = val
		case "model.context_size":
			v, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("invalid model.context_size: %w", err)
			}
			c.Model.ContextSize = v
		case "model.n_gpu_layers":
			v, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("invalid model.n_gpu_layers: %w", err)
			}
			c.Model.NGPULayers = v
		case "gpus.count":
			v, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("invalid gpus.count: %w", err)
			}
			c.GPUs.Count = v
		case "gpus.base_port":
			v, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("invalid gpus.base_port: %w", err)
			}
			c.GPUs.BasePort = v
		case "gpus.offset":
			v, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("invalid gpus.offset: %w", err)
			}
			c.GPUs.Offset = v
		case "backend.binary":
			c.Backend.Binary = val
		case "balancer.high_load_threshold":
			v, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("invalid balancer.high_load_threshold: %w", err)
			}
			c.Balancer.HighLoadThreshold = v
		case "balancer.max_in_flight_per_gpu":
			v, err := strconv.Atoi(val)
			if err != nil {
				return fmt.Errorf("invalid balancer.max_in_flight_per_gpu: %w", err)
			}
			c.Balancer.MaxInFlightPerGPU = v
		case "health.interval":
			d, err := time.ParseDuration(val)
			if err != nil {
				return fmt.Errorf("invalid health.interval: %w", err)
			}
			c.Health.Interval = Duration{d}
		case "health.timeout":
			d, err := time.ParseDuration(val)
			if err != nil {
				return fmt.Errorf("invalid health.timeout: %w", err)
			}
			c.Health.Timeout = Duration{d}
		case "balancer.latency_window":
			d, err := time.ParseDuration(val)
			if err != nil {
				return fmt.Errorf("invalid balancer.latency_window: %w", err)
			}
			c.Balancer.LatencyWindow = Duration{d}
		default:
			return fmt.Errorf("unknown override key: %s", key)
		}
	}
	return nil
}
