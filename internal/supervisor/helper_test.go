package supervisor

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as every child process these tests supervise: with
// SUPERVISOR_HELPER set, TestMain runs that mode instead of the tests.

// helperModes maps a SUPERVISOR_HELPER value to what the child does. Other test
// files add modes from init(), which runs before TestMain.
var helperModes = map[string]func(){
	"exit3": func() { os.Exit(3) },
	"sleep": func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM)
		fmt.Println("ready")
		select {
		case <-sig:
		case <-time.After(time.Minute):
		}
	},
	"ignore-term": func() {
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("ready")
		time.Sleep(time.Minute)
	},
	"spawn-child": func() {
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "SUPERVISOR_HELPER=ignore-term")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			fmt.Println("spawn failed:", err)
			os.Exit(1)
		}
		fmt.Printf("child %d\n", child.Process.Pid)
		time.Sleep(time.Minute)
	},
	"echo-env": func() {
		fmt.Printf("HELPER_VAR=%s\n", os.Getenv("HELPER_VAR"))
	},
}

func TestMain(m *testing.M) {
	if mode := os.Getenv("SUPERVISOR_HELPER"); mode != "" {
		run, ok := helperModes[mode]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown SUPERVISOR_HELPER mode %q\n", mode)
			os.Exit(2)
		}
		run()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// helperEnv is the environment that starts the test binary in mode.
func helperEnv(mode string, extra ...string) []string {
	env := append(os.Environ(), "SUPERVISOR_HELPER="+mode)
	return append(env, extra...)
}

// syncBuffer is an io.Writer safe for a child's output goroutine and a test
// reading it at the same time.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor polls cond every 20 ms and fails the test after 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// processGone is true when pid no longer exists or is a zombie. A zombie
// counts as gone: inside the gorun container PID 1 is the go command, which
// never reaps orphaned grandchildren.
func processGone(pid int) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return true
	}
	s := string(data)
	close := strings.LastIndexByte(s, ')')
	if close < 0 {
		return true
	}
	fields := strings.Fields(s[close+1:])
	return len(fields) > 0 && fields[0] == "Z"
}
