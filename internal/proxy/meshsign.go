package proxy

import (
	"bytes"
	"errors"
	"net/http"

	"github.com/janit/viiwork/internal/meshauth"
)

// signedJSON wraps a small JSON endpoint so a caller that proves mesh
// membership gets a signed answer, and everyone else gets exactly what they
// get today.
//
// Three outcomes, and the middle one is the point of the whole design:
//
//   - No proof offered: pass through untouched. This is the browser
//     (dashboard.html fetches /v1/cluster directly) and the gateway. Reads
//     are open and stay open.
//   - Proof offered and valid: the response is buffered, signed over the
//     caller's nonce and the body digest, and sent.
//   - Proof offered and invalid: 401. A caller that tried to prove membership
//     and failed is not the same as one that never tried, and must not be
//     quietly served as though it were.
//
// Buffering the body is acceptable here and only here: /v1/status and
// /v1/cluster are a few KB. Never wrap a stream with this.
func signedJSON(next http.Handler, signer *meshauth.Signer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if signer == nil {
			next.ServeHTTP(w, r)
			return
		}
		nonce, _, err := signer.VerifyRequest(r, nil)
		switch {
		case errors.Is(err, meshauth.ErrNoProof):
			next.ServeHTTP(w, r)
			return
		case err != nil:
			http.Error(w, `{"error":{"message":"mesh proof rejected","type":"unauthorized"}}`, http.StatusUnauthorized)
			return
		}

		rec := &bufferedWriter{header: http.Header{}, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		body := rec.buf.Bytes()
		for k, vs := range rec.header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		signer.SignResponse(w.Header(), r.URL.RequestURI(), nonce, body)
		w.WriteHeader(rec.status)
		w.Write(body)
	})
}

// bufferedWriter collects a handler's output so it can be signed before any
// of it is written. Deliberately not a http.Flusher: a handler that flushes
// is a stream, and a stream must never be routed through here.
type bufferedWriter struct {
	header http.Header
	buf    bytes.Buffer
	status int
}

func (b *bufferedWriter) Header() http.Header         { return b.header }
func (b *bufferedWriter) Write(p []byte) (int, error) { return b.buf.Write(p) }
func (b *bufferedWriter) WriteHeader(status int)      { b.status = status }
