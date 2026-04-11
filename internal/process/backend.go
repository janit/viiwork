package process

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/janit/viiwork/internal/balancer"
)

var healthClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        100,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	},
}

type Backend struct {
	GPUID         int   // -1 if TensorSplit (the backend spans multiple GPUs)
	GPUIDs        []int // populated only when TensorSplit; the device list
	TensorSplit   bool
	SplitMode     string    // "layer" or "row"; ignored if !TensorSplit
	SplitWeights  []float64 // optional per-GPU split fractions; default even
	MainGPU       int       // for SplitMode="row"; ignored otherwise
	ModelPath     string
	Port          int
	ContextSize   int
	NGPULayers    int
	Parallel      int
	Binary        string
	ExtraArgs     []string
	HealthTimeout   time.Duration
	PowerLimitWatts int
	State           *balancer.BackendState
	LogWriter     io.Writer
	cmd           *exec.Cmd
}

// label returns a human-readable identifier for log lines: "gpu-4" for a
// single-GPU replica backend, or "ts-4,5,6,7" for a tensor-split aggregate.
func (b *Backend) label() string {
	if b.TensorSplit {
		return "ts-" + joinInts(b.GPUIDs, ",")
	}
	return "gpu-" + strconv.Itoa(b.GPUID)
}

func joinInts(xs []int, sep string) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, sep)
}

func joinFloats(xs []float64, sep string) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.FormatFloat(x, 'f', -1, 64)
	}
	return strings.Join(parts, sep)
}

// evenWeights returns "1,1,1,..." with n copies, used as the default
// --tensor-split argument when no explicit weights are configured.
func evenWeights(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "1"
	}
	return strings.Join(parts, ",")
}

func (b *Backend) buildArgs() []string {
	parallel := b.Parallel
	if parallel < 1 {
		parallel = 1
	}
	args := []string{
		"--model", b.ModelPath,
		"--port", strconv.Itoa(b.Port),
		"--ctx-size", strconv.Itoa(b.ContextSize),
		"--n-gpu-layers", strconv.Itoa(b.NGPULayers),
		"--parallel", strconv.Itoa(parallel),
		"--slots",        // enable /slots endpoint for context monitoring
		"--log-disable",  // suppress per-request logging (viiwork polls /slots every 1s)
	}
	if b.TensorSplit {
		mode := b.SplitMode
		if mode == "" {
			mode = "layer"
		}
		args = append(args, "--split-mode", mode)
		if len(b.SplitWeights) > 0 {
			args = append(args, "--tensor-split", joinFloats(b.SplitWeights, ","))
		} else {
			args = append(args, "--tensor-split", evenWeights(len(b.GPUIDs)))
		}
		if mode == "row" {
			args = append(args, "--main-gpu", strconv.Itoa(b.MainGPU))
		}
	}
	args = append(args, b.ExtraArgs...)
	return args
}

func (b *Backend) buildEnv() []string {
	env := os.Environ()
	if b.TensorSplit {
		env = append(env, "ROCR_VISIBLE_DEVICES="+joinInts(b.GPUIDs, ","))
	} else {
		env = append(env, fmt.Sprintf("ROCR_VISIBLE_DEVICES=%d", b.GPUID))
	}
	// Reduce glibc malloc fragmentation in long-running llama-server processes:
	// - MMAP_THRESHOLD: allocations >64KB use mmap, freed immediately via munmap
	// - TRIM_THRESHOLD: aggressively trim the heap on free
	// - ARENA_MAX: limit arena count to prevent memory hoarding across threads
	env = append(env, "MALLOC_MMAP_THRESHOLD_=65536")
	env = append(env, "MALLOC_TRIM_THRESHOLD_=65536")
	env = append(env, "MALLOC_ARENA_MAX=4")
	return env
}

func (b *Backend) Start() error {
	b.State.SetStatus(balancer.StatusStarting)
	if b.PowerLimitWatts > 0 {
		// In tensor-split mode the same limit is applied to every device in
		// the group. In replica mode there's just the one GPUID.
		ids := []int{b.GPUID}
		if b.TensorSplit {
			ids = b.GPUIDs
		}
		for _, id := range ids {
			out, err := exec.Command("rocm-smi", "--setpoweroverdrive", strconv.Itoa(b.PowerLimitWatts), "-d", strconv.Itoa(id)).CombinedOutput()
			if err != nil {
				log.Printf("[%s] WARNING: failed to set power limit to %dW on gpu-%d: %v (%s)", b.label(), b.PowerLimitWatts, id, err, string(out))
			} else {
				log.Printf("[%s] gpu-%d power limit set to %dW", b.label(), id, b.PowerLimitWatts)
			}
		}
	}
	b.cmd = exec.Command(b.Binary, b.buildArgs()...)
	b.cmd.Env = b.buildEnv()
	if b.LogWriter != nil {
		b.cmd.Stdout = b.LogWriter
		b.cmd.Stderr = b.LogWriter
	}
	if err := b.cmd.Start(); err != nil {
		return fmt.Errorf("starting backend %s: %w", b.label(), err)
	}
	return nil
}

func (b *Backend) Stop() error {
	if b.cmd == nil || b.cmd.Process == nil { return nil }
	return b.cmd.Process.Signal(syscall.SIGTERM)
}

func (b *Backend) Kill() error {
	if b.cmd == nil || b.cmd.Process == nil { return nil }
	return b.cmd.Process.Kill()
}

func (b *Backend) Wait() error {
	if b.cmd == nil { return nil }
	return b.cmd.Wait()
}

func (b *Backend) IsRunning() bool {
	if b.cmd == nil || b.cmd.Process == nil {
		return false
	}
	// Signal 0 checks process existence without sending a signal.
	return b.cmd.Process.Signal(syscall.Signal(0)) == nil
}

func (b *Backend) CheckHealth(ctx context.Context) bool {
	url := fmt.Sprintf("http://%s/health", b.State.Addr)
	ctx, cancel := context.WithTimeout(ctx, b.HealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil { return false }
	resp, err := healthClient.Do(req)
	if err != nil { return false }
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) // drain for connection reuse
	return resp.StatusCode == 200
}

// SlotStats holds per-backend slot summary from llama-server /slots endpoint.
type SlotStats struct {
	NCtx       int64 // context size per slot
	NSlots     int   // total slots
	NActive    int   // slots with is_processing=true
	NDecoded   int64 // total tokens decoded across active slots
	NRemain    int64 // total tokens remaining across active slots
}

func (b *Backend) ReadSlots(ctx context.Context) SlotStats {
	url := fmt.Sprintf("http://%s/slots", b.State.Addr)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return SlotStats{}
	}
	resp, err := healthClient.Do(req)
	if err != nil {
		return SlotStats{}
	}
	defer resp.Body.Close()
	var slots []struct {
		NCtx         int64 `json:"n_ctx"`
		IsProcessing bool  `json:"is_processing"`
		NextToken    []struct {
			NDecoded int64 `json:"n_decoded"`
			NRemain  int64 `json:"n_remain"`
		} `json:"next_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&slots); err != nil {
		io.Copy(io.Discard, resp.Body)
		return SlotStats{}
	}
	io.Copy(io.Discard, resp.Body)
	var stats SlotStats
	stats.NSlots = len(slots)
	for _, s := range slots {
		if s.NCtx > stats.NCtx {
			stats.NCtx = s.NCtx
		}
		if s.IsProcessing {
			stats.NActive++
			if len(s.NextToken) > 0 {
				stats.NDecoded += s.NextToken[0].NDecoded
				stats.NRemain += s.NextToken[0].NRemain
			}
		}
	}
	return stats
}

func (b *Backend) ReadRSSMB() int64 {
	if b.cmd == nil || b.cmd.Process == nil {
		return 0
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", b.cmd.Process.Pid))
	if err != nil {
		return 0
	}
	var size, rss int64
	if _, err := fmt.Sscanf(string(data), "%d %d", &size, &rss); err != nil {
		return 0
	}
	return rss * int64(os.Getpagesize()) / (1024 * 1024)
}
