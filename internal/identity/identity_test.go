package identity

import (
	"strings"
	"testing"
)

func TestGenerateNodeID(t *testing.T) {
	id := GenerateNodeID()
	if !strings.HasPrefix(id, "viiwork-") {
		t.Errorf("expected viiwork- prefix, got %s", id)
	}
	if len(id) != len("viiwork-")+8 {
		t.Errorf("expected 16 char id, got %d: %s", len(id), id)
	}
}

func TestGenerateNodeIDUnique(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		id := GenerateNodeID()
		if seen[id] {
			t.Fatalf("duplicate id: %s", id)
		}
		seen[id] = true
	}
}
