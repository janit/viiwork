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
	GPUID         int
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
	args = append(args, b.ExtraArgs...)
	return args
}

func (b *Backend) buildEnv() []string {
	env := os.Environ()
	env = append(env, fmt.Sprintf("ROCR_VISIBLE_DEVICES=%d", b.GPUID))
	return env
}

func (b *Backend) Start() error {
	b.State.SetStatus(balancer.StatusStarting)
	if b.PowerLimitWatts > 0 {
		out, err := exec.Command("rocm-smi", "--setpoweroverdrive", strconv.Itoa(b.PowerLimitWatts), "-d", strconv.Itoa(b.GPUID)).CombinedOutput()
		if err != nil {
			log.Printf("[gpu-%d] WARNING: failed to set power limit to %dW: %v (%s)", b.GPUID, b.PowerLimitWatts, err, string(out))
		} else {
			log.Printf("[gpu-%d] power limit set to %dW", b.GPUID, b.PowerLimitWatts)
		}
	}
	b.cmd = exec.Command(b.Binary, b.buildArgs()...)
	b.cmd.Env = b.buildEnv()
	if b.LogWriter != nil {
		b.cmd.Stdout = b.LogWriter
		b.cmd.Stderr = b.LogWriter
	}
	if err := b.cmd.Start(); err != nil {
		return fmt.Errorf("starting backend gpu-%d: %w", b.GPUID, err)
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
