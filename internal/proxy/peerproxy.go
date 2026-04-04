package proxy

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

const HeaderForwarded = "X-Viiwork-Forwarded"

var peerClient = &http.Client{Timeout: 120 * time.Second}

func proxyToPeer(w http.ResponseWriter, r *http.Request, peerAddr string, nodeID string) {
	targetURL := fmt.Sprintf("http://%s%s", peerAddr, r.URL.Path)
	if r.URL.RawQuery != "" { targetURL += "?" + r.URL.RawQuery }

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, `{"error":{"message":"proxy error","type":"server_error"}}`, http.StatusBadGateway)
		return
	}
	for key, values := range r.Header {
		for _, v := range values { proxyReq.Header.Add(key, v) }
	}
	proxyReq.Header.Set(HeaderForwarded, nodeID)

	resp, err := peerClient.Do(proxyReq)
	if err != nil {
		http.Error(w, `{"error":{"message":"peer unavailable","type":"server_error"}}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for key, values := range resp.Header {
		for _, v := range values { w.Header().Add(key, v) }
	}
	w.Header().Set("X-Viiwork-Origin", peerAddr)
	w.WriteHeader(resp.StatusCode)

	if f, ok := w.(http.Flusher); ok {
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 { w.Write(buf[:n]); f.Flush() }
			if err != nil { break }
		}
	} else {
		io.Copy(w, resp.Body)
	}
}
