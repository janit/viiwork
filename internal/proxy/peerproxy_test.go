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

// The forward's overall timeout is long on purpose — a completion streams for
// as long as generation takes — so it cannot double as the handshake bound. A
// powered-off peer drops packets rather than refusing, and until the poll loop
// notices, every request routed to it used to sit in the dial for the whole
// 120s budget.
func TestPeerClientBoundsTheDial(t *testing.T) {
	if peerClient.Timeout != 120*time.Second {
		t.Fatalf("overall timeout = %s, want 120s: a streaming completion needs it", peerClient.Timeout)
	}
	if peerDialTimeout <= 0 || peerDialTimeout > peerClient.Timeout/10 {
		t.Fatalf("dial timeout %s should be a small fraction of the overall %s", peerDialTimeout, peerClient.Timeout)
	}
	tr, ok := peerClient.Transport.(*http.Transport)
	if !ok || tr.DialContext == nil {
		t.Fatal("peerClient must carry its own Transport with a bounded DialContext, not http.DefaultTransport")
	}

	// TEST-NET-1 (RFC 5737) is routed nowhere, so the dial times out the way
	// a powered-off chassis does. A sandbox with no route at all fails faster
	// still; either way the bound has to hold.
	const dial = 200 * time.Millisecond
	client := newPeerClient(dial)
	req, _ := http.NewRequest("POST", "http://192.0.2.1:9/v1/chat/completions", strings.NewReader("{}"))
	start := time.Now()
	resp, err := client.Do(req)
	took := time.Since(start)
	if err == nil {
		resp.Body.Close()
		t.Fatal("a blackholed peer must not answer")
	}
	if limit := 10 * dial; took > limit {
		t.Fatalf("dial to a blackholed peer took %s (limit %s): the dial is not bounded", took, limit)
	}
}
