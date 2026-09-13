// Package config is viiwork 2's node configuration, spec contract C1: one
// viiwork.yaml per machine. Parse decodes it strictly and fills defaults,
// Validate enforces the spec's rules, and Load does both.
package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/janit/viiwork/v2/internal/pipeline"
	"gopkg.in/yaml.v3"
)

const (
	NetworkTailnet = "tailnet"
	NetworkLAN     = "lan"

	EnforceFull     = "full"
	EnforceOutgoing = "outgoing"
	EnforceNone     = "none"

	VendorAuto   = "auto"
	VendorNVIDIA = "nvidia"
	VendorAMD    = "amd"
	VendorNone   = "none"
)

// Duration is a time.Duration written as a Go duration string ("60s").
type Duration struct{ time.Duration }

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

// Toggle is a tri-state switch. "auto" means the caller decides from context,
// for example tailnet discovery defaults on only when mesh.network is tailnet.
type Toggle string

const (
	ToggleAuto Toggle = "auto"
	ToggleOn   Toggle = "true"
	ToggleOff  Toggle = "false"
)

// UnmarshalYAML accepts auto/true/false plus yes/no/on/off, case-insensitively.
// A custom decoder is needed because yaml.v3 will not decode a YAML boolean
// into a string field.
func (t *Toggle) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: expected auto, true or false", value.Line)
	}
	switch strings.ToLower(strings.TrimSpace(value.Value)) {
	case "auto":
		*t = ToggleAuto
	case "true", "yes", "on":
		*t = ToggleOn
	case "false", "no", "off":
		*t = ToggleOff
	default:
		return fmt.Errorf("line %d: %q is not auto, true or false", value.Line, value.Value)
	}
	return nil
}

// Resolve reports whether the switch is on; auto yields the caller's default.
func (t Toggle) Resolve(auto bool) bool {
	switch t {
	case ToggleOn:
		return true
	case ToggleOff:
		return false
	default:
		return auto
	}
}

func (t Toggle) valid() bool { return t == ToggleAuto || t == ToggleOn || t == ToggleOff }

type Config struct {
	Node      NodeConfig                         `yaml:"node"`
	API       APIConfig                          `yaml:"api"`
	Mesh      MeshConfig                         `yaml:"mesh"`
	Routing   RoutingConfig                      `yaml:"routing"`
	GPU       GPUConfig                          `yaml:"gpu"`
	Models    []Model                            `yaml:"models"`
	Health    HealthConfig                       `yaml:"health"`
	Activity  ActivityConfig                     `yaml:"activity"`
	Power     PowerConfig                        `yaml:"power"`
	Energy    EnergyConfig                       `yaml:"energy"`
	Cost      CostConfig                         `yaml:"cost"`
	Pipelines map[string]pipeline.PipelineConfig `yaml:"pipelines"`
}

type NodeConfig struct {
	// Name is the node's mesh identity. Empty means os.Hostname(), resolved
	// at startup (P6), not here, so Parse stays pure.
	Name string `yaml:"name"`
	// StateDir holds durable node state such as the alias table.
	StateDir string `yaml:"state_dir"`
}

type APIConfig struct {
	Host string     `yaml:"host"`
	Port int        `yaml:"port"`
	CORS CORSConfig `yaml:"cors"`
}

// CORSConfig is unchanged from v1: the browser-origin allowlist of an API
// that authenticates nothing.
type CORSConfig struct {
	AllowOrigins    []string `yaml:"allow_origins"`
	AllowTailnetIPs *bool    `yaml:"allow_tailnet_ips"`
}

type MeshConfig struct {
	Network        string        `yaml:"network"`
	BindPort       int           `yaml:"bind_port"`
	Advertise      string        `yaml:"advertise"`
	Open           bool          `yaml:"open"`
	SecretEnv      string        `yaml:"secret_env"`
	SecretPrevEnv  string        `yaml:"secret_prev_env"`
	SecretEnforce  string        `yaml:"secret_enforce"`
	Tailnet        TailnetConfig `yaml:"tailnet"`
	LAN            LANConfig     `yaml:"lan"`
	Seeds          []string      `yaml:"seeds"`
	RejoinInterval Duration      `yaml:"rejoin_interval"`
	CapacityPoll   Duration      `yaml:"capacity_poll"`
}

type TailnetConfig struct {
	Enabled Toggle `yaml:"enabled"`
	Socket  string `yaml:"socket"`
}

type LANConfig struct {
	MDNS Toggle `yaml:"mdns"`
}

type RoutingConfig struct {
	QueueMax     int      `yaml:"queue_max"`
	QueueTimeout Duration `yaml:"queue_timeout"`
	ForwardRetry int      `yaml:"forward_retry"`
	StaleAfter   Duration `yaml:"stale_after"`
}

type GPUConfig struct {
	Vendor          string `yaml:"vendor"`
	PowerLimitWatts int    `yaml:"power_limit_watts"`
}

// Model is one models[] entry. Context is tokens PER SLOT for every engine;
// engines translate it (llama.cpp --ctx-size = context * parallel).
type Model struct {
	Name           string            `yaml:"name"`
	Engine         string            `yaml:"engine"`
	Path           string            `yaml:"path"`
	GPUs           []int             `yaml:"gpus"`
	GPUsPerBackend int               `yaml:"gpus_per_backend"`
	Context        int               `yaml:"context"`
	Parallel       int               `yaml:"parallel"`
	StartupTimeout Duration          `yaml:"startup_timeout"`
	Args           []string          `yaml:"args"`
	Env            map[string]string `yaml:"env"`
	// Options collects every key this struct does not define, which is how a
	// model carries its engine's own configuration block. Exactly one is
	// allowed and it must be named for the model's engine; Parse rejects
	// anything else, so a typo is still an error that names the key. The
	// engine decodes its own block (engine.DecodeOptions) and config never
	// looks inside — which is what keeps the list of engines that exist out of
	// this package.
	Options map[string]yaml.Node `yaml:",inline"`
}

// EngineBlock is this model's engine configuration block, or a zero Node when
// the operator wrote none. Parse guarantees any block present is named for
// this model's engine.
func (m Model) EngineBlock() yaml.Node {
	if n, ok := m.Options[m.Engine]; ok {
		return n
	}
	return yaml.Node{}
}

// Equal reports whether two models are the same configuration.
//
// This cannot be reflect.DeepEqual, and the reason is a trap worth stating: an
// engine block is a yaml.Node, which carries the Line and Column it was written
// at. Two parses of the same model at different places in a file are therefore
// not DeepEqual, and a supervisor diffing on that would restart a live backend
// because an unrelated model above it gained a line. Compare the block by what
// it says instead of where it was said.
func (m Model) Equal(o Model) bool {
	a, b := m, o
	a.Options, b.Options = nil, nil
	if !reflect.DeepEqual(a, b) {
		return false
	}
	return blockText(m.Options) == blockText(o.Options)
}

// blockText renders an options map to a stable string, keys in sorted order.
func blockText(opts map[string]yaml.Node) string {
	if len(opts) == 0 {
		return ""
	}
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		node := opts[k]
		sb.WriteString(k)
		sb.WriteByte(':')
		if b, err := yaml.Marshal(&node); err == nil {
			sb.Write(b)
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// Backends is how many backend processes the model runs: one per
// gpus_per_backend-sized group of its GPUs, or one CPU backend with no GPUs.
func (m Model) Backends() int {
	if len(m.GPUs) == 0 || m.GPUsPerBackend < 1 {
		return 1
	}
	return len(m.GPUs) / m.GPUsPerBackend
}

// BackendGPUs returns a copy of backend i's GPUs in config order, or nil for a
// CPU model.
func (m Model) BackendGPUs(i int) []int {
	if len(m.GPUs) == 0 {
		return nil
	}
	n := m.GPUsPerBackend
	return append([]int(nil), m.GPUs[i*n:(i+1)*n]...)
}

type HealthConfig struct {
	Interval     Duration `yaml:"interval"`
	Timeout      Duration `yaml:"timeout"`
	MaxFailures  int      `yaml:"max_failures"`
	RespawnGrace Duration `yaml:"respawn_grace"`
	MaxRespawns  int      `yaml:"max_respawns"`
}

type ActivityConfig struct {
	PromptHistory int `yaml:"prompt_history"`
	EventHistory  int `yaml:"event_history"`
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

// PowerConfig selects how node wattage is read over IPMI. The right answer is
// board-specific: the "Power Supply" sensor class carries wattage on some BMCs
// and only presence flags on others (every Gigabyte board in the gfx906 fleet),
// which is why probing replaced the single hardcoded command.
type PowerConfig struct {
	// Source is one of:
	//   auto           probe dcmi, then the Power Supply class, then any
	//                  Watts-valued sensor; keep the first that answers
	//   dcmi           DCMI whole-node power reading
	//   sdr            sum the Power Supply sensor class
	//   sensor:<NAME>  read one named sensor, e.g. sensor:SYS_POWER
	//   none           disable power monitoring without probing
	// Empty means auto.
	Source string `yaml:"source"`

	// Control turns on chassis power control. Off by default, and off is the
	// only safe default: this is the one part of the API that can switch a
	// machine off, on a service that authenticates nothing.
	Control PowerControlConfig `yaml:"control"`
}

// PowerControlConfig gates chassis power control.
//
// Hosts is an allowlist and there is no wildcard. On an API with no
// authentication, "someone wrote this hostname down" is the whole guard, so it
// has to be deliberate rather than inferred from the mesh — a node should not
// gain power over a machine merely by having been peered with it.
type PowerControlConfig struct {
	Enabled bool     `yaml:"enabled"`
	Hosts   []string `yaml:"hosts"`
	// BMC is the out-of-band path, needed only for hosts that must be
	// controllable while powered off. A running host is reached in-band, with
	// no credentials at all.
	BMC BMCConfig `yaml:"bmc"`
}

type BMCConfig struct {
	Username string `yaml:"username"`
	// PasswordEnv names the environment variable holding the BMC password.
	// Preferred over Password: viiwork.yaml is deployment-specific but still a
	// file people paste into issues, and .env is already how the ENTSO-E key
	// is supplied. Defaults to BMC_PASSWORD.
	PasswordEnv string `yaml:"password_env"`
	// Password is the inline fallback. Works, but puts a credential that can
	// power off a machine into a config file.
	Password string `yaml:"password"`
	// Addresses maps hostname to BMC address. Optional per host: a node
	// discovers its own BMC address in-band and publishes it, so a host seen
	// online at least once needs no entry. Write one for a host that must be
	// reachable before this node has ever seen it up -- and note these are
	// often DHCP, so a written address can go stale while a learned one cannot.
	Addresses map[string]string `yaml:"addresses"`
}

// EnergyConfig configures the durable kWh store. Disabled by default: it needs
// a writable directory that outlives the container, and defaulting it on would
// happily write to a container filesystem and lose everything on restart --
// durable in name only.
//
// Node wattage is a whole-host measurement. Running several viiwork instances on
// one host (the way multi-model hosts are deployed) and enabling this on more
// than one would record that same host draw several times over, so enable it on
// exactly one instance per host.
type EnergyConfig struct {
	Enabled bool   `yaml:"enabled"`
	Dir     string `yaml:"dir"`
	// SampleInterval is how often power is read. Records are always one per
	// minute; sampling faster is what makes that minute an average rather than
	// a coin flip on a BMC that refreshes every ~60s.
	SampleInterval Duration `yaml:"sample_interval"`
	// Slot counts are ring lengths, and therefore the retention. Changing one
	// changes the file geometry and discards that ring's history.
	MinuteSlots int `yaml:"minute_slots"`
	HourSlots   int `yaml:"hour_slots"`
	DaySlots    int `yaml:"day_slots"`
}
