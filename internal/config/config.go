package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

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
}

type GPUConfig struct {
	Count           int `yaml:"count"`
	BasePort        int `yaml:"base_port"`
	PowerLimitWatts int `yaml:"power_limit_watts"`
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
	Server   ServerConfig   `yaml:"server"`
	Model    ModelConfig    `yaml:"model"`
	GPUs     GPUConfig      `yaml:"gpus"`
	Backend  BackendConfig  `yaml:"backend"`
	Health   HealthConfig   `yaml:"health"`
	Balancer BalancerConfig `yaml:"balancer"`
	Peers    PeersConfig    `yaml:"peers"`
	Cost     CostConfig     `yaml:"cost"`
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
	if c.GPUs.Count < 1 {
		return fmt.Errorf("gpus.count must be >= 1")
	}
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be 1-65535")
	}
	if c.Balancer.MaxInFlightPerGPU < 1 {
		return fmt.Errorf("balancer.max_in_flight_per_gpu must be >= 1")
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
