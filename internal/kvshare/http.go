package kvshare

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

// dialTimeout bounds the handshake to a receiver.
const dialTimeout = 5 * time.Second

func init() { Register("http", func() Transport { return NewHTTPTransport(HTTPOptions{}) }) }

// StatusError is an error with the HTTP status the handler answers it with.
// Any other error is a 400.
type StatusError struct {
	Code int
	Err  error
}

func (e *StatusError) Error() string { return e.Err.Error() }
func (e *StatusError) Unwrap() error { return e.Err }

// Statusf builds a StatusError.
func Statusf(code int, format string, args ...any) error {
	return &StatusError{Code: code, Err: fmt.Errorf(format, args...)}
}

// HTTPHandler serves a Handler on meshapi.PathKVShareManifest (GET ?key=) and
// meshapi.PathKVSharePut (PUT ?key=, the delta as the body). The same handler
// runs in viiwork-kvsync and, behind the mesh's authorisation, on a node's
// API port. A manifest of nothing is a 200 with null: a receiver that holds
// no replica is not an error.
func HTTPHandler(h Handler) http.Handler {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any, err error) {
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			code := http.StatusBadRequest
			var se *StatusError
			if errors.As(err, &se) {
				code = se.Code
			}
			w.WriteHeader(code)
			v = map[string]string{"error": err.Error()}
		}
		_ = json.NewEncoder(w).Encode(v)
	}
	key := func(r *http.Request) (string, error) {
		k := r.URL.Query().Get("key")
		if !ValidKey(k) {
			return "", fmt.Errorf("kvshare: invalid key %q", k)
		}
		return k, nil
	}
	mux.HandleFunc("GET "+meshapi.PathKVShareManifest, func(w http.ResponseWriter, r *http.Request) {
		k, err := key(r)
		if err != nil {
			reply(w, nil, err)
			return
		}
		m, err := h.Manifest(r.Context(), k)
		reply(w, m, err)
	})
	mux.HandleFunc("PUT "+meshapi.PathKVSharePut, func(w http.ResponseWriter, r *http.Request) {
		k, err := key(r)
		if err != nil {
			reply(w, nil, err)
			return
		}
		res, err := h.Put(r.Context(), k, r.Body)
		reply(w, res, err)
	})
	return mux
}

// HTTPOptions shape the http transport's requests.
type HTTPOptions struct {
	// Query is added to every request (a node's store takes ?model=).
	Query url.Values
	// Sign is called last on every request, after the query is final: a
	// signature covers the path with its query. nil = unsigned.
	Sign func(*http.Request) error
}

// httpTransport: GET manifest and PUT put with the delta as a streamed body.
type httpTransport struct {
	client *http.Client
	o      HTTPOptions
}

// NewHTTPTransport is the http transport with options.
func NewHTTPTransport(o HTTPOptions) Transport {
	tr := &http.Transport{
		// Member traffic, as mesh/meshclient builds it: no Proxy, since a
		// proxy variable inherited by a container must not send a
		// conversation's state anywhere but the member; a bounded dial, since
		// a powered-off host drops packets; and no redirects (below).
		DisableCompression: true,
		WriteBufferSize:    4 << 20,
		ReadBufferSize:     4 << 20,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
			c, err := d.DialContext(ctx, network, addr)
			if err == nil {
				tune(c)
			}
			return c, err
		},
	}
	return &httpTransport{client: &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, o: o}
}

func (*httpTransport) Name() string { return "http" }

type httpServer struct {
	ln  net.Listener
	srv *http.Server
}

func (s *httpServer) Addr() string { return s.ln.Addr().String() }
func (s *httpServer) Close() error { return s.srv.Close() }

func (*httpTransport) Listen(addr string, h Handler) (Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: HTTPHandler(h)}
	go func() { _ = srv.Serve(ln) }()
	return &httpServer{ln, srv}, nil
}

func (t *httpTransport) do(req *http.Request, out any) error {
	if t.o.Sign != nil {
		if err := t.o.Sign(req); err != nil {
			return err
		}
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return &StatusError{Code: resp.StatusCode, Err: errors.New(e.Error)}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxRespJSON)).Decode(out)
}

func base(addr string) string {
	if strings.Contains(addr, "://") {
		return strings.TrimRight(addr, "/")
	}
	return "http://" + addr
}

func (t *httpTransport) url(addr, path, key string, extra url.Values) string {
	q := url.Values{}
	for k, v := range t.o.Query {
		q[k] = v
	}
	for k, v := range extra {
		q[k] = v
	}
	q.Set("key", key)
	return base(addr) + path + "?" + q.Encode()
}

func (t *httpTransport) Manifest(ctx context.Context, addr, key string) (*Manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.url(addr, meshapi.PathKVShareManifest, key, nil), nil)
	if err != nil {
		return nil, err
	}
	var m *Manifest
	return m, t.do(req, &m)
}

func (t *httpTransport) Put(ctx context.Context, addr, key string, delta io.Reader) (PutResult, error) {
	return t.put(ctx, addr, key, nil, delta)
}

// PutFile is MetaPutter: the new file's size and digest go in the query,
// where a signature covers them, since a streamed body's digest is not known
// before it is sent.
func (t *httpTransport) PutFile(ctx context.Context, addr, key string, meta FileMeta, delta io.Reader) (PutResult, error) {
	return t.put(ctx, addr, key, meta.Query(), delta)
}

func (t *httpTransport) put(ctx context.Context, addr, key string, extra url.Values, delta io.Reader) (PutResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, t.url(addr, meshapi.PathKVSharePut, key, extra), delta)
	if err != nil {
		return PutResult{}, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	var r PutResult
	return r, t.do(req, &r)
}

// FileMeta is what a sender says about the file a delta rebuilds before the
// delta is read: its size and FileDigest.
type FileMeta struct {
	Size   int64
	Digest [32]byte // FileDigest; none with an engine delta
	// SaveID is the engine's id of the save the file is, "" = unknown.
	SaveID string
	// Format is "" for a KVDELTA2 stream, FormatEngineDelta for the
	// engine's own delta file.
	Format string
	// Identity the receiver can restore (not on the wire: the receiver sets
	// it from its own model): a KVDELTA2 stream stamped with another is
	// refused with 409 before anything is stored. Zero accepts any.
	Identity Identity
}

// Query is the meta as query parameters (size, digest in hex, save_id and
// format when set).
func (m FileMeta) Query() url.Values {
	q := url.Values{"size": {strconv.FormatInt(m.Size, 10)}}
	if m.Format != FormatEngineDelta {
		q.Set("digest", hex.EncodeToString(m.Digest[:]))
	}
	if m.SaveID != "" {
		q.Set("save_id", m.SaveID)
	}
	if m.Format != "" {
		q.Set("format", m.Format)
	}
	return q
}

// FileMetaFromQuery reads the meta from a request's query.
func FileMetaFromQuery(q url.Values) (FileMeta, error) {
	var m FileMeta
	size, err := strconv.ParseInt(q.Get("size"), 10, 64)
	if err != nil || size <= 0 {
		return m, fmt.Errorf("kvshare: size %q is not a positive number", q.Get("size"))
	}
	if m.SaveID, err = saveIDFromQuery(q); err != nil {
		return m, err
	}
	switch m.Format = q.Get("format"); m.Format {
	case "":
	case FormatEngineDelta:
		if m.SaveID == "" {
			return m, errors.New("kvshare: an engine delta must name its save_id")
		}
		m.Size = size
		return m, nil
	default:
		return m, fmt.Errorf("kvshare: unknown format %q", m.Format)
	}
	d, err := hex.DecodeString(q.Get("digest"))
	if err != nil || len(d) != len(m.Digest) {
		return m, fmt.Errorf("kvshare: digest %q is not %d hex bytes", q.Get("digest"), len(m.Digest))
	}
	m.Size = size
	copy(m.Digest[:], d)
	return m, nil
}

func saveIDFromQuery(q url.Values) (string, error) {
	id := q.Get("save_id")
	if id == "" {
		return "", nil
	}
	if _, err := strconv.ParseUint(id, 16, 64); err != nil || len(id) != 16 {
		return "", fmt.Errorf("kvshare: save_id %q is not 16 hex digits", id)
	}
	return id, nil
}

// MetaPutter is a transport that can tell the receiver the new file's size
// and digest up front. Push uses it when the transport has it.
type MetaPutter interface {
	PutFile(ctx context.Context, addr, key string, meta FileMeta, delta io.Reader) (PutResult, error)
}
