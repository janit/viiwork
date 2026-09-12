package accept

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
)

const (
	apiPort    = 8086
	gossipPort = 7946

	// largeWeights and longStartup are Decision 4: a split or large model
	// loading from slow storage needs far longer than the engine default.
	largeWeights = 20 << 30
	longStartup  = 45 * time.Minute

	containerModels = "/models/"
)

// ModelSummary is one models[] entry as the node will run it.
type ModelSummary struct {
	Name           string        `json:"name"`
	Engine         string        `json:"engine"`
	GPUs           []int         `json:"gpus"`
	Backends       int           `json:"backends"`
	Slots          int           `json:"slots"`
	CtxPerSlot     int           `json:"ctx_per_slot"`
	StartupTimeout time.Duration `json:"startup_timeout_ns"`
	WeightsBytes   int64         `json:"weights_bytes,omitempty"`
}

// ConfigSummary is what a v2 file configures, for a reader checking it
// before the machine's v1 instances stop.
type ConfigSummary struct {
	Node       string         `json:"node"`
	StateDir   string         `json:"state_dir"`
	Network    string         `json:"network"`
	Mesh       string         `json:"mesh"`
	APIPort    int            `json:"api_port"`
	GossipPort int            `json:"gossip_port"`
	Models     []ModelSummary `json:"models"`
}

// SummarizeConfig validates the file at path exactly as the node would, then
// checks what validation cannot: the fixed ports, that every model's weights
// exist, and that large or split models allow a long enough load. modelsRoot,
// when set, replaces the /models/ prefix of container paths so the weights
// can be found from the host. requireMesh, when set, is the mesh mode the file
// must have (secured or open).
func SummarizeConfig(path string, lookupEnv func(string) (string, bool), modelsRoot, requireMesh string) (ConfigSummary, Report) {
	r := Report{Command: "config", Target: path, Started: time.Now()}
	cfg, err := config.Load(path, lookupEnv)
	if err != nil {
		r.Checks = []Check{{Name: "config valid", Detail: err.Error()}}
		return ConfigSummary{}, r
	}
	r.Checks = append(r.Checks, Check{Name: "config valid", Pass: true})

	mode := "secured"
	if cfg.Mesh.Open {
		mode = "open"
	}
	meshCheck := Check{Name: "mesh " + mode, Pass: requireMesh == "" || requireMesh == mode}
	if !meshCheck.Pass {
		meshCheck.Detail = "required: " + requireMesh
	}
	r.Checks = append(r.Checks, meshCheck,
		portCheck("api port", apiPort, "api.port", cfg.API.Port),
		portCheck("gossip port", gossipPort, "mesh.bind_port", cfg.Mesh.BindPort))

	sum := ConfigSummary{
		Node:       cfg.Node.Name,
		StateDir:   cfg.Node.StateDir,
		Network:    cfg.Mesh.Network,
		Mesh:       mode,
		APIPort:    cfg.API.Port,
		GossipPort: cfg.Mesh.BindPort,
	}
	for _, m := range cfg.Models {
		ms := ModelSummary{
			Name:           m.Name,
			Engine:         m.Engine,
			GPUs:           append([]int{}, m.GPUs...),
			Backends:       m.Backends(),
			CtxPerSlot:     m.Context,
			StartupTimeout: m.StartupTimeout.Duration,
		}
		ms.Slots = ms.Backends * m.Parallel

		weightsPath := hostPath(m.Path, modelsRoot)
		size, err := weightsSize(weightsPath)
		weights := Check{Name: "weights " + m.Name}
		if err != nil {
			weights.Detail = err.Error()
		} else {
			weights.Pass = true
			weights.Detail = fmt.Sprintf("%s %s", weightsPath, formatBytes(size))
			ms.WeightsBytes = size
		}
		r.Checks = append(r.Checks, weights, startupCheck(m, size, err == nil))
		sum.Models = append(sum.Models, ms)
	}
	return sum, r
}

func portCheck(name string, want int, key string, got int) Check {
	c := Check{Name: fmt.Sprintf("%s %d", name, want), Pass: got == want}
	if !c.Pass {
		c.Detail = fmt.Sprintf("%s is %d; every machine uses %d", key, got, want)
	}
	return c
}

func startupCheck(m config.Model, size int64, sized bool) Check {
	c := Check{Name: "startup_timeout " + m.Name, Pass: true}
	have := "engine default"
	if m.StartupTimeout.Duration > 0 {
		have = m.StartupTimeout.Duration.String()
	}
	var reasons []string
	if m.GPUsPerBackend > 1 {
		reasons = append(reasons, fmt.Sprintf("gpus_per_backend %d", m.GPUsPerBackend))
	}
	if sized && size > largeWeights {
		reasons = append(reasons, "weights "+formatBytes(size))
	}
	if len(reasons) > 0 && m.StartupTimeout.Duration < longStartup {
		c.Pass = false
		c.Detail = fmt.Sprintf("%s: need at least %s, have %s", strings.Join(reasons, ", "), longStartup, have)
		return c
	}
	c.Detail = have
	return c
}

// hostPath rewrites a container path under /models/ to modelsRoot.
func hostPath(p, modelsRoot string) string {
	if modelsRoot == "" || !strings.HasPrefix(p, containerModels) {
		return p
	}
	return filepath.Join(modelsRoot, strings.TrimPrefix(p, containerModels))
}

// splitGGUF matches the first shard of a split GGUF, whose loader opens every
// shard: name-00001-of-00003.gguf.
var splitGGUF = regexp.MustCompile(`^(.*)-00001-of-(\d{5})\.gguf$`)

// weightsSize is the size of everything a model loads from path: the file, all
// shards of a split GGUF, or every file under a model directory.
func weightsSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if info.IsDir() {
		var total int64
		err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type().IsRegular() {
				fi, err := d.Info()
				if err != nil {
					return err
				}
				total += fi.Size()
			}
			return nil
		})
		return total, err
	}
	m := splitGGUF.FindStringSubmatch(path)
	if m == nil {
		return info.Size(), nil
	}
	n, _ := strconv.Atoi(m[2])
	total := info.Size()
	for i := 2; i <= n; i++ {
		shard := fmt.Sprintf("%s-%05d-of-%s.gguf", m[1], i, m[2])
		fi, err := os.Stat(shard)
		if err != nil {
			return 0, fmt.Errorf("shard %d of %d: %w", i, n, err)
		}
		total += fi.Size()
	}
	return total, nil
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
