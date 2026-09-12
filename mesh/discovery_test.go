package mesh

import (
	"context"
	"slices"
	"testing"
)

func TestSeedFeeder(t *testing.T) {
	f := SeedFeeder([]string{"100.64.0.2:7946", "100.64.0.3:7946"})
	if f.Name() != "seeds" {
		t.Errorf("Name = %q", f.Name())
	}
	first, err := f.Candidates(context.Background())
	if err != nil || !slices.Equal(first, []string{"100.64.0.2:7946", "100.64.0.3:7946"}) {
		t.Fatalf("Candidates = %v, %v", first, err)
	}
	first[0] = "mutated"
	second, _ := f.Candidates(context.Background())
	if second[0] != "100.64.0.2:7946" {
		t.Error("changing a returned slice must not change the feeder")
	}
}

func TestMergeCandidates(t *testing.T) {
	got := mergeCandidates([][]string{{"b", "a"}, {"a", " c ", ""}}, map[string]bool{"c": true})
	if !slices.Equal(got, []string{"b", "a"}) {
		t.Errorf("mergeCandidates = %v, want [b a]", got)
	}
}
