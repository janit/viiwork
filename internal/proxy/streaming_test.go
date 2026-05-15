package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
)

func TestIsHardSocketFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context_canceled", context.Canceled, false},
		{"context_deadline", context.DeadlineExceeded, false},
		{"plain_eof", io.EOF, true},
		{"unexpected_eof", io.ErrUnexpectedEOF, true},
		{"econnrefused", syscall.ECONNREFUSED, true},
		{"econnreset", syscall.ECONNRESET, true},
		{"wrapped_eof", fmt.Errorf("Post X: %w", io.EOF), true},
		{"wrapped_econnrefused", fmt.Errorf("dial X: %w", syscall.ECONNREFUSED), true},
		{"oserr_dial_econnrefused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		// read errors are NOT hard failures (slow backend ≠ gone backend)
		{"oserr_read", &net.OpError{Op: "read", Err: errors.New("some read error")}, false},
		{"generic_string", errors.New("backend overloaded"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHardSocketFailure(tc.err); got != tc.want {
				t.Errorf("isHardSocketFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
