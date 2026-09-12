package supervisor

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/gpu"
)

// Events receives activity events. *activity.Log satisfies it.
type Events interface {
	Emit(typ string, gpuID int, format string, args ...any)
}

type discardEvents struct{}

func (discardEvents) Emit(string, int, string, ...any) {}

// TokenProgressReader is optionally implemented by an engine that can report
// decode progress along with occupancy. The supervisor finds it by type
// assertion (Decision 8), because C2's engine.Load is frozen and has no
// progress fields.
type TokenProgressReader interface {
	LoadProgress(ctx context.Context, addr string) (load engine.Load, decoded, remain int64, err error)
}

// Timing holds the supervisor's internal intervals. Production uses
// DefaultTiming; tests shorten them to milliseconds.
type Timing struct {
	StartProbeInterval time.Duration // probe cadence while starting
	LoadInterval       time.Duration // engine load poll cadence while healthy
	LoadStaleAfter     time.Duration // a load older than this is not used
	GPUCheckWindow     time.Duration // on-GPU check deadline after turning healthy
	RespawnStopGrace   time.Duration // SIGTERM window when stopping for a respawn
	QuickExitWindow    time.Duration // an exit this soon after launch gets a free retry
}

func DefaultTiming() Timing {
	return Timing{
		StartProbeInterval: time.Second,
		LoadInterval:       time.Second,
		LoadStaleAfter:     3 * time.Second,
		GPUCheckWindow:     60 * time.Second,
		RespawnStopGrace:   5 * time.Second,
		QuickExitWindow:    10 * time.Second,
	}
}

// Deps is everything a supervisor needs from its node.
type Deps struct {
	Vendor          gpu.Vendor
	Run             gpu.Runner // nil = gpu.ExecRunner
	Health          config.HealthConfig
	PowerLimitWatts int
	Log             io.Writer        // nil = os.Stdout
	Events          Events           // nil = discard
	Timing          Timing           // zero fields take DefaultTiming values
	Environ         func() []string  // nil = os.Environ
	Now             func() time.Time // nil = time.Now
}

func (d Deps) withDefaults() Deps {
	if d.Vendor == "" {
		d.Vendor = gpu.VendorNone
	}
	if d.Run == nil {
		d.Run = gpu.ExecRunner
	}
	if d.Log == nil {
		d.Log = os.Stdout
	}
	if d.Events == nil {
		d.Events = discardEvents{}
	}
	def := DefaultTiming()
	fill := func(v *time.Duration, fallback time.Duration) {
		if *v <= 0 {
			*v = fallback
		}
	}
	fill(&d.Timing.StartProbeInterval, def.StartProbeInterval)
	fill(&d.Timing.LoadInterval, def.LoadInterval)
	fill(&d.Timing.LoadStaleAfter, def.LoadStaleAfter)
	fill(&d.Timing.GPUCheckWindow, def.GPUCheckWindow)
	fill(&d.Timing.RespawnStopGrace, def.RespawnStopGrace)
	fill(&d.Timing.QuickExitWindow, def.QuickExitWindow)
	if d.Environ == nil {
		d.Environ = os.Environ
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return d
}
