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
	logger      *log.Logger
	cmdFactory  cmdFunc
}

func NewStatCollector(history *History, broadcaster *Broadcaster) *StatCollector {
	c := &StatCollector{
		history:     history,
		broadcaster: broadcaster,
		logger:      log.New(os.Stdout, "[gpu] ", log.LstdFlags),
		cmdFactory: func(ctx context.Context) ([]byte, error) {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			return exec.CommandContext(ctx, "rocm-smi", "--showuse", "--showmeminfo", "vram", "--json").Output()
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := c.cmdFactory(ctx)
	if err != nil {
		c.available.Store(false)
		c.logger.Printf("rocm-smi unavailable: %v (GPU metrics disabled)", err)
	} else {
		c.available.Store(true)
		c.logger.Println("rocm-smi available, GPU metrics enabled")
		c.Sample(context.Background())
	}
	return c
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
