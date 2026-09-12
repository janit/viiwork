package main

import (
	"bytes"
	"strings"
	"testing"
)

func runWith(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, runEnv{
		stdout:    &stdout,
		stderr:    &stderr,
		lookupEnv: func(string) (string, bool) { return "", false },
		hostname:  func() (string, error) { return "testhost", nil },
	})
	return code, stdout.String(), stderr.String()
}

func TestRun(t *testing.T) {
	version = "v2-test"
	if code, out, _ := runWith("--version"); code != 0 || strings.TrimSpace(out) != "v2-test" {
		t.Errorf("E1: exit %d, stdout %q", code, out)
	}
	if code, _, errOut := runWith("--config", "testdata/v1.yaml"); code != 1 || !strings.Contains(errOut, "docs/migrating-to-v2.md") {
		t.Errorf("E2: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := runWith("--gpus.count", "4"); code != 2 || !strings.Contains(errOut, "-config") {
		t.Errorf("E3: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := runWith("alias", "frobnicate"); code != 2 || !strings.Contains(errOut, "usage: viiwork alias") {
		t.Errorf("E4: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := runWith("--config", "missing.yaml"); code != 1 || !strings.Contains(errOut, "reading config") {
		t.Errorf("E5: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := runWith("serve"); code != 2 || !strings.Contains(errOut, "serve") {
		t.Errorf("a stray argument: exit %d, stderr %q", code, errOut)
	}
}
