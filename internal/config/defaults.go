package config

import (
	"time"

	"github.com/janit/viiwork/v2/energy"
	"github.com/janit/viiwork/v2/internal/activity"
	"github.com/janit/viiwork/v2/internal/power"
)

const (
	DefaultMeshSecretEnv     = "VIIWORK_MESH_SECRET"
	DefaultMeshSecretPrevEnv = "VIIWORK_MESH_SECRET_PREV"
	DefaultTailscaleSocket   = "/var/run/tailscale/tailscaled.sock"
	DefaultEventHistory      = 2000
)

// Defaults returns the spec C1 defaults. Model-level defaults depend on each
// model's engine and are applied by Parse after decoding.
func Defaults() Config {
	return Config{
		Node: NodeConfig{StateDir: "/var/lib/viiwork"},
		API: APIConfig{
			Host: "0.0.0.0",
			Port: 8086,
			CORS: CORSConfig{AllowOrigins: []string{"*.ts.net", "localhost", "127.0.0.1"}},
		},
		Mesh: MeshConfig{
			Network:        NetworkTailnet,
			BindPort:       7946,
			SecretEnv:      DefaultMeshSecretEnv,
			SecretPrevEnv:  DefaultMeshSecretPrevEnv,
			SecretEnforce:  EnforceFull,
			Tailnet:        TailnetConfig{Enabled: ToggleAuto, Socket: DefaultTailscaleSocket},
			LAN:            LANConfig{MDNS: ToggleAuto},
			RejoinInterval: Duration{60 * time.Second},
			CapacityPoll:   Duration{time.Second},
		},
		Routing: RoutingConfig{
			QueueMax:     64,
			QueueTimeout: Duration{20 * time.Second},
			ForwardRetry: 1,
			StaleAfter:   Duration{3 * time.Second},
		},
		GPU: GPUConfig{Vendor: VendorAuto},
		Health: HealthConfig{
			Interval:     Duration{5 * time.Second},
			Timeout:      Duration{10 * time.Second},
			MaxFailures:  3,
			RespawnGrace: Duration{60 * time.Second},
			MaxRespawns:  3,
		},
		Activity: ActivityConfig{
			PromptHistory: activity.DefaultPromptHistory,
			EventHistory:  DefaultEventHistory,
		},
		Power: PowerConfig{Source: power.SourceAuto},
		Energy: EnergyConfig{
			Dir:            "/var/lib/viiwork/energy",
			SampleInterval: Duration{30 * time.Second},
			MinuteSlots:    energy.DefaultMinuteSlots,
			HourSlots:      energy.DefaultHourSlots,
			DaySlots:       energy.DefaultDaySlots,
		},
		Cost: CostConfig{Timezone: "Europe/Helsinki"},
	}
}
