package alias

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/janit/viiwork/v2/internal/meshauth"
	"github.com/janit/viiwork/v2/meshapi"
)

// maxWriteBody bounds an alias write request's body.
const maxWriteBody = 64 << 10

// Authorizer decides who may write aliases.
//
// In a secured mesh a write must carry a meshauth signature made with the mesh
// secret (or the previous one, during rotation) and a fresh nonce. In an open
// mesh a write is accepted only from a loopback address, so only someone on
// the node's own machine can change the mesh's aliases (Decision 16).
//
// **A reverse proxy on the same machine in front of the node's API defeats the
// open-mesh rule**: every request it forwards arrives from loopback. Put such a
// proxy only in front of a secured mesh, or have it refuse alias writes.
type Authorizer struct {
	primary  *meshauth.Signer // nil = open mesh
	previous *meshauth.Signer
	nonces   *meshauth.NonceCache
}

// NewAuthorizer builds the authorizer; a nil primary secret is an open mesh.
func NewAuthorizer(self string, primary, previous []byte) (*Authorizer, error) {
	a := &Authorizer{}
	if primary == nil {
		return a, nil
	}
	var err error
	if a.primary, err = meshauth.NewSigner(primary, self); err != nil {
		return nil, err
	}
	if previous != nil {
		if a.previous, err = meshauth.NewSigner(previous, self); err != nil {
			return nil, err
		}
	}
	a.nonces = meshauth.NewNonceCache(2 * meshauth.SkewWindow)
	return a, nil
}

// Authorize reports whether r may write, and otherwise the status and message
// to answer with.
func (a *Authorizer) Authorize(r *http.Request, body []byte) (status int, message string, ok bool) {
	if a.primary != nil {
		nonce, _, err := a.primary.VerifyRequest(r, body)
		if err != nil && a.previous != nil && !errors.Is(err, meshauth.ErrNoProof) {
			nonce, _, err = a.previous.VerifyRequest(r, body)
		}
		if err != nil || !a.nonces.Use(nonce) {
			return http.StatusUnauthorized, "alias writes need a valid X-Viiwork-Auth signature (sign with the mesh secret)", false
		}
		return 0, "", true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if addr, err := netip.ParseAddr(host); err == nil && addr.Unmap().IsLoopback() {
		return 0, "", true
	}
	return http.StatusForbidden, "alias writes are accepted only from this machine in an open mesh", false
}

type apiHandler struct {
	svc  *Service
	auth *Authorizer
}

// NewHandler serves GET /v1/aliases and the write endpoints under it.
func NewHandler(svc *Service, auth *Authorizer) http.Handler {
	return &apiHandler{svc: svc, auth: auth}
}

func (h *apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	if path == meshapi.PathAliases {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
			return
		}
		if r.URL.Query().Get("table") == "1" {
			writeJSON(w, http.StatusOK, h.svc.Store().Table())
			return
		}
		writeJSON(w, http.StatusOK, meshapi.AliasesResponse{Aliases: h.svc.Resolver().Info()})
		return
	}

	rest, ok := strings.CutPrefix(path, meshapi.PathAliases+"/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Split the escaped path, so an encoded slash stays inside the name.
	segments := strings.Split(rest, "/")
	name, err := url.PathUnescape(segments[0])
	if err != nil || segments[0] == "" {
		http.NotFound(w, r)
		return
	}
	var write func(name string, body []byte) (meshapi.AliasBroadcast, error)
	switch {
	case len(segments) == 1 && r.Method == http.MethodPut:
		write = h.set
	case len(segments) == 1 && r.Method == http.MethodDelete:
		write = func(name string, _ []byte) (meshapi.AliasBroadcast, error) { return h.svc.Delete(name) }
	case len(segments) == 2 && "/"+segments[1] == meshapi.AliasRevertSuffix && r.Method == http.MethodPost:
		write = func(name string, _ []byte) (meshapi.AliasBroadcast, error) { return h.svc.Revert(name) }
	case len(segments) == 1, len(segments) == 2 && "/"+segments[1] == meshapi.AliasRevertSuffix:
		writeError(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
		return
	default:
		http.NotFound(w, r)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWriteBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request", "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "failed to read request")
		return
	}
	// Authorisation comes first, so an unauthorised caller learns nothing
	// about the table.
	if status, msg, ok := h.auth.Authorize(r, body); !ok {
		writeError(w, status, "invalid_request", msg)
		return
	}
	b, err := write(name, body)
	if err != nil {
		var we *WriteError
		var bad *badRequest
		switch {
		case errors.As(err, &bad):
			writeError(w, http.StatusBadRequest, "invalid_request", bad.msg)
		case errors.As(err, &we) && we.Status == http.StatusNotFound:
			writeError(w, we.Status, "not_found", we.Message)
		case errors.As(err, &we):
			writeError(w, we.Status, "invalid_request", we.Message)
		default:
			writeError(w, http.StatusInternalServerError, "server_error", fmt.Sprintf("alias write failed: %v", err))
		}
		return
	}
	writeJSON(w, http.StatusOK, b)
}

type badRequest struct{ msg string }

func (e *badRequest) Error() string { return e.msg }

func (h *apiHandler) set(name string, body []byte) (meshapi.AliasBroadcast, error) {
	var req meshapi.AliasWriteRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return meshapi.AliasBroadcast{}, &badRequest{msg: fmt.Sprintf("invalid alias write: %v", err)}
	}
	return h.svc.Set(name, req)
}

func writeError(w http.ResponseWriter, status int, typ, message string) {
	writeJSON(w, status, meshapi.ErrorResponse{Error: meshapi.ErrorBody{Message: message, Type: typ}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
