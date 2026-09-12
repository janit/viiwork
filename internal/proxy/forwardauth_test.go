package proxy

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/meshapi"
)

var (
	k1 = bytes.Repeat([]byte{1}, 32)
	k2 = bytes.Repeat([]byte{2}, 32)
)

type fakeMembers []mesh.Member

func (f fakeMembers) Members() []mesh.Member { return f }

func memberAt(name, addr, state string, local bool) mesh.Member {
	return mesh.Member{Name: name, Addr: netip.MustParseAddr(addr), Port: 7946, State: state, Local: local,
		Meta: mesh.NodeMeta{V: mesh.MetaVersion, API: 8086, Ver: "test", Role: meshapi.RoleNode}}
}

func mustAuth(t *testing.T, self string, primary, previous []byte, members Members) *ForwardAuth {
	t.Helper()
	a, err := NewForwardAuth(self, primary, previous, members)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func signedRequest(t *testing.T, signer *ForwardAuth, target string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err := signer.Sign(req, body); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestForwardAuthSecured(t *testing.T) {
	body := []byte(`{"model":"m"}`)
	gb1 := mustAuth(t, "gb1", k1, nil, fakeMembers{})

	t.Run("F1 valid", func(t *testing.T) {
		gb2 := mustAuth(t, "gb2", k1, nil, fakeMembers{})
		req := signedRequest(t, gb1, "/v1/chat/completions?host=gb2", body)
		if origin, ok := gb2.Verify(req, body); !ok || origin != "gb1" {
			t.Errorf("Verify = %q, %v", origin, ok)
		}
		// F4: a replay of the same request is refused.
		if _, ok := gb2.Verify(req, body); ok {
			t.Error("F4: the replayed request must not verify")
		}
	})

	t.Run("F2 body changed", func(t *testing.T) {
		gb2 := mustAuth(t, "gb2", k1, nil, fakeMembers{})
		req := signedRequest(t, gb1, "/v1/chat/completions", body)
		if _, ok := gb2.Verify(req, []byte(`{"model":"x"}`)); ok {
			t.Error("a changed body must not verify")
		}
	})

	t.Run("F3 query changed", func(t *testing.T) {
		gb2 := mustAuth(t, "gb2", k1, nil, fakeMembers{})
		req := signedRequest(t, gb1, "/v1/chat/completions?host=gb2", body)
		req.URL.RawQuery = "host=gb3"
		if _, ok := gb2.Verify(req, body); ok {
			t.Error("a changed query must not verify")
		}
	})

	t.Run("F5 wrong secret", func(t *testing.T) {
		gb2 := mustAuth(t, "gb2", k2, nil, fakeMembers{})
		if _, ok := gb2.Verify(signedRequest(t, gb1, "/v1/chat/completions", body), body); ok {
			t.Error("a different secret must not verify")
		}
	})

	t.Run("F6 F7 rotation", func(t *testing.T) {
		rotated := mustAuth(t, "gb2", k2, k1, fakeMembers{})
		if _, ok := rotated.Verify(signedRequest(t, gb1, "/v1/chat/completions", body), body); !ok {
			t.Error("F6: the previous secret must still verify")
		}
		noPrev := mustAuth(t, "gb2", k2, nil, fakeMembers{})
		if _, ok := noPrev.Verify(signedRequest(t, gb1, "/v1/chat/completions", body), body); ok {
			t.Error("F7: without the previous secret the old signature must fail")
		}
	})

	t.Run("F8 origin not the signer", func(t *testing.T) {
		gb2 := mustAuth(t, "gb2", k1, nil, fakeMembers{})
		req := signedRequest(t, gb1, "/v1/chat/completions", body)
		req.Header.Set(meshapi.HeaderForwarded, "gb3")
		if _, ok := gb2.Verify(req, body); ok {
			t.Error("a valid signature from gb1 must not vouch for gb3")
		}
	})

	t.Run("F9 no forward claim", func(t *testing.T) {
		gb2 := mustAuth(t, "gb2", k1, nil, fakeMembers{})
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		if _, ok := gb2.Verify(req, body); ok {
			t.Error("a request without X-Viiwork-Forwarded is not a forward")
		}
	})

	t.Run("F10 claim without proof", func(t *testing.T) {
		gb2 := mustAuth(t, "gb2", k1, nil, fakeMembers{})
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set(meshapi.HeaderForwarded, "gb1")
		if _, ok := gb2.Verify(req, body); ok {
			t.Error("an unsigned claim must not verify in a secured mesh")
		}
	})
}

func TestForwardAuthOpen(t *testing.T) {
	body := []byte(`{"model":"m"}`)
	members := fakeMembers{
		memberAt("gb2", "100.64.0.2", meshapi.MemberAlive, true),
		memberAt("gb1", "100.64.0.1", meshapi.MemberAlive, false),
		memberAt("gb3", "100.64.0.3", meshapi.MemberAlive, false),
		memberAt("gb4", "100.64.0.4", meshapi.MemberDead, false),
	}
	gb2 := mustAuth(t, "gb2", nil, nil, members)
	claim := func(origin, remote string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set(meshapi.HeaderForwarded, origin)
		req.RemoteAddr = remote
		return req
	}
	cases := []struct {
		name   string
		req    *http.Request
		wantOK bool
	}{
		{"F11 member at its address", claim("gb1", "100.64.0.1:51234"), true},
		{"F12 wrong source address", claim("gb1", "100.64.0.9:51234"), false},
		{"F13 dead member", claim("gb4", "100.64.0.4:51234"), false},
		{"F14 another member's name", claim("gb3", "100.64.0.1:51234"), false},
		{"F15 claims to be this node", claim("gb2", "100.64.0.2:51234"), false},
	}
	for _, tc := range cases {
		if origin, ok := gb2.Verify(tc.req, body); ok != tc.wantOK {
			t.Errorf("%s: Verify = %q, %v; want ok=%v", tc.name, origin, ok, tc.wantOK)
		}
	}
}

func TestStripMeshHeaders(t *testing.T) {
	h := http.Header{}
	for _, k := range []string{meshapi.HeaderForwarded, "X-Viiwork-Node", "X-Viiwork-Ts", "X-Viiwork-Nonce", "X-Viiwork-Auth", HeaderTask} {
		h.Set(k, "x")
	}
	stripMeshHeaders(h)
	if len(h) != 1 || h.Get(HeaderTask) != "x" {
		t.Errorf("headers after strip = %v", h)
	}
}

func TestNewForwardAuthShortSecret(t *testing.T) {
	if _, err := NewForwardAuth("gb1", make([]byte, 16), nil, fakeMembers{}); err == nil {
		t.Error("a 16-byte secret must be refused")
	}
}

func TestForwardAuthRejectionLogRateLimited(t *testing.T) {
	gb2 := mustAuth(t, "gb2", k1, nil, fakeMembers{})
	var mu sync.Mutex
	var lines []string
	gb2.logf = func(format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
		req.Header.Set(meshapi.HeaderForwarded, "gb3")
		gb2.Verify(req, []byte("{}"))
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "claiming to be gb3") {
		t.Errorf("log lines = %q, want one", lines)
	}
}
