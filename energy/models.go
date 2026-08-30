package energy

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
)

// maxModels is bounded by ModelIdx being a uint16. A node serves a handful of
// models, so the ceiling exists only to make the failure explicit.
const maxModels = 65535

// modelTable maps model names to the small integers stamped into GPURecord.
// It is append-only: an index, once handed out, keeps its meaning for as long
// as the rings that reference it, so a name is never reused for another model.
type modelTable struct {
	mu    sync.RWMutex
	path  string
	names []string
	index map[string]uint16
}

func openModelTable(path string) (*modelTable, error) {
	t := &modelTable{path: path, index: make(map[string]uint16)}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return t, nil
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		name := strings.TrimRight(scanner.Text(), "\r")
		t.index[name] = uint16(len(t.names))
		t.names = append(t.names, name)
	}
	return t, scanner.Err()
}

// Index returns the index for a model name, appending it if new. The empty
// name is index 0 and means "no model was resident", which is a real state:
// a GPU can be powered and idle between deployments.
func (t *modelTable) Index(name string) (uint16, error) {
	if name == "" {
		return 0, nil
	}

	t.mu.RLock()
	if idx, ok := t.index[name]; ok {
		t.mu.RUnlock()
		return idx, nil
	}
	t.mu.RUnlock()

	t.mu.Lock()
	defer t.mu.Unlock()
	if idx, ok := t.index[name]; ok {
		return idx, nil
	}
	if len(t.names) == 0 {
		// Reserve 0 for "no model" so a zeroed record does not read as a real one.
		t.names = append(t.names, "")
		t.index[""] = 0
		if err := t.append(""); err != nil {
			return 0, err
		}
	}
	if len(t.names) >= maxModels {
		return 0, fmt.Errorf("model table full (%d entries)", maxModels)
	}

	idx := uint16(len(t.names))
	t.names = append(t.names, name)
	t.index[name] = idx
	return idx, t.append(name)
}

// Name resolves an index back to a model name. An index from a ring written by
// a build with a longer table reads as unknown rather than as another model.
func (t *modelTable) Name(idx uint16) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if int(idx) >= len(t.names) {
		return ""
	}
	return t.names[idx]
}

func (t *modelTable) append(name string) error {
	f, err := os.OpenFile(t.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, name); err != nil {
		return err
	}
	return f.Sync()
}
