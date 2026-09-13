package engine

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// DecodeOptions decodes this model's engine block into dst, rejecting any key
// dst does not define. A zero Spec.Options leaves dst untouched, so an engine
// sets its defaults on dst and decodes over them.
//
// The block is re-marshalled and decoded with KnownFields rather than decoded
// from the node directly, because yaml.Node.Decode has no strict mode. That
// loses the node's line numbers, so an unknown key is looked back up in the
// original node to report where the operator actually wrote it — a line number
// counted from the start of the re-marshalled block would point at the wrong
// line of their file, which is worse than none.
func DecodeOptions(s Spec, dst any) error {
	if s.Options.IsZero() {
		return nil
	}
	b, err := yaml.Marshal(&s.Options)
	if err != nil {
		return fmt.Errorf("re-encoding options: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		return optionsError(err, b, &s.Options)
	}
	return nil
}

// unknownField matches yaml.v3's strict-mode complaint, whose own wording
// names a Go type the operator has never heard of.
var unknownField = regexp.MustCompile(`field (\S+) not found in type \S+`)

var relLineRe = regexp.MustCompile(`^line (\d+): `)

// optionsError turns a yaml.TypeError into one the operator can act on: the
// key they wrote, and the line they wrote it on.
//
// yaml reports a line in the RE-MARSHALLED block, which is not a line of their
// file, so every message is relocated through the original node. A message that
// cannot be relocated keeps the key and loses the line, which is honest; a
// message that kept yaml's own line number would point confidently at the wrong
// place, which is worse than saying nothing.
func optionsError(err error, marshalled []byte, node *yaml.Node) error {
	var te *yaml.TypeError
	if !errors.As(err, &te) || len(te.Errors) == 0 {
		return err
	}
	lines := strings.Split(string(marshalled), "\n")
	msgs := make([]string, 0, len(te.Errors))
	for _, e := range te.Errors {
		rel := 0
		if m := relLineRe.FindStringSubmatch(e); m != nil {
			rel, _ = strconv.Atoi(m[1])
			e = relLineRe.ReplaceAllString(e, "")
		}
		if m := unknownField.FindStringSubmatch(e); m != nil {
			msgs = append(msgs, withLine("unknown key "+m[1], keyLine(node, m[1])))
			continue
		}
		// A type mismatch: yaml's wording is about the value rather than about
		// Go, but it names no field, so the key is recovered from the line it
		// reported and looked back up for the operator's own line number.
		key := keyAtLine(lines, rel)
		if key != "" {
			e = key + ": " + e
		}
		msgs = append(msgs, withLine(strings.TrimSpace(e), keyLine(node, key)))
	}
	return errors.New(strings.Join(msgs, "; "))
}

func withLine(msg string, line int) string {
	if line > 0 {
		return fmt.Sprintf("%s (line %d)", msg, line)
	}
	return msg
}

// keyAtLine reads the mapping key from one line of the re-marshalled block.
// Empty for a line that is not a plain "key: value", such as a nested item.
func keyAtLine(lines []string, rel int) string {
	if rel < 1 || rel > len(lines) {
		return ""
	}
	key, _, found := strings.Cut(strings.TrimSpace(lines[rel-1]), ":")
	if !found || strings.ContainsAny(key, " \t-") {
		return ""
	}
	return key
}

// keyLine finds where key was written in a mapping node, or 0 if it is not
// there — which happens when the key came from a merge or an anchor.
func keyLine(node *yaml.Node, key string) int {
	if node == nil || node.Kind != yaml.MappingNode {
		return 0
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i].Line
		}
	}
	return 0
}
