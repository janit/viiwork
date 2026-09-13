// Package api is viiwork's client-facing API seam, spec contract C8.
//
// viiwork is a melting pot with two rims: inference engines on the backend
// side (internal/engine, C7) and consumer APIs on this one. The
// OpenAI-compatible API is the NATIVE one — every engine viiwork supervises
// speaks it, the mesh forwards it between nodes, and the dashboards consume it.
// Nothing here changes a byte of that.
//
// This package is deliberately a seam and not a framework. There is no second
// dialect, none is planned, and the seam is worth having precisely because that
// may stay true: the cost of adding one later is not writing the handler, it is
// finding every place the first one was assumed. That place is now one registry
// lookup.
//
// A dialect lives AT THE EDGE and only there:
//
//	client --(any dialect)--> origin node --(OpenAI)--> local backend
//	                                      --(OpenAI)--> peer node --> its backend
//
// Everything past decode — routing, the origin queue, peer forwarding,
// meshapi's paths and headers — is OpenAI and stays OpenAI. That is what lets
// the client rim move while C4 and C5 stay frozen: a v2.1.0 node and a node
// speaking some future dialect still forward to each other identically,
// because neither ever forwards a dialect.
package api

import (
	"bytes"
	"fmt"
	"net/http"
	"sort"
	"sync"
)

// Request is what routing needs, and the whole of what a dialect must produce.
type Request struct {
	// Model is the name the client asked for. Alias resolution happens after
	// decoding, on the origin node, and is not a dialect's business.
	Model string
	// Body is what gets sent to the engine: the original bytes when the
	// dialect is native, an OpenAI-shaped translation otherwise.
	Body []byte
	// Task is the optional request tag, from the body or the X-Viiwork-Task
	// header, already sanitised.
	Task string
	// Think carries the viiwork "think" extension: nil when the client said
	// nothing. It is a property of the request shape, so it is the dialect's
	// to read.
	Think *bool
}

// Dialect is one client-facing API shape.
type Dialect interface {
	// Name is the dialect's name, for logging.
	Name() string
	// Paths are the request paths this dialect owns. Registration panics on a
	// collision with another dialect, so two cannot silently share one.
	Paths() []string
	// Decode extracts what the node needs in order to route. native reports
	// that the body is already OpenAI-shaped and may be forwarded unchanged —
	// the pass-through case, and the only one on the hot path today.
	Decode(r *http.Request, body []byte) (req Request, native bool, err error)
}

// ResponseTranslator is implemented by a dialect whose clients expect a shape
// other than the engine's. A dialect that does not implement it gets byte
// pass-through, which is what the native path must always remain.
//
// TranslateStream is called per SSE frame and is therefore ON THE PER-TOKEN
// PATH. An implementation that allocates per frame shows up in
// `go test -bench=. -benchmem ./internal/proxy` before it shows up in a
// complaint, which is why that benchmark is a release gate.
type ResponseTranslator interface {
	TranslateStream(frame []byte, dst *bytes.Buffer) error
	TranslateWhole(body []byte) ([]byte, error)
}

var (
	mu       sync.RWMutex
	byPath   = map[string]Dialect{}
	byName   = map[string]Dialect{}
	registry []Dialect
)

// Register makes a dialect serve its paths. An empty name, no paths, or a path
// another dialect already owns is a programming error, so it panics at init
// rather than resolving to whichever package happened to load first.
func Register(d Dialect) {
	name := d.Name()
	if name == "" {
		panic("api: Register with empty name")
	}
	paths := d.Paths()
	if len(paths) == 0 {
		panic("api: " + name + " registers no paths")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := byName[name]; dup {
		panic(fmt.Sprintf("api: dialect %q registered twice", name))
	}
	for _, p := range paths {
		if other, dup := byPath[p]; dup {
			panic(fmt.Sprintf("api: %q is already served by dialect %q", p, other.Name()))
		}
	}
	byName[name] = d
	for _, p := range paths {
		byPath[p] = d
	}
	registry = append(registry, d)
}

// Lookup returns the dialect that owns a request path.
func Lookup(path string) (Dialect, bool) {
	mu.RLock()
	defer mu.RUnlock()
	d, ok := byPath[path]
	return d, ok
}

// Names returns every registered dialect name, sorted.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(byName))
	for n := range byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
