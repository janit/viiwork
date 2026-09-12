// Package accept checks a viiwork 2 node from the outside: its config before
// it starts, the ports the mesh needs, how long membership takes, and whether
// inference, routing and aliases behave. Every check only reads node state or
// sends inference requests; nothing here starts, stops or configures a node.
package accept

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Check is one named pass/fail result.
type Check struct {
	Name    string        `json:"name"`
	Pass    bool          `json:"pass"`
	Detail  string        `json:"detail,omitempty"`
	Elapsed time.Duration `json:"elapsed_ns,omitempty"`
}

// Report is the result of one command against one target.
type Report struct {
	Command string    `json:"command"`
	Target  string    `json:"target"`
	Started time.Time `json:"started"`
	Checks  []Check   `json:"checks"`
}

// Pass is true when the report has at least one check and all of them pass.
func (r Report) Pass() bool {
	if len(r.Checks) == 0 {
		return false
	}
	for _, c := range r.Checks {
		if !c.Pass {
			return false
		}
	}
	return true
}

// WriteText writes one line per check and a closing tally.
func (r Report) WriteText(w io.Writer) error {
	var b strings.Builder
	passed := 0
	for _, c := range r.Checks {
		parts := []string{"FAIL", c.Name}
		if c.Pass {
			parts[0] = "PASS"
			passed++
		}
		if c.Elapsed != 0 {
			parts = append(parts, c.Elapsed.Round(100*time.Millisecond).String())
		}
		if c.Detail != "" {
			parts = append(parts, c.Detail)
		}
		b.WriteString(strings.Join(parts, "  "))
		b.WriteByte('\n')
	}
	title := r.Command
	if r.Target != "" {
		title += " " + r.Target
	}
	fmt.Fprintf(&b, "%s: %d/%d passed\n", title, passed, len(r.Checks))
	_, err := io.WriteString(w, b.String())
	return err
}

// WriteJSON writes the report indented by two spaces.
func (r Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Env is what the checks use to reach nodes and to tell time, so tests can
// replace both.
type Env struct {
	HTTP    *http.Client
	Now     func() time.Time
	Sleep   func(ctx context.Context, d time.Duration) error
	Headers http.Header // added to every request; nil = none
}

// DefaultEnv talks HTTP directly (never through a proxy from the environment,
// which must not carry tailnet traffic) with no overall client timeout: each
// check bounds its own requests.
func DefaultEnv() Env {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return Env{
		HTTP:  &http.Client{Transport: tr},
		Now:   time.Now,
		Sleep: sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
