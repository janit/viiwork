package peer

import "testing"

func TestValidPeerAddr(t *testing.T) {
	tests := []struct {
		addr         string
		allowPrivate bool
		wantOK       bool
	}{
		{"100.64.0.11:9100", false, true},
		{"100.64.0.0:9100", false, true},
		{"100.127.255.255:9100", false, true},
		{"100.63.255.255:9100", false, false},
		{"100.128.0.0:9100", false, false},
		{"10.0.0.1:9100", false, false},
		{"10.0.0.1:9100", true, true},
		{"192.168.1.41:9100", true, true},
		{"172.16.0.1:9100", true, true},
		{"[fc00::1]:9100", true, true},
		{"8.8.8.8:9100", true, false},
		{"127.0.0.1:9100", true, false},
		{"169.254.169.254:80", true, false},
		{"[::1]:9100", true, false},
		{"[::]:9100", true, false},
		{"[64:ff9b::a9fe:a9fe]:80", true, false},
		{"[::ffff:100.64.0.11]:9100", false, true},
		{"gb2.tail1234.ts.net:9100", false, false},
		{"100.64.0.11:+9100", false, false},
		{"100.64.0.11:09100", false, false},
		{"100.64.0.11:0", false, false},
		{"100.64.0.11:65536", false, false},
		{"100.64.0.11:9100/latest/meta-data", false, false},
		{"100.64.0.11:9100?x=1", false, false},
		{"100.64.0.11:9100#frag", false, false},
		{"user@100.64.0.11:9100", false, false},
		{"100.64.0.11:9100 ", false, false},
		{"100.64.0.11", false, false},
		{"", false, false},
	}

	for _, tc := range tests {
		err := validPeerAddr(tc.addr, tc.allowPrivate)
		if gotOK := err == nil; gotOK != tc.wantOK {
			t.Errorf("validPeerAddr(%q, %v) ok = %v (err %v), want %v",
				tc.addr, tc.allowPrivate, gotOK, err, tc.wantOK)
		}
	}
}
