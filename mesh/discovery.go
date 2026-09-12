package mesh

import (
	"context"
	"slices"
	"strings"
)

// Feeder is one discovery source. It only produces "ip:port" addresses for
// memberlist.Join; it never dials anything itself.
type Feeder interface {
	Name() string
	Candidates(ctx context.Context) ([]string, error)
}

type seedFeeder struct{ seeds []string }

// SeedFeeder returns the operator's seeds as written. Config validation (C1),
// or the gateway's own checks, have already made them ip:port.
func SeedFeeder(seeds []string) Feeder { return seedFeeder{seeds: slices.Clone(seeds)} }

func (seedFeeder) Name() string { return "seeds" }

func (f seedFeeder) Candidates(context.Context) ([]string, error) {
	return slices.Clone(f.seeds), nil
}

// mergeCandidates unions the feeders' lists in first-seen order, trimming
// whitespace and dropping empty entries, duplicates and anything in exclude
// (the node's own address and members already alive).
func mergeCandidates(lists [][]string, exclude map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, c := range list {
			c = strings.TrimSpace(c)
			if c == "" || exclude[c] || seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}
