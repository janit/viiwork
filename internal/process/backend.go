package process

import (
	"context"
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
	Binary        string
	ExtraArgs     []string
	HealthTimeout   time.Duration
	PowerLimitWatts int
	State           *balancer.BackendState
	LogWriter     io.Writer
	cmd           *exec.Cmd
}

func (b *Backend) buildArgs() []string {
	args := []string{
		"--model", b.ModelPath,
		"--port", strconv.Itoa(b.Port),
		"--ctx-size", strconv.Itoa(b.ContextSize),
		"--n-gpu-layers", strconv.Itoa(b.NGPULayers),
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

func (b *Backend) CheckHealth(ctx context.Context) bool {
	url := fmt.Sprintf("http://%s/health", b.State.Addr)
	ctx, cancel := context.WithTimeout(ctx, b.HealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil { return false }
	resp, err := healthClient.Do(req)
	if err != nil { return false }
	defer resp.Body.Close()
	return resp.StatusCode == 200
}
