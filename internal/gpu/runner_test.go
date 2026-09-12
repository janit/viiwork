package gpu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner answers a command whose name and args joined by single spaces
// equal a key, and fails any other command the way a missing binary would.
func fakeRunner(outputs map[string]string) Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		out, ok := outputs[key]
		if !ok {
			return nil, fmt.Errorf("exec: %q: executable file not found in $PATH", key)
		}
		return []byte(out), nil
	}
}

// failingRunner fails the test if any command is run.
func failingRunner(t *testing.T) Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		t.Errorf("unexpected command: %s %s", name, strings.Join(args, " "))
		return nil, fmt.Errorf("unexpected command")
	}
}

// mustFixture returns the content of testdata/<name>.
func mustFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(data)
}
