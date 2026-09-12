package gpu

import (
	"context"
	"os/exec"
	"time"
)

// Runner runs an SMI command and returns its standard output. It is a type
// rather than a direct exec call so tests can fake nvidia-smi and rocm-smi
// without the tools or a GPU.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// smiTimeout bounds one SMI invocation. A wedged driver can hang rocm-smi or
// nvidia-smi indefinitely, and the callers are health ticks and backend
// launches that must not stall behind it.
const smiTimeout = 3 * time.Second

// ExecRunner runs the command with a 3 s timeout and returns stdout only.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, smiTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}
