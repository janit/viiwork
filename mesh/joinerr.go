package mesh

import "strings"

// joinFailure classifies why memberlist.Join failed for one address.
type joinFailure int

const (
	joinUnreachable joinFailure = iota // nothing answered: off, not viiwork, or firewalled
	joinMismatch                       // it answered and hung up: mesh mode or secret differ
	joinOther
)

// classifyJoinError reads the error text.
//
// P3 Decision 5 assumed the joiner of a rejected join only sees the stream
// close (EOF or a reset). memberlist v0.6.0 does more: the receiving member
// logs the precise cause and sends an error response back, so the joiner's
// Join fails with an encryption error of its own: "no installed keys could
// decrypt the message" when the two secrets differ (it cannot decrypt the
// reply), "encryption is configured but remote state is not encrypted" when
// the other side is open. Both shapes are a mismatch.
func classifyJoinError(err error) joinFailure {
	text := err.Error()
	for _, s := range []string{
		"no installed keys could decrypt",
		"encryption is configured but remote state is not encrypted",
		"remote state is encrypted and encryption is not configured",
	} {
		if strings.Contains(text, s) {
			return joinMismatch
		}
	}
	for _, s := range []string{"connection refused", "no route to host", "i/o timeout", "unreachable", "deadline exceeded"} {
		if strings.Contains(text, s) {
			return joinUnreachable
		}
	}
	for _, s := range []string{"EOF", "connection reset by peer"} {
		if strings.Contains(text, s) {
			return joinMismatch
		}
	}
	return joinOther
}
