package logging

import (
	"bytes"
	"testing"
)

func TestPrefixWriter(t *testing.T) {
	var buf bytes.Buffer
	pw := NewPrefixWriter(&buf, "[gpu-0] ")
	pw.Write([]byte("model loaded\n"))
	pw.Write([]byte("ready\n"))
	output := buf.String()
	if output != "[gpu-0] model loaded\n[gpu-0] ready\n" {
		t.Errorf("unexpected output: %q", output)
	}
}

func TestPrefixWriterPartialLines(t *testing.T) {
	var buf bytes.Buffer
	pw := NewPrefixWriter(&buf, "[gpu-1] ")
	pw.Write([]byte("partial"))
	pw.Write([]byte(" line\n"))
	output := buf.String()
	if output != "[gpu-1] partial line\n" {
		t.Errorf("unexpected output: %q", output)
	}
}
