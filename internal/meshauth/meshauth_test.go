package meshauth

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestCanonicalRequestVector(t *testing.T) {
	got := canonicalRequest("GET", "/v1/status", "1756800000", "AAAAAAAAAAAAAAAAAAAAAA", "gb1-a1b2", "")
	want := "v1\nreq\nGET\n/v1/status\n1756800000\nAAAAAAAAAAAAAAAAAAAAAA\ngb1-a1b2\n"
	if got != want {
		t.Fatalf("canonical request:\n got %q\nwant %q", got, want)
	}
}

func TestCanonicalResponseVector(t *testing.T) {
	got := canonicalResponse("/v1/cluster", "1756800000", "AAAAAAAAAAAAAAAAAAAAAA", "gb2-c3d4", "abc123")
	want := "v1\nresp\n/v1/cluster\n1756800000\nAAAAAAAAAAAAAAAAAAAAAA\ngb2-c3d4\nabc123"
	if got != want {
		t.Fatalf("canonical response:\n got %q\nwant %q", got, want)
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	caller, err := NewSigner(secret, "gb1-a1b2")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	responder, err := NewSigner(secret, "gb2-c3d4")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	req, _ := http.NewRequest("GET", "http://100.64.0.11:9100/v1/cluster", nil)
	nonce, err := caller.SignRequest(req, nil)
	if err != nil {
		t.Fatalf("SignRequest: %v", err)
	}

	gotNonce, gotNode, err := responder.VerifyRequest(req, nil)
	if err != nil {
		t.Fatalf("VerifyRequest: %v", err)
	}
	if gotNonce != nonce {
		t.Errorf("nonce = %q, want %q", gotNonce, nonce)
	}
	if gotNode != "gb1-a1b2" {
		t.Errorf("caller = %q, want gb1-a1b2", gotNode)
	}

	body := []byte(`{"node_id":"gb2-c3d4"}`)
	h := http.Header{}
	responder.SignResponse(h, "/v1/cluster", gotNonce, body)

	if err := caller.VerifyResponse(h, "/v1/cluster", nonce, body); err != nil {
		t.Fatalf("VerifyResponse: %v", err)
	}
}

func TestNewSignerRejectsShortSecret(t *testing.T) {
	if _, err := NewSigner([]byte("too short"), "gb1"); err == nil {
		t.Fatal("expected an error for a secret under MinSecretLen")
	}
}

func TestVerifyRequestFailureModes(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	caller, _ := NewSigner(secret, "gb1-a1b2")
	responder, _ := NewSigner(secret, "gb2-c3d4")
	other, _ := NewSigner([]byte("ffffffffffffffffffffffffffffffff"), "gb3-e5f6")

	sign := func(s *Signer, body []byte) *http.Request {
		req, _ := http.NewRequest("POST", "http://h/v1/chat/completions", nil)
		s.SignRequest(req, body)
		return req
	}

	tests := []struct {
		name    string
		mutate  func(*http.Request)
		body    []byte
		wantErr error
	}{
		{"no headers at all", func(r *http.Request) {
			r.Header.Del(HeaderAuth)
			r.Header.Del(HeaderTs)
			r.Header.Del(HeaderNonce)
			r.Header.Del(HeaderNode)
		}, nil, ErrNoProof},
		{"missing auth only", func(r *http.Request) { r.Header.Del(HeaderAuth) }, nil, ErrBadProof},
		{"wrong scheme prefix", func(r *http.Request) { r.Header.Set(HeaderAuth, "v2=aaaa") }, nil, ErrBadProof},
		{"ts too old", func(r *http.Request) {
			r.Header.Set(HeaderTs, strconv.FormatInt(time.Now().Add(-5*time.Minute).Unix(), 10))
		}, nil, ErrBadProof},
		{"ts in the future", func(r *http.Request) {
			r.Header.Set(HeaderTs, strconv.FormatInt(time.Now().Add(5*time.Minute).Unix(), 10))
		}, nil, ErrBadProof},
		{"ts not a number", func(r *http.Request) { r.Header.Set(HeaderTs, "soon") }, nil, ErrBadProof},
		{"nonce not base64", func(r *http.Request) { r.Header.Set(HeaderNonce, "!!!!") }, nil, ErrBadProof},
		{"nonce wrong length", func(r *http.Request) {
			r.Header.Set(HeaderNonce, base64.RawURLEncoding.EncodeToString([]byte("short")))
		}, nil, ErrBadProof},
		{"node id swapped", func(r *http.Request) { r.Header.Set(HeaderNode, "gb9-9999") }, nil, ErrBadProof},
		{"body tampered", func(r *http.Request) {}, []byte(`{"model":"evil"}`), ErrBadProof},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := sign(caller, []byte(`{"model":"real"}`))
			tc.mutate(req)
			body := tc.body
			if body == nil {
				body = []byte(`{"model":"real"}`)
			}
			_, _, err := responder.VerifyRequest(req, body)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}

	t.Run("wrong secret", func(t *testing.T) {
		req := sign(other, nil)
		if _, _, err := responder.VerifyRequest(req, nil); !errors.Is(err, ErrBadProof) {
			t.Fatalf("err = %v, want ErrBadProof", err)
		}
	})
}
