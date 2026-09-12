package proxy

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/janit/viiwork/v2/internal/meshauth"
	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/meshapi"
)

// rejectLogEvery rate-limits the log line for unproven forward claims per
// claimed origin.
const rejectLogEvery = time.Minute

// Members is the member list; *mesh.Mesh satisfies it.
type Members interface {
	Members() []mesh.Member
}

// ForwardAuth marks and proves mesh forwards (spec C5). A forward gets strict
// admission on the receiver (a free slot now or 429, never the queue), so an
// ordinary client must not be able to claim to be one.
type ForwardAuth struct {
	self     string
	primary  *meshauth.Signer // nil = open mesh
	previous *meshauth.Signer
	nonces   *meshauth.NonceCache
	members  Members
	logf     func(format string, args ...any)
	now      func() time.Time

	mu      sync.Mutex
	lastLog map[string]time.Time
}

// NewForwardAuth builds the authenticator. A nil primary is an open mesh.
func NewForwardAuth(self string, primary, previous []byte, members Members) (*ForwardAuth, error) {
	a := &ForwardAuth{self: self, members: members, logf: log.Printf, now: time.Now, lastLog: map[string]time.Time{}}
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
	// Nonces live for two skew windows: anything older fails the skew check
	// before it could be replayed.
	a.nonces = meshauth.NewNonceCache(2 * meshauth.SkewWindow)
	return a, nil
}

// Sign marks req as a forward from this node and, in a secured mesh, signs it.
// It must be the last change to the request: the signature covers the method,
// the path with its query, and a digest of the body.
func (a *ForwardAuth) Sign(req *http.Request, body []byte) error {
	req.Header.Set(meshapi.HeaderForwarded, a.self)
	if a.primary == nil {
		return nil
	}
	_, err := a.primary.SignRequest(req, body)
	return err
}

// Verify reports whether r is a genuine forward, and from which node.
//
// In a secured mesh the signature must verify with the current secret or the
// previous one (key rotation without downtime), the signer must be the claimed
// origin (a valid signature from one member must not let it impersonate
// another), and the nonce must be fresh. In an open mesh the request must come
// from the address of an alive member of that name, other than this node.
func (a *ForwardAuth) Verify(r *http.Request, body []byte) (origin string, ok bool) {
	origin = r.Header.Get(meshapi.HeaderForwarded)
	if origin == "" {
		return "", false
	}
	var reason error
	if a.primary != nil {
		reason = a.verifySigned(r, body, origin)
	} else {
		reason = a.verifySource(r, origin)
	}
	if reason != nil {
		a.noteRejection(origin, reason)
		return "", false
	}
	return origin, true
}

func (a *ForwardAuth) verifySigned(r *http.Request, body []byte, origin string) error {
	nonce, signer, err := a.primary.VerifyRequest(r, body)
	if err != nil && a.previous != nil && !errors.Is(err, meshauth.ErrNoProof) {
		nonce, signer, err = a.previous.VerifyRequest(r, body)
	}
	switch {
	case err != nil:
		return err
	case signer != origin:
		return fmt.Errorf("signed by %s", signer)
	case !a.nonces.Use(nonce):
		return errors.New("replayed nonce")
	}
	return nil
}

func (a *ForwardAuth) verifySource(r *http.Request, origin string) error {
	if origin == a.self {
		return errors.New("it names this node")
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	src, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("source address %q", r.RemoteAddr)
	}
	for _, m := range a.members.Members() {
		if m.Name == origin && !m.Local && m.State == meshapi.MemberAlive && m.Addr == src.Unmap() {
			return nil
		}
	}
	return fmt.Errorf("source %s is not the alive member %s", src, origin)
}

func (a *ForwardAuth) noteRejection(origin string, reason error) {
	now := a.now()
	a.mu.Lock()
	last, seen := a.lastLog[origin]
	if seen && now.Sub(last) < rejectLogEvery {
		a.mu.Unlock()
		return
	}
	a.lastLog[origin] = now
	a.mu.Unlock()
	a.logf("ignoring an unproven forward claiming to be %s: %v", origin, reason)
}

// stripMeshHeaders removes the forward claim and the meshauth headers, and
// nothing else. The handler calls it on every request right after Verify, so
// an ordinary client cannot smuggle a claim through to a peer, and engines
// never see mesh headers (Decision 15).
func stripMeshHeaders(h http.Header) {
	for _, k := range []string{meshapi.HeaderForwarded, meshauth.HeaderNode, meshauth.HeaderTs, meshauth.HeaderNonce, meshauth.HeaderAuth} {
		h.Del(k)
	}
}
