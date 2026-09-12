package mesh

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestLogFilter(t *testing.T) {
	var out bytes.Buffer
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	f := newLogFilter(&out, false, clock)
	line := "[DEBUG] memberlist: a\n"
	if n, _ := f.Write([]byte(line)); n != len(line) {
		t.Errorf("Write returned %d, want %d even for a dropped line", n, len(line))
	}
	if out.Len() != 0 {
		t.Errorf("debug line written without debug: %q", out.String())
	}
	dbg := newLogFilter(&out, true, clock)
	dbg.Write([]byte(line))
	if !strings.Contains(out.String(), "[DEBUG] memberlist: a") {
		t.Error("debug line dropped with debug on")
	}

	out.Reset()
	f.Write([]byte("[WARN] memberlist: b\n"))
	now = now.Add(59 * time.Second)
	f.Write([]byte("[WARN] memberlist: b\n"))
	if c := strings.Count(out.String(), "memberlist: b"); c != 1 {
		t.Errorf("a repeat within 59 s was written %d times, want 1", c)
	}
	now = now.Add(2 * time.Second) // 61 s after the first
	f.Write([]byte("[WARN] memberlist: b\n"))
	if c := strings.Count(out.String(), "memberlist: b"); c != 2 {
		t.Errorf("a repeat after 61 s: %d lines, want 2", c)
	}

	out.Reset()
	f.Write([]byte("[INFO] memberlist: split "))
	if out.Len() != 0 {
		t.Error("a partial line must wait for its newline")
	}
	f.Write([]byte("across writes\n"))
	if c := strings.Count(out.String(), "memberlist: split across writes"); c != 1 {
		t.Errorf("split line appeared %d times, want once whole: %q", c, out.String())
	}
	if !strings.Contains(out.String(), "[mesh] ") {
		t.Errorf("output line %q lacks the [mesh] prefix", out.String())
	}
}
