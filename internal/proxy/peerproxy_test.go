package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/meshauth"
	"github.com/janit/viiwork/internal/peer"
)

func TestProxyToPeer(t *testing.T) {
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Viiwork-Forwarded") != "viiwork-test" {
			t.Errorf("expected X-Viiwork-Forwarded header, got %q", r.Header.Get("X-Viiwork-Forwarded"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-peer","choices":[{"message":{"content":"from peer"}}]}`))
	}))
	defer peerSrv.Close()

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"peer-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	proxyToPeer(w, req, peerSrv.Listener.Addr().String(), "viiwork-test", false, nil, nil)

	if w.Code != 200 { t.Errorf("expected 200, got %d", w.Code) }
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), "from peer") { t.Errorf("expected 'from peer' in body, got %s", body) }
}

func TestProxyToPeerHeaders(t *testing.T) {
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-GPU-Backend", "gpu-0")
		w.Write([]byte(`{}`))
	}))
	defer peerSrv.Close()

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	proxyToPeer(w, req, peerSrv.Listener.Addr().String(), "viiwork-test", false, nil, nil)

	if w.Header().Get("X-GPU-Backend") != "gpu-0" { t.Errorf("expected X-GPU-Backend from peer, got %q", w.Header().Get("X-GPU-Backend")) }
	if w.Header().Get("X-Viiwork-Origin") == "" { t.Error("expected X-Viiwork-Origin header") }
}

func TestProxyToPeerUnreachable(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	proxyToPeer(w, req, "127.0.0.1:1", "viiwork-test", false, nil, nil)
	if w.Code != 502 { t.Errorf("expected 502, got %d", w.Code) }
}

func TestForwardCarriesAProof(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	caller, _ := meshauth.NewSigner(secret, "gb1-a1b2")
	receiver, _ := meshauth.NewSigner(secret, "gb2-c3d4")
	body := []byte(`{"model":"m1"}`)

	var verified bool
	peerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		_, _, err := receiver.VerifyRequest(r, got)
		verified = err == nil
		w.Write([]byte("ok"))
	}))
	defer peerSrv.Close()

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	w := httptest.NewRecorder()
	proxyToPeer(w, req, peerSrv.Listener.Addr().String(), "gb1-a1b2", false, body, caller)

	if !verified {
		t.Fatal("the forwarded request carried no valid proof")
	}
}

func TestForwardIsRejectedWhenProofRequiredAndAbsent(t *testing.T) {
	h := &Handler{requireForwardProof: true}
	h.signer, _ = meshauth.NewSigner([]byte("0123456789abcdef0123456789abcdef"), "gb2-c3d4")

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"m1"}`)))
	req.Header.Set(HeaderForwarded, "gb1-a1b2")

	if ok := h.forwardIsTrusted(req, []byte(`{"model":"m1"}`)); ok {
		t.Fatal("an unsigned forward must be refused when require_forward_proof is on")
	}
}

func TestForwardIsAcceptedUnsignedDuringRollout(t *testing.T) {
	// A configured peer running an older build: it cannot sign, and with the
	// flag off its forward must still be honoured on the old claim alone.
	known := peer.NewPeerState("100.64.0.11:9100")
	known.Update(peer.StatusResponse{NodeID: "gb1-a1b2", Models: []string{"m1"}})
	reg := peer.NewRegistry("gb2-c3d4", "m1", nil, []*peer.PeerState{known}, time.Second)

	h := &Handler{requireForwardProof: false, registry: reg}

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"m1"}`)))
	req.Header.Set(HeaderForwarded, "gb1-a1b2")

	if ok := h.forwardIsTrusted(req, []byte(`{"model":"m1"}`)); !ok {
		t.Fatal("with the flag off, an un-upgraded peer's forward must still be honoured")
	}
}

func TestReplayedForwardIsRejected(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	caller, _ := meshauth.NewSigner(secret, "gb1-a1b2")
	h := &Handler{requireForwardProof: true, forwardNonces: meshauth.NewNonceCache(2 * meshauth.SkewWindow)}
	h.signer, _ = meshauth.NewSigner(secret, "gb2-c3d4")

	body := []byte(`{"model":"m1"}`)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	caller.SignRequest(req, body)
	req.Header.Set(HeaderForwarded, "gb1-a1b2")

	if !h.forwardIsTrusted(req, body) {
		t.Fatal("first forward should be accepted")
	}
	if h.forwardIsTrusted(req, body) {
		t.Fatal("a replayed forward must be rejected")
	}
}
