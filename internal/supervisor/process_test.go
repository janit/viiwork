package supervisor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startHelper starts the test binary in mode and stops it at cleanup.
func startHelper(t *testing.T, mode string, out *syncBuffer, extra ...string) *Process {
	t.Helper()
	p, err := StartProcess(os.Args[0], nil, helperEnv(mode, extra...), out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Stop(time.Second) })
	return p
}

func TestProcessExit(t *testing.T) {
	p := startHelper(t, "exit3", &syncBuffer{})
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("exit3 did not exit")
	}
	if p.Running() || p.ExitErr() == nil {
		t.Errorf("running=%v exitErr=%v, want false and an error", p.Running(), p.ExitErr())
	}
}

func TestProcessEnvAndOutput(t *testing.T) {
	out := &syncBuffer{}
	p := startHelper(t, "echo-env", out, "HELPER_VAR=pinned")
	<-p.Done()
	if !strings.Contains(out.String(), "HELPER_VAR=pinned") {
		t.Errorf("output = %q, want HELPER_VAR=pinned", out.String())
	}
}

func TestProcessGracefulStop(t *testing.T) {
	out := &syncBuffer{}
	p := startHelper(t, "sleep", out)
	waitFor(t, "ready", func() bool { return strings.Contains(out.String(), "ready") })
	start := time.Now()
	p.Stop(5 * time.Second)
	if took := time.Since(start); took >= 3*time.Second {
		t.Errorf("graceful stop took %v, want under 3 s", took)
	}
	if p.Running() {
		t.Error("still running after Stop")
	}
}

func TestProcessStopEscalates(t *testing.T) {
	out := &syncBuffer{}
	p := startHelper(t, "ignore-term", out)
	waitFor(t, "ready", func() bool { return strings.Contains(out.String(), "ready") })
	start := time.Now()
	p.Stop(300 * time.Millisecond)
	took := time.Since(start)
	if took < 300*time.Millisecond || took >= 5*time.Second {
		t.Errorf("escalating stop took %v, want 300 ms to 5 s", took)
	}
	if p.Running() {
		t.Error("still running after Stop")
	}
}

func TestProcessStopKillsGroup(t *testing.T) {
	out := &syncBuffer{}
	p := startHelper(t, "spawn-child", out)
	childRE := regexp.MustCompile(`child (\d+)`)
	var child int
	waitFor(t, "child pid", func() bool {
		m := childRE.FindStringSubmatch(out.String())
		if m == nil {
			return false
		}
		child, _ = strconv.Atoi(m[1])
		return true
	})
	waitFor(t, "child in TreePIDs", func() bool { return slices.Contains(p.TreePIDs(), child) })
	p.Stop(300 * time.Millisecond)
	waitFor(t, "child gone", func() bool { return processGone(child) })
}

func init() {
	// orphan-worker starts a SIGTERM-ignoring child that shares its output,
	// then exits: a crashed leader whose worker survives.
	helperModes["orphan-worker"] = func() {
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "SUPERVISOR_HELPER=ignore-term")
		child.Stdout = os.Stdout
		if err := child.Start(); err != nil {
			os.Exit(1)
		}
		fmt.Printf("child %d\n", child.Process.Pid)
		os.Exit(1)
	}
}

// A leader that dies while a worker still holds its output pipe must still
// close Done (os/exec would otherwise wait on the pipe forever), and the
// orphaned worker must not be left holding GPUs.
func TestProcessLeaderExitKillsOrphanedWorker(t *testing.T) {
	out := &syncBuffer{}
	p := startHelper(t, "orphan-worker", out)
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done not closed while an orphaned worker holds the output pipe")
	}
	m := regexp.MustCompile(`child (\d+)`).FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("no child pid in output %q", out.String())
	}
	child, _ := strconv.Atoi(m[1])
	waitFor(t, "orphaned worker gone", func() bool { return processGone(child) })
}

func TestProcessRSS(t *testing.T) {
	out := &syncBuffer{}
	p := startHelper(t, "sleep", out)
	waitFor(t, "ready", func() bool { return strings.Contains(out.String(), "ready") })
	if rss := p.RSSMB(); rss <= 0 {
		t.Errorf("RSSMB = %d, want > 0", rss)
	}
}

func TestDescendants(t *testing.T) {
	root := t.TempDir()
	stat := map[string]string{
		"100": "100 (llama server) S 1 100 100 0 -1",
		"101": "101 (worker) S 100 100 100 0 -1",
		"102": "102 (worker (sub)) S 101 100 100 0 -1",
		"200": "200 (unrelated) S 1 200 200 0 -1",
	}
	for pid, line := range stat {
		if err := os.MkdirAll(filepath.Join(root, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, pid, "stat"), []byte(line+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := descendants(root, 100); !slices.Equal(got, []int{100, 101, 102}) {
		t.Errorf("descendants = %v, want [100 101 102]", got)
	}
}

func TestStartProcessBadPath(t *testing.T) {
	_, err := StartProcess("/nonexistent/binary", nil, nil, &syncBuffer{})
	if err == nil || !strings.Contains(err.Error(), "/nonexistent/binary") {
		t.Errorf("err = %v, want it to name the path", err)
	}
}
