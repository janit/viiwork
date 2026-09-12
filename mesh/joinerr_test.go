package mesh

import (
	"errors"
	"testing"
)

func TestClassifyJoinError(t *testing.T) {
	cases := []struct {
		text string
		want joinFailure
	}{
		{"failed to join 100.64.0.2:7946: dial tcp 100.64.0.2:7946: connect: connection refused", joinUnreachable},
		{"failed to join 100.64.0.2:7946: dial tcp 100.64.0.2:7946: i/o timeout", joinUnreachable},
		{"failed to join 100.64.0.2:7946: meshtest: 100.64.0.2:7946 unreachable", joinUnreachable},
		{"failed to join 100.64.0.2:7946: EOF", joinMismatch},
		{"failed to join 100.64.0.2:7946: read tcp 100.64.0.1:40000->100.64.0.2:7946: read: connection reset by peer", joinMismatch},
		{"failed to join 100.64.0.2:7946: something else", joinOther},
		// Captured from meshtest integration runs (I6, I7) with memberlist v0.6.0:
		// the receiver answers a rejected join with an error response.
		{"1 error occurred: * Failed to join 100.64.0.1:7946: no installed keys could decrypt the message", joinMismatch},
		{"1 error occurred: * Failed to join 100.64.0.1:7946: encryption is configured but remote state is not encrypted", joinMismatch},
	}
	for _, tc := range cases {
		if got := classifyJoinError(errors.New(tc.text)); got != tc.want {
			t.Errorf("classifyJoinError(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}
