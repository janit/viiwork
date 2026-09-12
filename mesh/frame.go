package mesh

// Gossip user messages share one channel, so each is framed by its first byte
// (Decision 11). The framing lives here, which nodes and the gateway both
// import, so it cannot drift. Push/pull state is the payload's alone and is
// not framed.
const (
	frameLeave   byte = 0x01 // body: the leaving node's name
	framePayload byte = 0x02 // body: one Payload broadcast
)

// frame returns the kind byte followed by the body.
func frame(kind byte, body []byte) []byte {
	out := make([]byte, 0, len(body)+1)
	out = append(out, kind)
	return append(out, body...)
}

// parseFrame splits a message. body aliases b; callers that keep it copy it.
func parseFrame(b []byte) (kind byte, body []byte, ok bool) {
	if len(b) == 0 {
		return 0, nil, false
	}
	switch b[0] {
	case frameLeave, framePayload:
		return b[0], b[1:], true
	}
	return 0, nil, false
}
