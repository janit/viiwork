package enginetest

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
)

// brokenEngine violates every assertion the kit makes about Command and Probe.
// It exists so that "llama.cpp passes" is evidence the kit works rather than
// evidence it checks nothing.
type brokenEngine struct{}

func (brokenEngine) Name() string { return "broken" }

func (brokenEngine) Command(s engine.Spec) (engine.Command, error) {
	// Operator args first (they must be last), no model name, and bound to
	// every interface rather than loopback.
	args := append([]string{}, s.Args...)
	return engine.Command{Path: "/bin/false", Args: append(args, "--host", "0.0.0.0")}, nil
}

// Probe reports a closed port as "not ready" rather than as the transport
// failure it is, which is the mistake that makes a dead backend look like a
// loading one.
func (brokenEngine) Probe(context.Context, string) (engine.Probe, error) {
	return engine.Probe{Ready: false}, nil
}

func (brokenEngine) Load(context.Context, engine.Spec, string) (engine.Load, error) {
	return engine.Load{Slots: -1}, nil
}

func (brokenEngine) DefaultStartupTimeout() time.Duration { return 0 }

func init() { engine.Register(brokenEngine{}) }

// The kit is the release's headline deliverable, so it gets the same treatment
// it asks of an engine: run it against something known to be wrong, in a
// subprocess, and check it fails for the stated reasons.
func TestKitCatchesViolations(t *testing.T) {
	if os.Getenv("ENGINETEST_RUN_BROKEN") == "1" {
		Run(t, brokenEngine{}, Case{Name: "broken", Spec: engine.Spec{GPUs: []int{0}}})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestKitCatchesViolations", "-test.v")
	cmd.Env = append(os.Environ(), "ENGINETEST_RUN_BROKEN=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the kit passed an engine that violates every rule:\n%s", out)
	}
	for _, want := range []string{
		"does not contain the model name",
		"does not bind 127.0.0.1",
		"must be appended LAST",
		"transport failure must be an error",
		"want positive",
		"no field may be negative",
	} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("the kit did not report %q\n%s", want, indent(out))
		}
	}
}

func indent(b []byte) string {
	return "\t" + strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", "\n\t")
}
