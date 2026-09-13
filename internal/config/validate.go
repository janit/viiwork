package config

import (
	"encoding/base64"
	"fmt"
	"github.com/janit/viiwork/v2/internal/engine"
	"net/netip"
	"os"
	"strings"
	"unicode"

	"github.com/janit/viiwork/v2/internal/power"
)

// Load reads, parses and validates a config file.
func Load(path string, lookupEnv func(string) (string, bool)) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(lookupEnv); err != nil {
		return nil, err
	}
	return cfg, nil
}

// MeshKeys are the decoded mesh secrets. A nil Primary means an open mesh.
type MeshKeys struct {
	Primary  []byte
	Previous []byte
}

// Open reports whether the mesh runs without a secret.
func (k MeshKeys) Open() bool { return k.Primary == nil }

// MeshKeys resolves the secrets named by mesh.secret_env and
// mesh.secret_prev_env and enforces the mesh-mode rules: a secret or
// mesh.open, never both and never neither.
func (c *Config) MeshKeys(lookupEnv func(string) (string, bool)) (MeshKeys, error) {
	primaryEnv := orDefault(c.Mesh.SecretEnv, DefaultMeshSecretEnv)
	prevEnv := orDefault(c.Mesh.SecretPrevEnv, DefaultMeshSecretPrevEnv)

	var keys MeshKeys
	if v, ok := lookupEnv(primaryEnv); ok && v != "" {
		k, err := decodeKey(primaryEnv, v)
		if err != nil {
			return MeshKeys{}, err
		}
		keys.Primary = k
	}
	if v, ok := lookupEnv(prevEnv); ok && v != "" {
		if keys.Primary == nil {
			return MeshKeys{}, fmt.Errorf("mesh.secret_prev_env: %s is set but %s is not", prevEnv, primaryEnv)
		}
		k, err := decodeKey(prevEnv, v)
		if err != nil {
			return MeshKeys{}, err
		}
		keys.Previous = k
	}

	switch {
	case keys.Primary != nil && c.Mesh.Open:
		return MeshKeys{}, fmt.Errorf("mesh.open is true but %s is set: choose a secured mesh or an open one, not both", primaryEnv)
	case keys.Primary == nil && !c.Mesh.Open:
		return MeshKeys{}, fmt.Errorf("mesh: %s is not set and mesh.open is not true: set %s (base64 of 32 bytes, openssl rand -base64 32) or declare mesh.open: true", primaryEnv, primaryEnv)
	case keys.Primary == nil && c.Mesh.SecretEnforce != EnforceFull:
		return MeshKeys{}, fmt.Errorf("mesh.secret_enforce %q requires %s: levels below full exist only for adding a secret to a running open mesh", c.Mesh.SecretEnforce, primaryEnv)
	}
	return keys, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func decodeKey(env, v string) ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil {
		return nil, fmt.Errorf("%s is not standard base64: %w", env, err)
	}
	if len(k) != 32 {
		return nil, fmt.Errorf("%s decodes to %d bytes, need exactly 32 (openssl rand -base64 32)", env, len(k))
	}
	return k, nil
}

// Validate enforces every C1 rule. Errors name the offending field.
func (c *Config) Validate(lookupEnv func(string) (string, bool)) error {
	if strings.TrimSpace(c.Node.StateDir) == "" {
		return fmt.Errorf("node.state_dir is required")
	}
	if c.API.Port < 1 || c.API.Port > 65535 {
		return fmt.Errorf("api.port must be 1-65535, got %d", c.API.Port)
	}
	if err := c.validateMesh(); err != nil {
		return err
	}
	if _, err := c.MeshKeys(lookupEnv); err != nil {
		return err
	}
	if err := c.validateRouting(); err != nil {
		return err
	}
	switch c.GPU.Vendor {
	case VendorAuto, VendorNVIDIA, VendorAMD, VendorNone:
	default:
		return fmt.Errorf("gpu.vendor %q must be one of: auto, nvidia, amd, none", c.GPU.Vendor)
	}
	if c.GPU.PowerLimitWatts < 0 {
		return fmt.Errorf("gpu.power_limit_watts must be >= 0")
	}
	if err := c.validateHealth(); err != nil {
		return err
	}
	if c.Activity.PromptHistory < 0 {
		return fmt.Errorf("activity.prompt_history must be >= 0")
	}
	if c.Activity.EventHistory < 0 {
		return fmt.Errorf("activity.event_history must be >= 0")
	}
	if err := validatePowerControl(&c.Power.Control); err != nil {
		return err
	}
	if err := validatePowerSource(c.Power.Source); err != nil {
		return err
	}
	if c.Energy.Enabled {
		if c.Energy.Dir == "" {
			return fmt.Errorf("energy.dir is required when energy.enabled is true")
		}
		if c.Energy.SampleInterval.Duration <= 0 {
			return fmt.Errorf("energy.sample_interval must be positive")
		}
	}
	return c.validateModels()
}

func (c *Config) validateMesh() error {
	m := c.Mesh
	switch m.Network {
	case NetworkTailnet, NetworkLAN:
	default:
		return fmt.Errorf("mesh.network %q must be tailnet or lan", m.Network)
	}
	if m.BindPort < 1 || m.BindPort > 65535 {
		return fmt.Errorf("mesh.bind_port must be 1-65535, got %d", m.BindPort)
	}
	if m.Advertise != "" {
		if _, err := netip.ParseAddr(m.Advertise); err != nil {
			return fmt.Errorf("mesh.advertise %q must be an IP address: %w", m.Advertise, err)
		}
	}
	switch m.SecretEnforce {
	case EnforceFull, EnforceOutgoing, EnforceNone:
	default:
		return fmt.Errorf("mesh.secret_enforce %q must be full, outgoing or none", m.SecretEnforce)
	}
	if !m.Tailnet.Enabled.valid() {
		return fmt.Errorf("mesh.tailnet.enabled %q must be auto, true or false", m.Tailnet.Enabled)
	}
	if !m.LAN.MDNS.valid() {
		return fmt.Errorf("mesh.lan.mdns %q must be auto, true or false", m.LAN.MDNS)
	}
	if m.Tailnet.Enabled.Resolve(m.Network == NetworkTailnet) && strings.TrimSpace(m.Tailnet.Socket) == "" {
		return fmt.Errorf("mesh.tailnet.socket is required when tailnet discovery is on")
	}
	for i, s := range m.Seeds {
		if _, err := netip.ParseAddrPort(s); err != nil {
			return fmt.Errorf("mesh.seeds[%d] %q must be ip:port: %w", i, s, err)
		}
	}
	if m.RejoinInterval.Duration <= 0 {
		return fmt.Errorf("mesh.rejoin_interval must be positive")
	}
	if m.CapacityPoll.Duration <= 0 {
		return fmt.Errorf("mesh.capacity_poll must be positive")
	}
	return nil
}

func (c *Config) validateRouting() error {
	r := c.Routing
	switch {
	case r.QueueMax < 0:
		return fmt.Errorf("routing.queue_max must be >= 0 (0 = no queue)")
	case r.QueueTimeout.Duration < 0:
		return fmt.Errorf("routing.queue_timeout must be >= 0")
	case r.ForwardRetry < 0:
		return fmt.Errorf("routing.forward_retry must be >= 0")
	case r.StaleAfter.Duration <= 0:
		return fmt.Errorf("routing.stale_after must be positive")
	}
	return nil
}

func (c *Config) validateHealth() error {
	h := c.Health
	switch {
	case h.Interval.Duration <= 0:
		return fmt.Errorf("health.interval must be positive")
	case h.Timeout.Duration <= 0:
		return fmt.Errorf("health.timeout must be positive")
	case h.MaxFailures < 1:
		return fmt.Errorf("health.max_failures must be >= 1")
	case h.RespawnGrace.Duration < 0:
		return fmt.Errorf("health.respawn_grace must be >= 0")
	case h.MaxRespawns < 0:
		return fmt.Errorf("health.max_respawns must be >= 0")
	}
	return nil
}

func (c *Config) validateModels() error {
	names := map[string]int{}
	owner := map[int]int{} // GPU index -> index of the model that owns it
	for i := range c.Models {
		m := &c.Models[i]
		p := fmt.Sprintf("models[%d]", i)

		if m.Name == "" || strings.IndexFunc(m.Name, unicode.IsSpace) >= 0 {
			return fmt.Errorf("%s.name %q must be non-empty and contain no whitespace", p, m.Name)
		}
		if j, dup := names[m.Name]; dup {
			return fmt.Errorf("%s.name %q is already used by models[%d]", p, m.Name, j)
		}
		names[m.Name] = i

		eng, ok := engine.Lookup(m.Engine)
		if !ok {
			// An empty registry is a build wiring fault, not an operator's
			// mistake, and "must be one of: " with nothing after it would send
			// them looking in the wrong place.
			if names := engine.Names(); len(names) > 0 {
				return fmt.Errorf("%s.engine %q must be one of: %s", p, m.Engine, strings.Join(names, ", "))
			}
			return fmt.Errorf("%s.engine %q: this binary registers no engines (an engine package must be blank-imported)", p, m.Engine)
		}
		if strings.TrimSpace(m.Path) == "" {
			return fmt.Errorf("%s.path is required", p)
		}

		if m.GPUsPerBackend < 1 {
			return fmt.Errorf("%s.gpus_per_backend must be >= 1, got %d", p, m.GPUsPerBackend)
		}
		if len(m.GPUs) == 0 && !runsOnCPU(eng) {
			return fmt.Errorf("%s.gpus is required for engine %s (it cannot run on CPU)", p, m.Engine)
		}
		for _, g := range m.GPUs {
			if g < 0 {
				return fmt.Errorf("%s.gpus: %d is not a GPU index", p, g)
			}
			if j, used := owner[g]; used {
				if j == i {
					return fmt.Errorf("%s.gpus lists GPU %d twice", p, g)
				}
				return fmt.Errorf("%s.gpus: GPU %d is already used by models[%d] (%s)", p, g, j, c.Models[j].Name)
			}
			owner[g] = i
		}
		if len(m.GPUs)%m.GPUsPerBackend != 0 {
			return fmt.Errorf("%s: %d GPUs are not divisible by gpus_per_backend %d", p, len(m.GPUs), m.GPUsPerBackend)
		}

		if m.Context < 1 {
			return fmt.Errorf("%s.context (tokens per slot) must be >= 1", p)
		}
		if m.Parallel < 1 {
			return fmt.Errorf("%s.parallel must be >= 1", p)
		}
		if m.StartupTimeout.Duration < 0 {
			return fmt.Errorf("%s.startup_timeout must be >= 0", p)
		}
		if v, ok := eng.(engine.OptionsValidator); ok {
			if err := v.ValidateOptions(p, ModelSpec(*m)); err != nil {
				return err
			}
		}
	}
	return nil
}

// runsOnCPU reports whether an engine has declared it can serve with no GPUs.
// Not implementing engine.CPURunner means it cannot, which is the safe default
// for an engine whose author did not think about it.
func runsOnCPU(e engine.Engine) bool {
	c, ok := e.(engine.CPURunner)
	return ok && c.RunsOnCPU()
}

// ModelSpec builds the engine.Spec for a model's FIRST backend, which is what
// an engine validates its options against: Spec.GPUs is one backend's cards,
// so a rule about how many cards a backend has (llama.cpp's split weights, an
// engine that binds exactly one) reads the same here as at launch. Port and
// Vendor are zero — nothing has been assigned yet, and no options rule may
// depend on them.
func ModelSpec(m Model) engine.Spec {
	return engine.Spec{
		Name:     m.Name,
		Path:     m.Path,
		GPUs:     m.BackendGPUs(0),
		Context:  m.Context,
		Parallel: m.Parallel,
		Backends: m.Backends(),
		Args:     m.Args,
		Options:  m.EngineBlock(),
	}
}

// validatePowerControl fails startup on a power-control block that would not do
// what it appears to say. Enabling control with no hosts listed is the case
// worth catching: it looks armed and controls nothing, and the operator finds
// out by clicking a button that refuses.
func validatePowerControl(pc *PowerControlConfig) error {
	if !pc.Enabled {
		return nil
	}
	if len(pc.Hosts) == 0 {
		return fmt.Errorf("power.control.enabled is true but power.control.hosts is empty: list the hosts that may be controlled")
	}
	for _, h := range pc.Hosts {
		if strings.TrimSpace(h) == "" {
			return fmt.Errorf("power.control.hosts contains an empty hostname")
		}
	}
	return nil
}

// validatePowerSource rejects a misspelled source at startup. Without this a
// typo would fall through the sampler's probe and silently disable power and
// cost tracking, which is exactly the failure mode this feature exists to end.
func validatePowerSource(source string) error {
	switch s := strings.TrimSpace(source); {
	case s == "", s == power.SourceAuto, s == power.SourceDCMI,
		s == power.SourceSDR, s == power.SourceNone:
		return nil
	case strings.HasPrefix(s, power.SourceSensorPrefix):
		if strings.TrimSpace(strings.TrimPrefix(s, power.SourceSensorPrefix)) == "" {
			return fmt.Errorf("power.source %q needs a sensor name, e.g. %qSYS_POWER", s, power.SourceSensorPrefix)
		}
		return nil
	default:
		return fmt.Errorf("power.source %q must be one of: auto, dcmi, sdr, none, sensor:<NAME>", s)
	}
}
