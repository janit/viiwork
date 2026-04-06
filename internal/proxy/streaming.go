package proxy

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/janit/viiwork/internal/balancer"
)

// hopByHopHeaders are HTTP/1.1 headers that must not be forwarded by proxies (RFC 7230).
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// backendClient has no timeout — LLM inference requests can stream for minutes.
// The context from the incoming request controls cancellation.
var backendClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	},
}

// proxyRequest forwards a request to a backend. When thinkDisabled is true,
// reasoning_content is rewritten to content with think blocks stripped.
// Returns true if the client disconnected early.
func proxyRequest(w http.ResponseWriter, r *http.Request, backend *balancer.BackendState, latencyWindow time.Duration, thinkDisabled bool) (clientAborted bool) {
	start := time.Now()
	defer func() {
		backend.DecrInFlight()
		backend.RecordLatency(time.Since(start), latencyWindow)
	}()
	backend.IncrInFlight()

	// Derive a cancellable context so we can kill the backend request immediately
	// when the client disconnects, rather than waiting for resp.Body to drain.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	targetURL := fmt.Sprintf("http://%s%s", backend.Addr, r.URL.Path)
	if r.URL.RawQuery != "" { targetURL += "?" + r.URL.RawQuery }

	proxyReq, err := http.NewRequestWithContext(ctx, r.Method, targetURL, r.Body)
	if err != nil {
		log.Printf("[debug] proxy request creation failed for gpu-%d: %v", backend.GPUID, err)
		http.Error(w, "proxy error", http.StatusBadGateway)
		return
	}
	for key, values := range r.Header {
		for _, v := range values { proxyReq.Header.Add(key, v) }
	}

	resp, err := backendClient.Do(proxyReq)
	if err != nil {
		log.Printf("[debug] backend gpu-%d (%s) unavailable: %v", backend.GPUID, backend.Addr, err)
		http.Error(w, "backend unavailable", http.StatusBadGateway)
		return
	}
	log.Printf("[debug] backend gpu-%d responded %d (content-type: %s)", backend.GPUID, resp.StatusCode, resp.Header.Get("Content-Type"))
	defer resp.Body.Close()

	for key, values := range resp.Header {
		if hopByHopHeaders[http.CanonicalHeaderKey(key)] {
			continue
		}
		if thinkDisabled && strings.EqualFold(key, "Content-Length") {
			continue // body will be rewritten, original length is wrong
		}
		for _, v := range values { w.Header().Add(key, v) }
	}
	w.Header().Set("X-GPU-Backend", fmt.Sprintf("gpu-%d", backend.GPUID))
	w.Header().Set("X-Queue-Depth", fmt.Sprintf("%d", backend.InFlight()))

	// When think is disabled, rewrite the response to move reasoning_content
	// into content with <think> blocks stripped.
	if thinkDisabled {
		if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
			w.WriteHeader(resp.StatusCode)
			clientAborted = streamThinkDisabled(w, resp.Body, cancel)
			if clientAborted {
				log.Printf("[debug] gpu-%d stream: client aborted (think-disabled path)", backend.GPUID)
			}
		} else {
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				log.Printf("[debug] gpu-%d non-streaming read error: %v", backend.GPUID, err)
				http.Error(w, "backend read error", http.StatusBadGateway)
				return
			}
			rewritten := rewriteThinkResponse(body)
			w.Header().Set("Content-Length", strconv.Itoa(len(rewritten)))
			w.WriteHeader(resp.StatusCode)
			w.Write(rewritten)
		}
		return
	}

	w.WriteHeader(resp.StatusCode)

	if f, ok := w.(http.Flusher); ok {
		buf := make([]byte, 4096)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				if _, writeErr := w.Write(buf[:n]); writeErr != nil {
					log.Printf("[debug] gpu-%d stream: client write error (aborting): %v", backend.GPUID, writeErr)
					cancel() // client disconnected — kill backend request immediately
					clientAborted = true
					break
				}
				f.Flush()
			}
			if readErr != nil {
				if readErr != io.EOF {
					log.Printf("[debug] gpu-%d stream: backend read error: %v", backend.GPUID, readErr)
				}
				break
			}
		}
	} else {
		io.Copy(w, resp.Body)
	}
	return
}
