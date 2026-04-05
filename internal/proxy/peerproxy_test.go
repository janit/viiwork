package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	proxyToPeer(w, req, peerSrv.Listener.Addr().String(), "viiwork-test", false)

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
	proxyToPeer(w, req, peerSrv.Listener.Addr().String(), "viiwork-test", false)

	if w.Header().Get("X-GPU-Backend") != "gpu-0" { t.Errorf("expected X-GPU-Backend from peer, got %q", w.Header().Get("X-GPU-Backend")) }
	if w.Header().Get("X-Viiwork-Origin") == "" { t.Error("expected X-Viiwork-Origin header") }
}

func TestProxyToPeerUnreachable(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	proxyToPeer(w, req, "127.0.0.1:1", "viiwork-test", false)
	if w.Code != 502 { t.Errorf("expected 502, got %d", w.Code) }
}
