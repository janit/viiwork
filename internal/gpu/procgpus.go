package gpu

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Verdict is the on-GPU check's judgement of one backend.
//
// Unknown is not a softer Wrong, and must not be "fixed" into one. The SMI
// tools report host PIDs, so a node in a container with its own PID namespace
// sees none of its children in their output; a missing tool or an empty report
// looks the same. Killing on unknown would respawn a correctly pinned backend
// in a loop. The check therefore answers unknown only when none of the node's
// backend PIDs appear in the output. When some do, the namespaces match, and a
// backend absent from the output really holds no GPU, which is Wrong. The known
// limit: a node with a single GPU backend cannot tell "on no card" from "private
// PID namespace", so it reports unknown.
type Verdict int

const (
	VerdictUnknown Verdict = iota
	VerdictOK
	VerdictWrong
)

func (v Verdict) String() string {
	switch v {
	case VerdictOK:
		return "ok"
	case VerdictWrong:
		return "wrong"
	default:
		return "unknown"
	}
}

var (
	nvidiaQueryGPUArgs    = []string{"--query-gpu=index,uuid", "--format=csv,noheader"}
	nvidiaComputeAppsArgs = []string{"--query-compute-apps=pid,gpu_uuid", "--format=csv,noheader"}
	rocmShowPIDGPUsArgs   = []string{"--showpidgpus"}
)

// ProcessGPUs maps host PIDs to the GPU indices they hold, each list
// ascending. It returns nil, nil for VendorNone.
//
// On NVIDIA, compute apps are reported by card UUID, one row per card per
// process (a split backend appears once per card), and mapped to indices
// through --query-gpu. On AMD it reads `rocm-smi --showpidgpus`, which has no
// JSON form in ROCm SMI 3.0; `--showpids` is not usable because its GPU(s)
// column is a count, not indices.
func ProcessGPUs(ctx context.Context, vendor Vendor, run Runner) (map[int][]int, error) {
	switch vendor {
	case VendorNVIDIA:
		return nvidiaProcessGPUs(ctx, run)
	case VendorAMD:
		return rocmProcessGPUs(ctx, run)
	}
	return nil, nil
}

func runSMI(ctx context.Context, run Runner, name string, args []string) ([]byte, error) {
	out, err := run(ctx, name, args...)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

func nvidiaProcessGPUs(ctx context.Context, run Runner) (map[int][]int, error) {
	gpus, err := runSMI(ctx, run, "nvidia-smi", nvidiaQueryGPUArgs)
	if err != nil {
		return nil, err
	}
	index := map[string]int{}
	for _, row := range csvRows(gpus) {
		if len(row) < 2 {
			continue
		}
		i, err := strconv.Atoi(row[0])
		if err != nil {
			continue
		}
		index[row[1]] = i
	}

	apps, err := runSMI(ctx, run, "nvidia-smi", nvidiaComputeAppsArgs)
	if err != nil {
		return nil, err
	}
	held := map[int][]int{}
	for _, row := range csvRows(apps) {
		if len(row) < 2 {
			continue
		}
		pid, err := strconv.Atoi(row[0])
		if err != nil {
			continue
		}
		i, ok := index[row[1]]
		if !ok {
			continue
		}
		held[pid] = append(held[pid], i)
	}
	return normalize(held), nil
}

func rocmProcessGPUs(ctx context.Context, run Runner) (map[int][]int, error) {
	out, err := runSMI(ctx, run, "rocm-smi", rocmShowPIDGPUsArgs)
	if err != nil {
		return nil, err
	}
	held := map[int][]int{}
	pid, open := 0, false
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "PID ") {
			// "PID N is using K DRM device(s):" opens a block; the next
			// non-empty line holds N's indices.
			fields := strings.Fields(line)
			p, err := strconv.Atoi(fields[1])
			pid, open = p, err == nil
			continue
		}
		if !open {
			continue // banner lines
		}
		open = false
		indices, ok := allInts(strings.Fields(line))
		if !ok {
			continue // the ===== footer: the block had no index line
		}
		held[pid] = append(held[pid], indices...)
	}
	return normalize(held), nil
}

// csvRows splits nvidia-smi's "csv,noheader" output into trimmed fields.
func csvRows(out []byte) [][]string {
	var rows [][]string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		rows = append(rows, fields)
	}
	return rows
}

func allInts(fields []string) ([]int, bool) {
	if len(fields) == 0 {
		return nil, false
	}
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// normalize sorts and de-duplicates every PID's GPU list.
func normalize(held map[int][]int) map[int][]int {
	for pid, list := range held {
		slices.Sort(list)
		held[pid] = slices.Compact(list)
	}
	return held
}

// CheckAssignment judges one backend. nodePIDs is every backend process and
// descendant on the node, backendPIDs this backend's process tree, and want
// its configured GPUs. See Verdict for why unknown is reserved for "none of
// the node's PIDs appear". GPUs are compared as sets: holding an extra card is
// wrong, and so is holding none.
func CheckAssignment(pidGPUs map[int][]int, nodePIDs, backendPIDs, want []int) Verdict {
	if len(pidGPUs) == 0 {
		return VerdictUnknown
	}
	visible := false
	for _, pid := range nodePIDs {
		if _, ok := pidGPUs[pid]; ok {
			visible = true
			break
		}
	}
	if !visible {
		return VerdictUnknown
	}

	var holds []int
	for _, pid := range backendPIDs {
		holds = append(holds, pidGPUs[pid]...)
	}
	if slices.Equal(sortedSet(holds), sortedSet(want)) {
		return VerdictOK
	}
	return VerdictWrong
}

func sortedSet(ids []int) []int {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}
