package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/janit/viiwork/v2/internal/route"
	"github.com/janit/viiwork/v2/meshapi"
)

type outcome int

const (
	outcomeServed    outcome = iota // a response (any status) went to the client, possibly cut short
	outcomeRetryable                // nothing was written to the client; another route may serve it
)

type execResult struct {
	Outcome outcome
	Status  int    // status written to the client when served
	Aborted bool   // the client went away mid-response
	Reason  string // why it is retryable
}

func retryable(format string, args ...any) execResult {
	return execResult{Outcome: outcomeRetryable, Reason: fmt.Sprintf(format, args...)}
}

// isHardSocketFailure reports whether err from a backend request means the
// backend's listener is gone: EOF before a response header (the process closed
// the connection mid-request), or refused/reset from the dialer (the port is no
// longer bound). These are kernel-level signals the inference path acts on at
// once without waiting for /health. Context errors are excluded, so a client
// cancelling does not evict a healthy backend. Copied from v1.
func isHardSocketFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return false
}

// hopByHopHeaders are HTTP/1.1 headers a proxy must not forward (RFC 7230).
// Copied from v1.
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

// backendClient has no timeout: inference streams for minutes, and the
// client's request context controls cancellation. Copied from v1.
var backendClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	},
}

// peerDialTimeout bounds only the handshake to a peer. A powered-off host drops
// packets rather than refusing, so a dial would otherwise run to the kernel's
// connect timeout (v1.8.1).
const peerDialTimeout = 5 * time.Second

// peerClient has no overall timeout (Decision 10): v1's 120 s cap cut off long
// generations, and a non-streaming completion withholds its headers until the
// generation ends. Cancellation comes from the client's context. Mesh traffic
// ignores proxy variables, so one inherited by a container cannot route
// tailnet traffic elsewhere.
var peerClient = &http.Client{
	Transport: &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: peerDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	},
}

func targetURL(addr string, r *http.Request) string {
	u := "http://" + addr + r.URL.Path
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	return u
}

func copyRequestHeaders(dst, src http.Header) {
	for key, values := range src {
		if hopByHopHeaders[http.CanonicalHeaderKey(key)] {
			continue
		}
		for _, v := range values {
			dst.Add(key, v)
		}
	}
}

// serveLocal runs the request on a local backend. Nothing is written to the
// client unless a response the client should see arrived: a transport failure
// or a 429/503 from the engine is retryable, and a hard socket failure also
// tells the backend's supervision loop to probe at once.
func serveLocal(w http.ResponseWriter, r *http.Request, body []byte, b route.LocalBackend, model, self string, thinkDisabled bool) execResult {
	addr := b.Addr()
	if addr == "" {
		return retryable("backend %s has no running process", b.ID())
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, r.Method, targetURL(addr, r), bytes.NewReader(body))
	if err != nil {
		return retryable("backend %s: %v", b.ID(), err)
	}
	copyRequestHeaders(req.Header, r.Header)
	req.ContentLength = int64(len(body))

	resp, err := backendClient.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			return execResult{Outcome: outcomeServed, Aborted: true}
		}
		if isHardSocketFailure(err) {
			b.NoteHardFailure()
		}
		return retryable("backend %s: %v", b.ID(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		_, _ = io.Copy(io.Discard, resp.Body)
		return retryable("backend %s answered %d", b.ID(), resp.StatusCode)
	}

	sse := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	var rewritten []byte
	if thinkDisabled && !sse {
		// Read before touching the client's headers: a body cut off here is
		// retryable, and the next attempt must not inherit this one's headers.
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return retryable("backend %s: reading the response: %v", b.ID(), err)
		}
		rewritten = rewriteThinkResponse(raw)
	}

	for key, values := range resp.Header {
		if hopByHopHeaders[http.CanonicalHeaderKey(key)] || (thinkDisabled && strings.EqualFold(key, "Content-Length")) {
			continue
		}
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.Header().Set(meshapi.HeaderNode, self)
	w.Header().Set(meshapi.HeaderGPUBackend, b.ID())
	w.Header().Set(meshapi.HeaderModel, model)

	res := execResult{Outcome: outcomeServed, Status: resp.StatusCode}
	if thinkDisabled {
		if sse {
			w.WriteHeader(resp.StatusCode)
			res.Aborted = streamThinkDisabled(w, resp.Body, cancel) || r.Context().Err() != nil
			return res
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(rewritten)))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(rewritten)
		return res
	}
	w.WriteHeader(resp.StatusCode)
	res.Aborted = stream(w, resp.Body, cancel) || r.Context().Err() != nil
	return res
}

// forwardToPeer runs the request on a peer. The body is passed explicitly
// because the forward signature covers its digest. The peer's response goes to
// the client byte for byte: thinking is rewritten once, on the executing node
// (Decision 9).
func forwardToPeer(w http.ResponseWriter, r *http.Request, body []byte, t route.Target, auth *ForwardAuth, self string) execResult {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL(t.Addr, r), bytes.NewReader(body))
	if err != nil {
		return retryable("peer %s: %v", t.Node, err)
	}
	copyRequestHeaders(req.Header, r.Header)
	req.ContentLength = int64(len(body))
	if err := auth.Sign(req, body); err != nil { // last: the signature covers the request
		return retryable("peer %s: signing the forward: %v", t.Node, err)
	}

	resp, err := peerClient.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			return execResult{Outcome: outcomeServed, Aborted: true}
		}
		return retryable("peer %s: %v", t.Node, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		_, _ = io.Copy(io.Discard, resp.Body)
		return retryable("peer %s answered %d", t.Node, resp.StatusCode)
	}

	for key, values := range resp.Header {
		if hopByHopHeaders[http.CanonicalHeaderKey(key)] {
			continue
		}
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.Header().Set(meshapi.HeaderOrigin, self)
	if w.Header().Get(meshapi.HeaderNode) == "" {
		w.Header().Set(meshapi.HeaderNode, t.Node)
	}
	w.WriteHeader(resp.StatusCode)
	// A failure after this point ends the response as it stands: the peer may
	// already be generating, so it is never retried.
	aborted := stream(w, resp.Body, func() {}) || r.Context().Err() != nil
	return execResult{Outcome: outcomeServed, Status: resp.StatusCode, Aborted: aborted}
}

// stream copies body to w, flushing after each read. A client write error
// cancels the upstream request and reports the client gone.
func stream(w http.ResponseWriter, body io.Reader, cancel func()) (clientAborted bool) {
	f, ok := w.(http.Flusher)
	if !ok {
		_, _ = io.Copy(w, body)
		return false
	}
	buf := make([]byte, 4096)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				cancel()
				return true
			}
			f.Flush()
		}
		if readErr != nil {
			return false
		}
	}
}
