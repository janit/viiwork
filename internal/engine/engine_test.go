package engine

import (
	"context"
	"slices"
	"testing"
	"time"
)

type fakeEngine struct{ name string }

func (f fakeEngine) Name() string { return f.name }

func (f fakeEngine) Command(s Spec) (Command, error) {
	return Command{Path: "/bin/true", Args: []string{"--alias", s.Name}}, nil
}

func (f fakeEngine) Probe(context.Context, string) (Probe, error) {
	return Probe{Ready: true, Progress: -1}, nil
}

func (f fakeEngine) Load(context.Context, Spec, string) (Load, error) {
	return Load{Slots: 2, Busy: 1}, nil
}

func (f fakeEngine) DefaultStartupTimeout() time.Duration { return time.Minute }

var _ Engine = fakeEngine{}

// registerOnce registers a test engine unless an earlier run of the same test
// binary (go test -count=N) already did: the registry is process-global.
func registerOnce(name string) {
	if _, ok := Lookup(name); !ok {
		Register(fakeEngine{name: name})
	}
}

func TestRegisterAndLookup(t *testing.T) {
	registerOnce("fake-a")
	e, ok := Lookup("fake-a")
	if !ok || e.Name() != "fake-a" {
		t.Fatalf("Lookup(fake-a) = %v, %v", e, ok)
	}
	if _, ok := Lookup("missing"); ok {
		t.Error("Lookup(missing) must report false")
	}
	if !slices.Contains(Names(), "fake-a") {
		t.Errorf("Names() = %v, want fake-a listed", Names())
	}
}

func TestRegisterPanicsOnDuplicateAndEmpty(t *testing.T) {
	registerOnce("fake-b")
	for _, name := range []string{"fake-b", ""} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Register(%q) must panic", name)
				}
			}()
			Register(fakeEngine{name: name})
		}()
	}
}
