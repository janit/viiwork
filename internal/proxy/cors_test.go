package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/balancer"
)

func testCORS() *CORS {
	return &CORS{
		Origins:    []string{"*.ts.net", "*.example.com", "localhost"},
		TailnetIPs: true,
	}
}

func TestCORSAllows(t *testing.T) {
	c := testCORS()
	cases := []struct {
		origin string
		want   bool
		why    string
	}{
		{"https://node0.tailnet-abc.ts.net", true, "MagicDNS name"},
		{"http://node0.tailnet-abc.ts.net:8080", true, "port is not part of the host match"},
		{"https://admin.example.com", true, "allowed subdomain"},
		{"http://localhost:5180", true, "exact pattern, dev server"},
		{"http://100.100.42.7:8080", true, "tailnet IPv4 literal"},
		{"http://[fd7a:115c:a1e0::1]", true, "tailnet IPv6 literal"},

		{"https://ts.net", false, "a *. rule must not match the bare apex"},
		{"https://example.com.evil.test", false, "suffix must be a real label boundary"},
		{"https://notexample.com", false, "must not match a longer label ending the same way"},
		{"http://192.168.1.10", false, "LAN address is not tailnet"},
		{"http://100.63.255.255", false, "just below the CGNAT block"},
		{"http://100.128.0.0", false, "just above the CGNAT block"},
		{"null", false, "sandboxed iframe / file origin"},
		{"", false, "no Origin header"},
		{"chrome-extension://abcdef", false, "non-http scheme"},
	}
	for _, tc := range cases {
		if got := c.Allows(tc.origin); got != tc.want {
			t.Errorf("Allows(%q) = %v, want %v — %s", tc.origin, got, tc.want, tc.why)
		}
	}
}

// A nil CORS is the "not configured" state and must never emit a header.
func TestCORSNilAllowsNothing(t *testing.T) {
	var c *CORS
	if c.Allows("https://node0.tailnet-abc.ts.net") {
		t.Error("nil CORS allowed an origin")
	}
}

func newCORSHandler(t *testing.T) *Handler {
	t.Helper()
	h := NewHandler(balancer.New(nil, 7, 4), "/models/test.gguf", 30*time.Second)
	h.SetCORS(testCORS())
	return h
}

func TestCORSHeadersOnGET(t *testing.T) {
	h := newCORSHandler(t)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://admin.example.com" {
		t.Errorf("allow-origin = %q, want the echoed origin", got)
	}
	if got := w.Header().Get("Vary"); got == "" {
		t.Error("Vary: Origin missing — a shared cache could cross origins over")
	}
	if got := w.Header().Get("Access-Control-Expose-Headers"); got == "" {
		t.Error("expose-headers missing; a browser client cannot read X-GPU-Backend")
	}
}

// Vary must be set even when the origin is refused, or a cache can hand the
// allowed origin's response to a disallowed one.
func TestCORSDisallowedOriginGetsNoHeaderButStillVaries(t *testing.T) {
	h := newCORSHandler(t)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Origin", "https://evil.test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("allow-origin = %q, want empty for a disallowed origin", got)
	}
	if w.Header().Get("Vary") == "" {
		t.Error("Vary: Origin missing on the refusal path")
	}
	if w.Code != 200 {
		t.Errorf("status = %d; the request itself is not blocked, only the browser's read of it", w.Code)
	}
}

// Before CORS existed the router matched only GET and POST, so every preflight
// fell through to 404 and no cross-origin POST could work at all.
func TestCORSPreflightAnswered(t *testing.T) {
	h := newCORSHandler(t)
	req := httptest.NewRequest("OPTIONS", "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type, x-viiwork-task")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("allow-methods missing")
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); got != "content-type, x-viiwork-task" {
		t.Errorf("allow-headers = %q, want the requested set echoed", got)
	}
}

func TestCORSPreflightRefused(t *testing.T) {
	h := newCORSHandler(t)
	req := httptest.NewRequest("OPTIONS", "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://evil.test")
	req.Header.Set("Access-Control-Request-Method", "POST")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 so the cause is visible in devtools", w.Code)
	}
}

// A plain OPTIONS with no Access-Control-Request-Method is not a preflight and
// must keep its previous behaviour rather than being swallowed as one.
func TestCORSNonPreflightOptionsStill404s(t *testing.T) {
	h := newCORSHandler(t)
	req := httptest.NewRequest("OPTIONS", "/v1/models", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// Without CORS configured the API must behave exactly as it did before.
func TestNoCORSConfiguredSendsNoHeaders(t *testing.T) {
	h := NewHandler(balancer.New(nil, 7, 4), "/models/test.gguf", 30*time.Second)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Origin", "https://admin.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("allow-origin = %q, want none when CORS is unconfigured", got)
	}
}

// EventSource sends no preflight, so an SSE endpoint that misses the header on
// its own GET response is unusable cross-origin no matter what OPTIONS does.
func TestCORSHeaderOnSSEEndpoint(t *testing.T) {
	h := newCORSHandler(t)
	req := httptest.NewRequest("GET", "/v1/metrics/stream", nil)
	req.Header.Set("Origin", "https://node0.tailnet-abc.ts.net")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://node0.tailnet-abc.ts.net" {
		t.Errorf("allow-origin = %q on the SSE path", got)
	}
}
