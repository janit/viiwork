package proxy

import (
	"bytes"
	"encoding/json/jsontext"
	"io"
)

// chunkProbe reads the few fields of a streamed chat chunk that the think
// rewrite decides on, with a jsontext.Decoder reused across the chunks of one
// stream, so a token costs no allocation for the decoder, no reflection and
// no interface boxing.
//
// It is stricter than the slow path's map decode on purpose, and every chunk
// it does not accept goes to the slow path, which stays the authority:
//
//   - names are compared exactly. The slow path's map is case-sensitive, so a
//     "Choices" key is not the choices; an escaped or case-folded spelling of
//     a name the probe reads is refused rather than guessed at.
//   - a duplicate name, invalid UTF-8 or data after the object is a decode
//     error here (jsontext's defaults), and so a refusal.
//   - a field of an unexpected type is a refusal.
type chunkProbe struct {
	rd  bytes.Reader
	dec *jsontext.Decoder

	// Filled by read for an accepted chunk.
	choices      int    // length of the choices array (read stops at 2)
	hasDelta     bool   // the one choice has a delta object
	hasContent   bool   // the delta has a non-null content
	hasReasoning bool   // the delta has a string reasoning_content
	finishSet    bool   // the choice has a finish_reason other than null or ""
	reasoning    []byte // reasoning_content unquoted; valid until the next read
}

func newChunkProbe() *chunkProbe {
	p := &chunkProbe{}
	p.dec = jsontext.NewDecoder(&p.rd)
	return p
}

var (
	nameChoices      = []byte("choices")
	nameDelta        = []byte("delta")
	nameFinishReason = []byte("finish_reason")
	nameContent      = []byte("content")
	nameReasoning    = []byte("reasoning_content")
)

// read decodes payload and reports whether it is an object the probe could
// read completely. A chunk with more than one choice reports false.
func (p *chunkProbe) read(payload []byte) bool {
	p.choices, p.hasDelta, p.hasContent, p.hasReasoning, p.finishSet = 0, false, false, false, false
	p.reasoning = p.reasoning[:0]
	p.rd.Reset(payload)
	p.dec.Reset(&p.rd)
	if !p.object(p.topField) {
		return false
	}
	_, err := p.dec.ReadToken()
	return err == io.EOF
}

// object reads one object, handing each name to field, which must consume the
// value. A name with an escape in it is refused: it could spell a name field
// compares against.
func (p *chunkProbe) object(field func(name []byte) bool) bool {
	if p.dec.PeekKind() != '{' {
		return false
	}
	if _, err := p.dec.ReadToken(); err != nil {
		return false
	}
	for {
		switch p.dec.PeekKind() {
		case '}':
			_, err := p.dec.ReadToken()
			return err == nil
		case '"':
		default:
			return false
		}
		raw, err := p.dec.ReadValue()
		if err != nil || bytes.IndexByte(raw, '\\') >= 0 {
			return false
		}
		// raw is valid until the next read; field compares it before reading.
		if !field(raw[1 : len(raw)-1]) {
			return false
		}
	}
}

// known refuses a name that differs from want only in case.
func known(name, want []byte) (match, ok bool) {
	if bytes.Equal(name, want) {
		return true, true
	}
	return false, !bytes.EqualFold(name, want)
}

func (p *chunkProbe) skip() bool { return p.dec.SkipValue() == nil }

func (p *chunkProbe) topField(name []byte) bool {
	match, ok := known(name, nameChoices)
	if !ok {
		return false
	}
	if !match {
		return p.skip()
	}
	if p.dec.PeekKind() != '[' {
		return false
	}
	if _, err := p.dec.ReadToken(); err != nil {
		return false
	}
	for p.dec.PeekKind() != ']' {
		p.choices++
		if p.choices > 1 || !p.object(p.choiceField) {
			return false
		}
	}
	_, err := p.dec.ReadToken()
	return err == nil
}

func (p *chunkProbe) choiceField(name []byte) bool {
	if match, ok := known(name, nameDelta); !ok {
		return false
	} else if match {
		p.hasDelta = true
		return p.object(p.deltaField)
	}
	if match, ok := known(name, nameFinishReason); !ok {
		return false
	} else if match {
		v, err := p.dec.ReadValue()
		if err != nil {
			return false
		}
		switch {
		case string(v) == "null", string(v) == `""`:
		case v.Kind() == '"':
			p.finishSet = true
		default:
			return false
		}
		return true
	}
	return p.skip()
}

func (p *chunkProbe) deltaField(name []byte) bool {
	if match, ok := known(name, nameContent); !ok {
		return false
	} else if match {
		v, err := p.dec.ReadValue()
		if err != nil {
			return false
		}
		switch v.Kind() {
		case 'n':
		case '"':
			p.hasContent = true
		default:
			return false
		}
		return true
	}
	if match, ok := known(name, nameReasoning); !ok {
		return false
	} else if match {
		v, err := p.dec.ReadValue()
		if err != nil || v.Kind() != '"' {
			// null or another type: leave it to the slow path.
			return false
		}
		p.reasoning, err = jsontext.AppendUnquote(p.reasoning[:0], v)
		if err != nil {
			return false
		}
		p.hasReasoning = true
		return true
	}
	return p.skip()
}

// suppressible reports whether, inside a think block, the slow path would
// buffer this chunk's reasoning and emit nothing: one choice, a delta with a
// string reasoning_content and no content, no finish_reason, and no think tag
// in the decoded text (an open tag resets the buffer and a close tag ends the
// block, whatever escapes spelled them).
func (p *chunkProbe) suppressible() bool {
	return p.choices == 1 && p.hasDelta && p.hasReasoning && !p.hasContent && !p.finishSet &&
		!bytes.Contains(p.reasoning, openThinkTag) && !bytes.Contains(p.reasoning, closeThinkTag)
}

// renamable reports whether, outside a think block, the slow path would only
// rename reasoning_content to content: one choice, a delta with a string
// reasoning_content and no content key, and no open tag in the decoded text.
// The slow path renames whatever the finish_reason, so it is not read here.
func (p *chunkProbe) renamable() bool {
	return p.choices == 1 && p.hasDelta && p.hasReasoning && !p.hasContent &&
		!bytes.Contains(p.reasoning, openThinkTag)
}
