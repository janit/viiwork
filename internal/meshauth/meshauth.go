// Package meshauth is the membership proof that makes a node a member of the
// mesh rather than merely something at a reachable address.
//
// The mesh authenticates callers, not endpoints: /v1/status and /v1/cluster
// answer anyone, exactly as they always have, because the dashboards fetch
// them from a browser and EventSource cannot set a header. What a proof buys
// is standing — only a signed response can cause an address to be adopted,
// routed to, or advertised onward. Everything else about the mesh is
// unchanged for a caller that does not sign.
package meshauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	HeaderNode  = "X-Viiwork-Node"
	HeaderTs    = "X-Viiwork-Ts"
	HeaderNonce = "X-Viiwork-Nonce"
	HeaderAuth  = "X-Viiwork-Auth"

	// SkewWindow bounds both clock disagreement and replay. Fleet nodes are
	// NTP-synced tailnet machines, so two minutes is generous for the first
	// and tight for the second.
	SkewWindow = 120 * time.Second

	NonceBytes = 16

	// MinSecretLen mirrors the gateway's posture on a short API key: a weak
	// mesh secret is a misconfiguration to fix, not a state to run in.
	MinSecretLen = 32

	// scheme prefixes every MAC. It exists so a future scheme can be
	// introduced without either end of a mixed-version wire guessing which
	// one it is looking at; an unknown prefix is a failure, never a fallback.
	scheme = "v1"
)

var (
	ErrNoProof  = errors.New("meshauth: no proof present")
	ErrBadProof = errors.New("meshauth: proof did not verify")
)

// canonicalRequest is what a caller signs. Body digest is the empty string
// for a bodyless request, which is why the vector ends in a bare newline.
func canonicalRequest(method, pathQuery, ts, nonce, nodeID, bodyHex string) string {
	return strings.Join([]string{scheme, "req", method, pathQuery, ts, nonce, nodeID, bodyHex}, "\n")
}

// canonicalResponse is what a responder signs. It covers the caller's nonce,
// which is what makes it proof of liveness rather than a replayable
// recording, and the body digest, which makes the peer list it carries
// tamper-evident.
func canonicalResponse(pathQuery, ts, nonce, nodeID, bodyHex string) string {
	return strings.Join([]string{scheme, "resp", pathQuery, ts, nonce, nodeID, bodyHex}, "\n")
}

func bodyDigest(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Signer both proves this node's membership and checks others'. One type for
// both directions because a mesh member is always both ends of some call.
type Signer struct {
	secret []byte
	nodeID string
	now    func() time.Time // swappable for skew tests
}

func NewSigner(secret []byte, nodeID string) (*Signer, error) {
	if len(secret) < MinSecretLen {
		return nil, fmt.Errorf("meshauth: secret is %d bytes, need at least %d", len(secret), MinSecretLen)
	}
	if nodeID == "" {
		return nil, errors.New("meshauth: empty node id")
	}
	return &Signer{secret: secret, nodeID: nodeID, now: time.Now}, nil
}

func (s *Signer) mac(canonical string) string {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(canonical))
	return scheme + "=" + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Note on what is signed: req.URL.RequestURI() (path plus query), never the
// whole URL. The caller knows the host it dialled and the responder knows
// only what arrived; scheme, Host and any proxying make those two strings
// differ in ways that would break verification for no security gain.

func (s *Signer) SignRequest(req *http.Request, body []byte) (string, error) {
	raw := make([]byte, NonceBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("meshauth: nonce: %w", err)
	}
	nonce := base64.RawURLEncoding.EncodeToString(raw)
	ts := strconv.FormatInt(s.now().Unix(), 10)
	canonical := canonicalRequest(req.Method, req.URL.RequestURI(), ts, nonce, s.nodeID, bodyDigest(body))

	req.Header.Set(HeaderNode, s.nodeID)
	req.Header.Set(HeaderTs, ts)
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderAuth, s.mac(canonical))
	return nonce, nil
}

// VerifyRequest returns ErrNoProof when the caller did not attempt one at all
// — an ordinary browser or the gateway — which callers must be able to tell
// apart from ErrBadProof, a caller that tried and failed.
func (s *Signer) VerifyRequest(r *http.Request, body []byte) (string, string, error) {
	auth := r.Header.Get(HeaderAuth)
	nonce := r.Header.Get(HeaderNonce)
	ts := r.Header.Get(HeaderTs)
	node := r.Header.Get(HeaderNode)
	if auth == "" && nonce == "" && ts == "" && node == "" {
		return "", "", ErrNoProof
	}
	if err := s.checkSkew(ts); err != nil {
		return "", "", err
	}
	if err := checkNonce(nonce); err != nil {
		return "", "", err
	}
	if node == "" {
		return "", "", ErrBadProof
	}
	want := s.mac(canonicalRequest(r.Method, r.URL.RequestURI(), ts, nonce, node, bodyDigest(body)))
	if !hmac.Equal([]byte(auth), []byte(want)) {
		return "", "", ErrBadProof
	}
	return nonce, node, nil
}

func (s *Signer) SignResponse(h http.Header, pq, nonce string, body []byte) {
	ts := strconv.FormatInt(s.now().Unix(), 10)
	h.Set(HeaderNode, s.nodeID)
	h.Set(HeaderTs, ts)
	h.Set(HeaderAuth, s.mac(canonicalResponse(pq, ts, nonce, s.nodeID, bodyDigest(body))))
}

func (s *Signer) VerifyResponse(h http.Header, pq, nonce string, body []byte) error {
	auth := h.Get(HeaderAuth)
	ts := h.Get(HeaderTs)
	node := h.Get(HeaderNode)
	if auth == "" && ts == "" && node == "" {
		return ErrNoProof
	}
	if err := s.checkSkew(ts); err != nil {
		return err
	}
	if node == "" {
		return ErrBadProof
	}
	want := s.mac(canonicalResponse(pq, ts, nonce, node, bodyDigest(body)))
	if !hmac.Equal([]byte(auth), []byte(want)) {
		return ErrBadProof
	}
	return nil
}

func (s *Signer) checkSkew(ts string) error {
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrBadProof
	}
	d := s.now().Sub(time.Unix(secs, 0))
	if d < 0 {
		d = -d
	}
	if d > SkewWindow {
		return ErrBadProof
	}
	return nil
}

func checkNonce(nonce string) error {
	raw, err := base64.RawURLEncoding.DecodeString(nonce)
	if err != nil || len(raw) != NonceBytes {
		return ErrBadProof
	}
	return nil
}
