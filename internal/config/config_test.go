package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadFromYAML(t *testing.T) {
	yaml := `
server:
  host: 0.0.0.0
  port: 8080
model:
  path: /models/test.gguf
  context_size: 4096
  n_gpu_layers: -1
gpus:
  count: 2
  base_port: 9001
backend:
  binary: llama-server
  extra_args: ["--threads", "4"]
health:
  interval: 5s
  timeout: 3s
  max_failures: 3
balancer:
  strategy: adaptive
  latency_window: 30s
  high_load_threshold: 7
  max_in_flight_per_gpu: 4
`
	f, err := os.CreateTemp("", "viiwork-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString(yaml)
	f.Close()

	cfg, err := Load(f.Name())
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Server.Port != 8080 {
		t.Errorf("expected port 8080, got %d", cfg.Server.Port)
	}
	if cfg.Model.Path != "/models/test.gguf" {
		t.Errorf("expected model path /models/test.gguf, got %s", cfg.Model.Path)
	}
	if cfg.GPUs.Count != 2 {
		t.Errorf("expected 2 gpus, got %d", cfg.GPUs.Count)
	}
	if cfg.Health.Interval.Seconds() != 5 {
		t.Errorf("expected 5s interval, got %v", cfg.Health.Interval)
	}
	if len(cfg.Backend.ExtraArgs) != 2 {
		t.Errorf("expected 2 extra args, got %d", len(cfg.Backend.ExtraArgs))
	}
	if cfg.Balancer.MaxInFlightPerGPU != 4 {
		t.Errorf("expected max_in_flight_per_gpu 4, got %d", cfg.Balancer.MaxInFlightPerGPU)
	}
}

func TestDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.Server.Port != 8080 {
		t.Errorf("expected default port 8080, got %d", cfg.Server.Port)
	}
	if cfg.GPUs.Count != 10 {
		t.Errorf("expected default 10 gpus, got %d", cfg.GPUs.Count)
	}
	if cfg.Balancer.MaxInFlightPerGPU != 4 {
		t.Errorf("expected default max_in_flight 4, got %d", cfg.Balancer.MaxInFlightPerGPU)
	}
}

func TestValidation(t *testing.T) {
	cfg := Defaults()
	cfg.Model.Path = ""
	if err := cfg.Validate(); err == nil {
		t.Error("expected validation error for empty model path")
	}
}

func TestTensorSplitConfigYAML(t *testing.T) {
	yaml := `
model:
  path: /models/big.gguf
  context_size: 4096
  parallel: 4
gpus:
  devices: [4, 5, 6, 7]
  base_port: 8081
  tensor_split:
    enabled: true
    weights: [1.0, 1.0, 1.0, 1.0]
`
	f, err := os.CreateTemp("", "viiwork-ts-*.yaml")
	if err != nil { t.Fatal(err) }
	defer os.Remove(f.Name())
	f.WriteString(yaml)
	f.Close()

	cfg, err := Load(f.Name())
	if err != nil { t.Fatalf("Load failed: %v", err) }
	if !cfg.GPUs.TensorSplit.Enabled {
		t.Error("expected tensor_split.enabled=true")
	}
	if cfg.GPUs.TensorSplit.Mode != "layer" {
		t.Errorf("expected default mode 'layer', got %q", cfg.GPUs.TensorSplit.Mode)
	}
	if len(cfg.GPUs.TensorSplit.Weights) != 4 {
		t.Errorf("expected 4 weights, got %d", len(cfg.GPUs.TensorSplit.Weights))
	}
	// parallel must be forced to 1 in tensor-split mode regardless of user setting
	if cfg.Model.Parallel != 1 {
		t.Errorf("expected model.parallel forced to 1 in tensor-split mode, got %d", cfg.Model.Parallel)
	}
	if cfg.GPUs.Count != 4 {
		t.Errorf("expected count derived from devices=4, got %d", cfg.GPUs.Count)
	}
}

func TestTensorSplitValidationRejects(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"single device", func(c *Config) {
			c.GPUs.Devices = []int{4}
			c.GPUs.TensorSplit.Enabled = true
		}},
		{"bad mode", func(c *Config) {
			c.GPUs.Devices = []int{4, 5}
			c.GPUs.TensorSplit.Enabled = true
			c.GPUs.TensorSplit.Mode = "weird"
		}},
		{"weights length mismatch", func(c *Config) {
			c.GPUs.Devices = []int{4, 5, 6}
			c.GPUs.TensorSplit.Enabled = true
			c.GPUs.TensorSplit.Weights = []float64{1, 1}
		}},
		{"main_gpu out of range", func(c *Config) {
			c.GPUs.Devices = []int{4, 5}
			c.GPUs.TensorSplit.Enabled = true
			c.GPUs.TensorSplit.MainGPU = 5
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Model.Path = "/models/x.gguf"
			tc.mut(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Errorf("expected validation error for %s", tc.name)
			}
		})
	}
}

func TestTensorSplitDefaultMode(t *testing.T) {
	cfg := Defaults()
	cfg.Model.Path = "/models/x.gguf"
	cfg.GPUs.Devices = []int{4, 5}
	cfg.GPUs.TensorSplit.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	if cfg.GPUs.TensorSplit.Mode != "layer" {
		t.Errorf("expected default mode 'layer', got %q", cfg.GPUs.TensorSplit.Mode)
	}
}

func TestCLIOverrides(t *testing.T) {
	cfg := Defaults()
	cfg.Model.Path = "/models/test.gguf"

	overrides := map[string]string{
		"gpus.count":  "4",
		"server.port": "9090",
		"model.path":  "/models/other.gguf",
	}
	if err := cfg.ApplyOverrides(overrides); err != nil {
		t.Fatalf("ApplyOverrides failed: %v", err)
	}

	if cfg.GPUs.Count != 4 {
		t.Errorf("expected 4 gpus, got %d", cfg.GPUs.Count)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("expected port 9090, got %d", cfg.Server.Port)
	}
	if cfg.Model.Path != "/models/other.gguf" {
		t.Errorf("expected /models/other.gguf, got %s", cfg.Model.Path)
	}
}

func TestPeersConfig(t *testing.T) {
	yaml := `
model:
  path: /models/test.gguf
gpus:
  count: 2
peers:
  hosts:
    - 192.168.1.10:8080
    - 192.168.1.11:8080
  poll_interval: 15s
  timeout: 5s
`
	f, err := os.CreateTemp("", "viiwork-*.yaml")
	if err != nil { t.Fatal(err) }
	defer os.Remove(f.Name())
	f.WriteString(yaml)
	f.Close()

	cfg, err := Load(f.Name())
	if err != nil { t.Fatalf("Load failed: %v", err) }
	if len(cfg.Peers.Hosts) != 2 { t.Errorf("expected 2 peer hosts, got %d", len(cfg.Peers.Hosts)) }
	if cfg.Peers.Hosts[0] != "192.168.1.10:8080" { t.Errorf("expected first host 192.168.1.10:8080, got %s", cfg.Peers.Hosts[0]) }
	if cfg.Peers.PollInterval.Duration != 15*time.Second { t.Errorf("expected 15s poll interval, got %v", cfg.Peers.PollInterval) }
	if cfg.Peers.Timeout.Duration != 5*time.Second { t.Errorf("expected 5s timeout, got %v", cfg.Peers.Timeout) }
}

func TestPeersConfigDefaults(t *testing.T) {
	cfg := Defaults()
	if cfg.Peers.PollInterval.Duration != 10*time.Second { t.Errorf("expected default 10s poll interval, got %v", cfg.Peers.PollInterval) }
	if cfg.Peers.Timeout.Duration != 3*time.Second { t.Errorf("expected default 3s timeout, got %v", cfg.Peers.Timeout) }
}

func TestNoPeersConfigStandalone(t *testing.T) {
	yaml := `
model:
  path: /models/test.gguf
gpus:
  count: 2
`
	f, err := os.CreateTemp("", "viiwork-*.yaml")
	if err != nil { t.Fatal(err) }
	defer os.Remove(f.Name())
	f.WriteString(yaml)
	f.Close()

	cfg, err := Load(f.Name())
	if err != nil { t.Fatalf("Load failed: %v", err) }
	if len(cfg.Peers.Hosts) != 0 { t.Errorf("expected no peer hosts, got %d", len(cfg.Peers.Hosts)) }
}

func TestCostConfig(t *testing.T) {
	yaml := `
model:
  path: /models/test.gguf
gpus:
  count: 2
  power_limit_watts: 180
cost:
  bidding_zone: 10YFI-1--------U
  timezone: Europe/Helsinki
  transfer:
    winter:
      peak_cents_kwh: 4.28
      offpeak_cents_kwh: 2.49
    summer:
      flat_cents_kwh: 2.49
  electricity_tax_cents_kwh: 2.253
  vat_percent: 25.5
`
	f, err := os.CreateTemp("", "viiwork-*.yaml")
	if err != nil { t.Fatal(err) }
	defer os.Remove(f.Name())
	f.WriteString(yaml)
	f.Close()

	cfg, err := Load(f.Name())
	if err != nil { t.Fatalf("Load failed: %v", err) }
	if cfg.Cost.BiddingZone != "10YFI-1--------U" { t.Errorf("expected Finland zone, got %s", cfg.Cost.BiddingZone) }
	if cfg.Cost.VATPercent != 25.5 { t.Errorf("expected 25.5 VAT, got %f", cfg.Cost.VATPercent) }
	if cfg.Cost.Transfer.Winter.PeakCentsKWh != 4.28 { t.Errorf("expected 4.28 peak, got %f", cfg.Cost.Transfer.Winter.PeakCentsKWh) }
	if cfg.Cost.Transfer.Summer.FlatCentsKWh != 2.49 { t.Errorf("expected 2.49 flat, got %f", cfg.Cost.Transfer.Summer.FlatCentsKWh) }
	if cfg.Cost.ElectricityTaxCentsKWh != 2.253 { t.Errorf("expected 2.253, got %f", cfg.Cost.ElectricityTaxCentsKWh) }
	if cfg.GPUs.PowerLimitWatts != 180 { t.Errorf("expected 180W limit, got %d", cfg.GPUs.PowerLimitWatts) }
}

func TestCostConfigOptional(t *testing.T) {
	yaml := `
model:
  path: /models/test.gguf
gpus:
  count: 2
`
	f, err := os.CreateTemp("", "viiwork-*.yaml")
	if err != nil { t.Fatal(err) }
	defer os.Remove(f.Name())
	f.WriteString(yaml)
	f.Close()

	cfg, err := Load(f.Name())
	if err != nil { t.Fatalf("Load failed: %v", err) }
	if cfg.Cost.BiddingZone != "" { t.Errorf("expected empty default zone, got %s", cfg.Cost.BiddingZone) }
	if cfg.GPUs.PowerLimitWatts != 0 { t.Errorf("expected 0 power limit, got %d", cfg.GPUs.PowerLimitWatts) }
}
