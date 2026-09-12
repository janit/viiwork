// Package supervisor runs a node's models as backend processes for any
// registered engine: one supervision loop per backend over a pure health
// ladder, a node-wide load gate, the on-GPU check, and a node supervisor that
// applies a model list by diff and publishes meshapi snapshots.
package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// outputWaitDelay bounds how long Done waits for the child's output after the
// leader exits. os/exec otherwise waits until every process holding the
// output pipe is gone, and an orphaned worker would keep Done open forever,
// hiding a crashed leader from the supervisor.
const outputWaitDelay = time.Second

// Process is one supervised child process and its process group.
type Process struct {
	cmd     *exec.Cmd
	pid     int
	started time.Time
	done    chan struct{}
	exitErr error // written once, before done is closed
}

// StartProcess starts path with exactly env (nothing is inherited: the caller
// builds the environment, GPU pinning included) in its own process group,
// with stdout and stderr both going to out.
//
// A process group, because vLLM and FreeToken spawn worker processes that
// hold GPU memory, and stopping only the leader would leave them running.
// There is deliberately no Pdeathsig: in Go it fires when the forking OS
// thread exits, not the process, and systemd and docker already kill the whole
// cgroup when the node dies.
func StartProcess(path string, args, env []string, out io.Writer) (*Process, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = append([]string{}, env...) // non-nil: a nil Env would inherit os.Environ
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = outputWaitDelay
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", path, err)
	}
	p := &Process{cmd: cmd, pid: cmd.Process.Pid, started: time.Now(), done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		// Workers that outlived their leader are orphans holding GPUs: kill
		// the rest of the group now, while it still has members, so the group
		// id cannot have been reused by an unrelated process.
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		p.exitErr = err
		close(p.done)
	}()
	return p, nil
}

func (p *Process) PID() int { return p.pid }

// Done is closed once the leader has exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// ExitErr is nil until Done is closed, then the wait result.
func (p *Process) ExitErr() error {
	select {
	case <-p.done:
		return p.exitErr
	default:
		return nil
	}
}

// Running is true until Done is closed.
func (p *Process) Running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *Process) Uptime() time.Duration { return time.Since(p.started) }

// Stop sends SIGTERM to the whole group and waits for the leader to exit or
// grace to pass, then SIGKILLs the group and waits for the exit. It always
// ends with one more SIGKILL to the group, because workers that ignored
// SIGTERM can outlive the leader and would keep holding GPUs. It returns only
// after the leader has exited, and does nothing if it already has.
func (p *Process) Stop(grace time.Duration) {
	if !p.Running() {
		return
	}
	_ = syscall.Kill(-p.pid, syscall.SIGTERM)
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		_ = syscall.Kill(-p.pid, syscall.SIGKILL)
		<-p.done
	}
	_ = syscall.Kill(-p.pid, syscall.SIGKILL)
}

// TreePIDs is the leader plus every descendant, or nil once the leader has
// exited. The on-GPU check needs the tree because a worker, not the leader,
// may be the process that holds the card.
func (p *Process) TreePIDs() []int {
	if !p.Running() {
		return nil
	}
	return descendants("/proc", p.pid)
}

// RSSMB is the resident memory of the whole tree in MiB. Unreadable entries
// (a process that just exited) are skipped.
func (p *Process) RSSMB() int64 {
	pageSize := int64(os.Getpagesize())
	var pages int64
	for _, pid := range p.TreePIDs() {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "statm"))
		if err != nil {
			continue
		}
		fields := strings.Fields(string(data))
		if len(fields) < 2 {
			continue
		}
		if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
			pages += n
		}
	}
	return pages * pageSize / (1 << 20)
}

// descendants returns root and every process under it, breadth-first, found
// from parent links in <procRoot>/<pid>/stat. The command name in a stat line
// is parenthesised and may itself contain spaces and parentheses, so the parent
// PID is read from the fields after the last ')'.
func descendants(procRoot string, root int) []int {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return []int{root}
	}
	children := map[int][]int{}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		ppid, err := readPPID(filepath.Join(procRoot, e.Name(), "stat"))
		if err != nil {
			continue
		}
		children[ppid] = append(children[ppid], pid)
	}
	out := []int{root}
	for i := 0; i < len(out); i++ {
		kids := children[out[i]]
		slices.Sort(kids)
		out = append(out, kids...)
	}
	return out
}

func readPPID(statPath string) (int, error) {
	data, err := os.ReadFile(statPath)
	if err != nil {
		return 0, err
	}
	s := string(data)
	close := strings.LastIndexByte(s, ')')
	if close < 0 {
		return 0, errors.New("malformed stat")
	}
	fields := strings.Fields(s[close+1:])
	if len(fields) < 2 {
		return 0, errors.New("malformed stat")
	}
	return strconv.Atoi(fields[1])
}
