package proxy

import (
	"bytes"
	"encoding/json"
	"strconv"
	"time"

	"github.com/janit/viiwork/v2/internal/route"
	"github.com/janit/viiwork/v2/internal/sessionshare"
	"github.com/janit/viiwork/v2/internal/supervisor"
)

// withSlot names the slot a slot-addressed backend must run the request in
// (route.Lease.Slot): the field is spliced in after the object's opening
// brace, one copy of the body and no decode. A body that already has the
// field at any depth takes the slow path, a decode and re-encode of the top
// level, so the node's slot always wins over a client's; so does a body with
// any object key written with a \u escape, which could spell the field
// without its bytes (llama.cpp keeps the last of two equal keys). A body that is not
// an object goes out unchanged; the engine refuses it on its own.
func withSlot(body []byte, field string, slot int) []byte {
	var kb [64]byte
	key := append(append(append(kb[:0], '"'), field...), '"')
	if bytes.Contains(body, key) || escapedKey(body) {
		var generic map[string]json.RawMessage
		if json.Unmarshal(body, &generic) != nil {
			return body
		}
		generic[field] = json.RawMessage(strconv.Itoa(slot))
		out, err := json.Marshal(generic)
		if err != nil {
			return body
		}
		return out
	}
	i := skipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return body
	}
	out := make([]byte, 0, len(body)+len(key)+22)
	out = append(out, body[:i+1]...)
	out = append(out, key...)
	out = append(out, ':')
	out = strconv.AppendInt(out, int64(slot), 10)
	if j := skipSpace(body, i+1); j < len(body) && body[j] != '}' {
		out = append(out, ',')
	}
	return append(out, body[i+1:]...)
}

// escapedKey reports whether an object key in body (valid JSON assumed)
// contains a \u escape: a real one (preceded by an even run of backslashes)
// in a string that is followed by a colon. Only such a body can name a
// field without its literal bytes.
var uEscape = []byte(`\u`)

func escapedKey(body []byte) bool {
	for i := 0; ; {
		j := bytes.Index(body[i:], uEscape)
		if j < 0 {
			return false
		}
		j += i
		run := 0
		for k := j; k >= 0 && body[k] == '\\'; k-- {
			run++
		}
		i = j + 2
		if run%2 == 0 {
			continue // an escaped backslash followed by a u
		}
		// The rest of this string, then what follows it.
		k := i
		for k < len(body) && body[k] != '"' {
			if body[k] == '\\' {
				k++
			}
			k++
		}
		if k >= len(body) {
			return false
		}
		if n := skipSpace(body, k+1); n < len(body) && body[n] == ':' {
			return true
		}
		i = k + 1
	}
}

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// sessionBackend is the lease's backend as kv_share sees it: with the slot
// the lease claimed on a slot-addressed backend (sessionshare.Slotted),
// which is what a SlotSharer model saves and restores.
func sessionBackend(l *route.Lease) sessionshare.Backend {
	c, ok := l.Slot()
	if !ok {
		return l.Backend()
	}
	return leasedSlot{l.Backend(), c}
}

// displacesSession reports whether the lease's slot held another session's
// conversation when it was claimed.
func displacesSession(l *route.Lease) bool {
	c, ok := l.Slot()
	return ok && c.Displaced != 0
}

type leasedSlot struct {
	route.LocalBackend
	claim supervisor.SlotClaim
}

func (s leasedSlot) Slot() (supervisor.SlotClaim, bool) { return s.claim, true }

// ModelPath forwards the backend's weights path, which kv_share fingerprints.
func (s leasedSlot) ModelPath() string {
	p, ok := s.LocalBackend.(interface{ ModelPath() string })
	if !ok {
		return ""
	}
	return p.ModelPath()
}

func (s leasedSlot) SlotHolds(c supervisor.SlotClaim, session uint64) bool {
	h, ok := s.LocalBackend.(interface {
		SlotHolds(supervisor.SlotClaim, uint64) bool
	})
	return ok && h.SlotHolds(c, session)
}

// LaunchedAt passes the backend's launch through: kv_share keys what a
// backend holds by its launch.
func (s leasedSlot) LaunchedAt() time.Time {
	if l, ok := s.LocalBackend.(interface{ LaunchedAt() time.Time }); ok {
		return l.LaunchedAt()
	}
	return time.Time{}
}

var _ sessionshare.Slotted = leasedSlot{}
