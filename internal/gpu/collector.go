package gpu

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"time"
)

type cmdFunc func(ctx context.Context) ([]byte, error)

type StatCollector struct {
	history     *History
	broadcaster *Broadcaster
	available   atomic.Bool
	// powerAvailable is tracked separately from available: GPU metrics can be
	// working fine while per-GPU wattage is absent, and energy attribution
	// needs to know the difference.
	powerAvailable atomic.Bool
	logger         *log.Logger
	cmdFactory     cmdFunc
}

// argSets are tried in order at startup. Power is requested first because
// per-GPU wattage is what energy attribution is built on, but a rocm-smi that
// rejects --showpower must not take utilisation and VRAM down with it: the flag
// is a bonus, and the second set is exactly what viiwork asked for before it.
var argSets = [][]string{
	{"--showuse", "--showmeminfo", "vram", "--showpower", "--json"},
	{"--showuse", "--showmeminfo", "vram", "--json"},
}

func rocmSMI(args []string) cmdFunc {
	return func(ctx context.Context) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, "rocm-smi", args...).Output()
	}
}

func NewStatCollector(history *History, broadcaster *Broadcaster) *StatCollector {
	return newStatCollector(history, broadcaster, rocmSMI)
}

// newStatCollector takes the command factory so tests can drive the arg-set
// fallback without a GPU. Probing the real thing is the whole point of the
// fallback, so a test that re-implemented the loop would prove nothing.
func newStatCollector(history *History, broadcaster *Broadcaster, factory func([]string) cmdFunc) *StatCollector {
	c := &StatCollector{
		history:     history,
		broadcaster: broadcaster,
		logger:      log.New(os.Stdout, "[gpu] ", log.LstdFlags),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	var lastErr error
	for _, args := range argSets {
		cmd := factory(args)
		out, err := cmd(ctx)
		if err != nil {
			lastErr = err
			continue
		}

		c.cmdFactory = cmd
		c.available.Store(true)

		// Whether power arrived is checked against parsed output, not against
		// the exit status: a rocm-smi can accept --showpower and still report no
		// wattage. Saying so once at startup beats a silently empty column.
		hasPower := false
		for _, sample := range ParseROCmSMI(out) {
			if sample.PowerW > 0 {
				hasPower = true
				break
			}
		}
		c.powerAvailable.Store(hasPower)
		if hasPower {
			c.logger.Println("rocm-smi available, GPU metrics enabled (with per-GPU power)")
		} else {
			c.logger.Printf("rocm-smi available, GPU metrics enabled without per-GPU power (%v)", powerless(lastErr))
		}

		c.Sample(context.Background())
		return c
	}

	c.available.Store(false)
	c.logger.Printf("rocm-smi unavailable: %v (GPU metrics disabled)", lastErr)
	return c
}

// powerless explains why wattage is missing: either --showpower was rejected
// outright, or it was accepted and reported nothing.
func powerless(err error) string {
	if err != nil {
		return "--showpower rejected: " + err.Error()
	}
	return "--showpower reported no wattage"
}

func (c *StatCollector) Sample(ctx context.Context) {
	if !c.available.Load() { return }
	out, err := c.cmdFactory(ctx)
	if err != nil {
		c.logger.Printf("rocm-smi failed: %v", err)
		return
	}
	now := time.Now().Unix()
	samples := ParseROCmSMI(out)
	for i := range samples {
		samples[i].Timestamp = now
		c.history.Record(samples[i])
	}

	type streamGPU struct {
		Util       float64 `json:"util"`
		VRAMUsedMB float64 `json:"vram_used_mb"`
	}
	event := struct {
		T    int64                `json:"t"`
		GPUs map[string]streamGPU `json:"gpus"`
	}{T: now, GPUs: make(map[string]streamGPU)}
	for _, s := range samples {
		event.GPUs[strconv.Itoa(s.GPUID)] = streamGPU{Util: s.Utilization, VRAMUsedMB: s.VRAMUsedMB}
	}
	data, _ := json.Marshal(event)
	c.broadcaster.Broadcast(data)
}

func (c *StatCollector) Available() bool {
	return c.available.Load()
}

// PowerAvailable reports whether samples carry per-GPU wattage.
func (c *StatCollector) PowerAvailable() bool {
	return c.powerAvailable.Load()
}
