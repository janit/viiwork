package engine

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type opts struct {
	Binary  string  `yaml:"binary"`
	Ratio   float64 `yaml:"ratio"`
	Threads int     `yaml:"threads"`
}

// block parses one engine configuration block the way Parse captures it: a
// mapping node whose line numbers are those of the file it came from.
func block(t *testing.T, doc string) yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
		t.Fatal(err)
	}
	return *n.Content[0]
}

func TestDecodeOptionsDecodesOverDefaults(t *testing.T) {
	o := opts{Binary: "default-binary", Ratio: 0.9}
	s := Spec{Options: block(t, "ratio: 0.5\nthreads: 6\n")}
	if err := DecodeOptions(s, &o); err != nil {
		t.Fatal(err)
	}
	if o.Binary != "default-binary" {
		t.Errorf("binary = %q, want the default kept for a key the block omits", o.Binary)
	}
	if o.Ratio != 0.5 || o.Threads != 6 {
		t.Errorf("opts = %+v, want the block's values", o)
	}
}

// An engine that is given no block at all gets its defaults untouched, which is
// what lets an operator write a model with no engine block.
func TestDecodeOptionsZeroNodeKeepsDefaults(t *testing.T) {
	o := opts{Binary: "default-binary", Ratio: 0.9}
	if err := DecodeOptions(Spec{}, &o); err != nil {
		t.Fatal(err)
	}
	if o.Binary != "default-binary" || o.Ratio != 0.9 {
		t.Errorf("opts = %+v, want the defaults untouched", o)
	}
}

// The point of the re-marshal: a key the engine does not define is an error,
// not a silently ignored line. The message must name the key rather than a Go
// type the operator has never heard of.
func TestDecodeOptionsRejectsUnknownKey(t *testing.T) {
	s := Spec{Options: block(t, "binary: x\nratioo: 0.5\n")}
	err := DecodeOptions(s, &opts{})
	if err == nil {
		t.Fatal("an unknown key must be an error")
	}
	if !strings.Contains(err.Error(), "unknown key ratioo") {
		t.Errorf("err = %q, want it to name the key", err)
	}
	if strings.Contains(err.Error(), "engine.opts") || strings.Contains(err.Error(), "not found in type") {
		t.Errorf("err = %q, must not leak the Go type at the operator", err)
	}
}

// The line reported must be the line in the operator's file, not a line of the
// re-marshalled block — otherwise it points at the wrong place, which is worse
// than reporting none.
func TestDecodeOptionsReportsTheOperatorsLine(t *testing.T) {
	var doc yaml.Node
	full := "models:\n  - name: m\n    engine: e\n    e:\n      binary: x\n      ratioo: 0.5\n"
	if err := yaml.Unmarshal([]byte(full), &doc); err != nil {
		t.Fatal(err)
	}
	// models -> seq -> model map -> the "e" block's value.
	models := doc.Content[0].Content[1]
	model := models.Content[0]
	var blk yaml.Node
	for i := 0; i+1 < len(model.Content); i += 2 {
		if model.Content[i].Value == "e" {
			blk = *model.Content[i+1]
		}
	}
	err := DecodeOptions(Spec{Options: blk}, &opts{})
	if err == nil || !strings.Contains(err.Error(), "line 6") {
		t.Fatalf("err = %v, want the operator's line 6", err)
	}
}

func TestDecodeOptionsReportsATypeMismatch(t *testing.T) {
	s := Spec{Options: block(t, "threads: not-a-number\n")}
	err := DecodeOptions(s, &opts{})
	if err == nil || !strings.Contains(err.Error(), "threads") {
		t.Fatalf("err = %v, want it to name the field", err)
	}
}
