package proxy

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/janit/viiwork/internal/meshauth"
)

const HeaderForwarded = "X-Viiwork-Forwarded"

// peerDialTimeout bounds only the TCP handshake to a peer, never the request.
//
// A peer whose host has been powered off does not refuse the connection, it
// drops the packets, so a dial against it runs to the kernel's own connect
// timeout — over two minutes on Linux. Until the poll loop notices the peer
// is gone (at worst one poll_interval plus one peers.timeout), routes to it
// are still handed out, and every request that took one used to sit here for
// the whole of peerClient's 120s budget. A peer that cannot complete a
// handshake in this long over a LAN or tailnet is not going to serve an
// inference either.
const peerDialTimeout = 5 * time.Second

// peerClient keeps a long overall timeout on purpose: a forwarded completion
// streams for as long as the generation takes. Only the dial is bounded, so
// an unreachable peer fails fast while a slow one is left alone. There is no
// response-header timeout, also on purpose: a non-streaming completion
// legitimately withholds its headers until the whole generation is done, so
// a bound short enough to matter would cut off exactly the long requests.
var peerClient = newPeerClient(peerDialTimeout)

func newPeerClient(dialTimeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			// Otherwise a mirror of http.DefaultTransport, which this client
			// used until the dial needed bounding, so proxy environment and
			// connection reuse behave as they did. MaxIdleConnsPerHost is
			// raised from the default 2: forwards fan out to a handful of
			// peers, and the default closed every connection past the second
			// after each use.
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   dialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// proxyToPeer takes the body explicitly rather than streaming r.Body: the
// membership proof covers a digest, so the exact bytes have to be known, and
// the caller already has them buffered.
func proxyToPeer(w http.ResponseWriter, r *http.Request, peerAddr string, nodeID string, thinkDisabled bool, body []byte, signer *meshauth.Signer) {
	targetURL := fmt.Sprintf("http://%s%s", peerAddr, r.URL.Path)
	if r.URL.RawQuery != "" { targetURL += "?" + r.URL.RawQuery }

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("[debug] peer proxy request creation failed for %s: %v", peerAddr, err)
		http.Error(w, `{"error":{"message":"proxy error","type":"server_error"}}`, http.StatusBadGateway)
		return
	}
	for key, values := range r.Header {
		for _, v := range values { proxyReq.Header.Add(key, v) }
	}
	proxyReq.Header.Set(HeaderForwarded, nodeID)
	proxyReq.ContentLength = int64(len(body))
	// Signed last: SignRequest covers the method, path and body digest, and
	// must be computed after every other header is in place.
	if signer != nil {
		if _, err := signer.SignRequest(proxyReq, body); err != nil {
			log.Printf("[debug] could not sign forward to %s: %v", peerAddr, err)
		}
	}

	resp, err := peerClient.Do(proxyReq)
	if err != nil {
		log.Printf("[debug] peer %s unavailable: %v", peerAddr, err)
		http.Error(w, `{"error":{"message":"peer unavailable","type":"server_error"}}`, http.StatusBadGateway)
		return
	}
	log.Printf("[debug] peer %s responded %d", peerAddr, resp.StatusCode)
	defer resp.Body.Close()

	for key, values := range resp.Header {
		if hopByHopHeaders[http.CanonicalHeaderKey(key)] {
			continue
		}
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
