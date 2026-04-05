package proxy

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const HeaderForwarded = "X-Viiwork-Forwarded"

var peerClient = &http.Client{Timeout: 120 * time.Second}

func proxyToPeer(w http.ResponseWriter, r *http.Request, peerAddr string, nodeID string, thinkDisabled bool) {
	targetURL := fmt.Sprintf("http://%s%s", peerAddr, r.URL.Path)
	if r.URL.RawQuery != "" { targetURL += "?" + r.URL.RawQuery }

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		log.Printf("[debug] peer proxy request creation failed for %s: %v", peerAddr, err)
		http.Error(w, `{"error":{"message":"proxy error","type":"server_error"}}`, http.StatusBadGateway)
		return
	}
	for key, values := range r.Header {
		for _, v := range values { proxyReq.Header.Add(key, v) }
	}
	proxyReq.Header.Set(HeaderForwarded, nodeID)

	resp, err := peerClient.Do(proxyReq)
	if err != nil {
		log.Printf("[debug] peer %s unavailable: %v", peerAddr, err)
		http.Error(w, `{"error":{"message":"peer unavailable","type":"server_error"}}`, http.StatusBadGateway)
		return
	}
	log.Printf("[debug] peer %s responded %d", peerAddr, resp.StatusCode)
	defer resp.Body.Close()

	for key, values := range resp.Header {
		if thinkDisabled && strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, v := range values { w.Header().Add(key, v) }
	}
	w.Header().Set("X-Viiwork-Origin", peerAddr)

	if thinkDisabled {
		if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
			w.WriteHeader(resp.StatusCode)
			streamThinkDisabled(w, resp.Body, func() {})
		} else {
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				http.Error(w, `{"error":{"message":"peer read error","type":"server_error"}}`, http.StatusBadGateway)
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
					log.Printf("[debug] peer %s stream: client write error: %v", peerAddr, writeErr)
					break
				}
				f.Flush()
			}
			if readErr != nil {
				if readErr != io.EOF {
					log.Printf("[debug] peer %s stream: read error: %v", peerAddr, readErr)
				}
				break
			}
		}
	} else {
		io.Copy(w, resp.Body)
	}
}
