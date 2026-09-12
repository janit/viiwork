package mesh

import (
	"bytes"
	"testing"
)

func TestFrame(t *testing.T) {
	b := frame(framePayload, []byte("x"))
	if !bytes.Equal(b, []byte{0x02, 'x'}) {
		t.Errorf("frame = %v", b)
	}
	kind, body, ok := parseFrame(b)
	if !ok || kind != framePayload || string(body) != "x" {
		t.Errorf("parseFrame = %v, %q, %v", kind, body, ok)
	}
	if _, _, ok := parseFrame(nil); ok {
		t.Error("an empty message is not a frame")
	}
	if _, _, ok := parseFrame([]byte{0x07, 'x'}); ok {
		t.Error("an unknown kind is not a frame")
	}
}
