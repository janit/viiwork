package llamacpp

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// autoThreads is the --threads a backend gets when the operator set none:
// a fair share of the CPUs across the model's backends, at least 1.
//
// Without it every llama-server defaults to half the cores. v1 field report:
// 10 backends on an 8-thread EPYC 3151 ran ~90 threads, and the backends
// thrashed and crashed under long-prompt load.
func autoThreads(nproc, backends int) int {
	if backends < 1 {
		backends = 1
	}
	if n := nproc / backends; n > 1 {
		return n
	}
	return 1
}

// needsNoMmap reports whether a model is at least 80% of host RAM, the point
// at which mmap-loading it thrashes the page cache. v1 measured a 100 GB model
// on a 46 GB host spending hours in folio_wait_bit_common over NFS; with
// --no-mmap each tensor is read once into a buffer and freed after upload.
// Unknown RAM (0) never adds the flag.
func needsNoMmap(modelBytes, totalRAMBytes int64) bool {
	if totalRAMBytes <= 0 || modelBytes <= 0 {
		return false
	}
	// modelBytes >= 0.8 * RAM, in integers so exactly 80% is not lost to
	// float rounding.
	return modelBytes*5 >= totalRAMBytes*4
}

// multiPartGGUFRe matches the llama.cpp multi-part naming convention,
// <prefix>-NNNNN-of-MMMMM.gguf. Groups: 1=prefix, 2=part, 3=total parts.
var multiPartGGUFRe = regexp.MustCompile(`^(.+)-(\d{5})-of-(\d{5})\.gguf$`)

// modelTotalSize is the on-disk size of a model: one file, or the sum of every
// part of a multi-part GGUF, because llama.cpp loads all parts when given part
// 1 and a 100 GB model split into 37 GB parts must still trip the no-mmap
// check. A glob that finds nothing falls back to the single file's size; a
// missing file is an error.
func modelTotalSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	m := multiPartGGUFRe.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return st.Size(), nil
	}
	pattern := filepath.Join(filepath.Dir(path), fmt.Sprintf("%s-?????-of-%s.gguf", m[1], m[3]))
	parts, err := filepath.Glob(pattern)
	if err != nil || len(parts) == 0 {
		return st.Size(), nil
	}
	var total int64
	for _, p := range parts {
		if ps, err := os.Stat(p); err == nil {
			total += ps.Size()
		}
	}
	if total == 0 {
		return st.Size(), nil
	}
	return total, nil
}

// readTotalRAMBytes returns MemTotal from /proc/meminfo, or 0 if unreadable.
func readTotalRAMBytes() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		var kb int64
		if _, err := fmt.Sscanf(line, "MemTotal: %d kB", &kb); err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}
