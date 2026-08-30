package energy

import (
	"fmt"
	"os"
	"strings"
)

// sourceFile names the sibling file that records where a store's node wattage
// came from. It is a sibling rather than a ring header field on purpose: the
// label is per-store, not per-series, it has no length bound worth picking
// (a sensor name is operator-supplied), and keeping it out of the rings means
// the VIIWENG1 geometry stays byte-identical to what v1.5.x wrote.
const sourceFile = "source"

// maxSourceLen bounds what is read back, so a directory holding something
// other than a viiwork store cannot turn into an unbounded string.
const maxSourceLen = 256

// readSource returns the recorded provenance label, or "" if none is recorded.
// A missing file is not an error: stores written before v1.6.0 have none, and
// "" means "unknown", never "measured nothing" — the same rule the mesh wire
// contract applies to absent fields.
func readSource(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	if len(b) > maxSourceLen {
		b = b[:maxSourceLen]
	}
	return strings.TrimSpace(string(b)), nil
}

func writeSource(path, src string) error {
	if err := os.WriteFile(path, []byte(src+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
