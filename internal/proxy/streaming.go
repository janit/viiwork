package proxy

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/janit/viiwork/internal/balancer"
)

// backendClient has no timeout — LLM inference requests can stream for minutes.
// The context from the incoming request controls cancellation.
var backendClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	},
}

func proxyRequest(w http.ResponseWriter, r *http.Request, backend *balancer.BackendState, latencyWindow time.Duration) {
	start := time.Now()
	backend.IncrInFlight()
	defer func() {
		backend.DecrInFlight()
		backend.RecordLatency(time.Since(start), latencyWindow)
	}()

	targetURL := fmt.Sprintf("http://%s%s", backend.Addr, r.URL.Path)
	if r.URL.RawQuery != "" { targetURL += "?" + r.URL.RawQuery }

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "proxy error", http.StatusBadGateway)
		return
	}
	for key, values := range r.Header {
		for _, v := range values { proxyReq.Header.Add(key, v) }
	}

	resp, err := backendClient.Do(proxyReq)
	if err != nil {
		http.Error(w, "backend unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, v := range values { w.Header().Add(key, v) }
	}
	w.Header().Set("X-GPU-Backend", fmt.Sprintf("gpu-%d", backend.GPUID))
	w.Header().Set("X-Queue-Depth", fmt.Sprintf("%d", backend.InFlight()))
	w.WriteHeader(resp.StatusCode)

	if f, ok := w.(http.Flusher); ok {
		buf := make([]byte, 4096)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				if _, writeErr := w.Write(buf[:n]); writeErr != nil {
					break // client disconnected
				}
				f.Flush()
			}
			if readErr != nil { break }
		}
	} else {
		io.Copy(w, resp.Body)
	}
}
