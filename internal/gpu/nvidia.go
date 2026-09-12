package gpu

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// nvidiaArgSets are probed in order at construction, like the ROCm
// collector's: power is asked for first because energy attribution is built on
// per-GPU wattage, but a driver that rejects power.draw must not cost
// utilisation and VRAM.
var nvidiaArgSets = [][]string{
	{"--query-gpu=index,utilization.gpu,memory.used,memory.total,power.draw", "--format=csv,noheader,nounits"},
	{"--query-gpu=index,utilization.gpu,memory.used,memory.total", "--format=csv,noheader,nounits"},
}

// NVIDIACollector samples utilisation, VRAM and board power with nvidia-smi.
// It records into the same History and broadcasts the same live event as the
// ROCm StatCollector, so one set of dashboards serves both vendors.
type NVIDIACollector struct {
	history     *History
	broadcaster *Broadcaster
	run         Runner
	args        []string
	available   atomic.Bool
	// powerAvailable is judged from parsed samples, not from the exit status:
	// nvidia-smi accepts power.draw on hardware that then prints [N/A] for
	// every card.
	powerAvailable atomic.Bool
	logger         *log.Logger
}

func NewNVIDIACollector(history *History, broadcaster *Broadcaster, run Runner) *NVIDIACollector {
	c := &NVIDIACollector{
		history:     history,
		broadcaster: broadcaster,
		run:         run,
		logger:      log.New(os.Stdout, "[gpu] ", log.LstdFlags),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	for _, args := range nvidiaArgSets {
		out, err := run(ctx, "nvidia-smi", args...)
		if err != nil {
			continue
		}
		samples := parseNVIDIAStats(out, time.Now().Unix())
		if len(samples) == 0 {
			continue
		}
		c.args = args
		c.available.Store(true)
		hasPower := false
		for _, s := range samples {
			if s.PowerW > 0 {
				hasPower = true
				break
			}
		}
		c.powerAvailable.Store(hasPower)
		c.logger.Printf("nvidia-smi available, GPU metrics enabled (per-GPU power: %v)", hasPower)
		c.Sample(context.Background())
		return c
	}

	c.logger.Println("nvidia-smi unavailable (GPU metrics disabled)")
	return c
}

func (c *NVIDIACollector) Sample(ctx context.Context) {
	if !c.available.Load() {
		return
	}
	out, err := c.run(ctx, "nvidia-smi", c.args...)
	if err != nil {
		c.logger.Printf("nvidia-smi failed: %v", err)
		return
	}
	now := time.Now().Unix()
	samples := parseNVIDIAStats(out, now)
	for _, s := range samples {
		c.history.Record(s)
	}
	c.broadcaster.Broadcast(streamEvent(now, samples))
}

func (c *NVIDIACollector) Available() bool { return c.available.Load() }

// PowerAvailable reports whether samples carry per-GPU wattage.
func (c *NVIDIACollector) PowerAvailable() bool { return c.powerAvailable.Load() }

// streamEvent is the live event StatCollector.Sample broadcasts, field for
// field: the dashboards parse this shape whichever vendor produced it.
func streamEvent(now int64, samples []GPUSample) []byte {
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
	return data
}

// parseNVIDIAStats reads `index, utilization.gpu, memory.used, memory.total[,
// power.draw]` rows. A row with fewer than 4 fields, a non-integer index or an
// unreadable memory.total is not a card and is skipped; an unreadable
// utilisation, memory.used or power ([N/A], empty) leaves only that field at
// zero, because a card that cannot report its draw still reports its VRAM.
func parseNVIDIAStats(out []byte, now int64) []GPUSample {
	var samples []GPUSample
	for _, row := range csvRows(out) {
		if len(row) < 4 {
			continue
		}
		index, err := strconv.Atoi(row[0])
		if err != nil {
			continue
		}
		total, ok := nvidiaFloat(row[3])
		if !ok {
			continue
		}
		s := GPUSample{GPUID: index, VRAMTotalMB: total, Timestamp: now}
		s.Utilization, _ = nvidiaFloat(row[1])
		s.VRAMUsedMB, _ = nvidiaFloat(row[2])
		if len(row) > 4 {
			s.PowerW, _ = nvidiaFloat(row[4])
		}
		samples = append(samples, s)
	}
	return samples
}

// nvidiaFloat parses one CSV value; nvidia-smi writes unsupported values as
// "[N/A]" or "[Not Supported]".
func nvidiaFloat(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "[") {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// Identity is a card's stable identity, which GPUSample does not carry and
// meshapi.GPUInfo publishes.
type Identity struct {
	Index int
	UUID  string
	Name  string
}

// Inventory lists the machine's cards. NVIDIA parses `nvidia-smi -L` lines of
// the form "GPU <index>: <name> (UUID: <uuid>)", in index order. AMD returns
// nil, nil: no rocm-smi identity output has been captured yet, so AMD GPUInfo
// carries index and vendor only, and absent is not zero.
func Inventory(ctx context.Context, vendor Vendor, run Runner) ([]Identity, error) {
	if vendor != VendorNVIDIA {
		return nil, nil
	}
	out, err := run(ctx, "nvidia-smi", "-L")
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi -L: %w", err)
	}
	var ids []Identity
	for _, line := range strings.Split(string(out), "\n") {
		if id, ok := parseNVIDIAListLine(strings.TrimSpace(line)); ok {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Index < ids[j].Index })
	return ids, nil
}

func parseNVIDIAListLine(line string) (Identity, bool) {
	rest, ok := strings.CutPrefix(line, "GPU ")
	if !ok {
		return Identity{}, false
	}
	indexText, rest, ok := strings.Cut(rest, ": ")
	if !ok {
		return Identity{}, false
	}
	index, err := strconv.Atoi(indexText)
	if err != nil {
		return Identity{}, false
	}
	open := strings.LastIndex(rest, " (UUID: ")
	if open < 0 || !strings.HasSuffix(rest, ")") {
		return Identity{}, false
	}
	name := rest[:open]
	uuid := strings.TrimSuffix(rest[open+len(" (UUID: "):], ")")
	return Identity{Index: index, UUID: uuid, Name: name}, true
}
