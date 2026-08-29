package proxy

import (
	"context"
	"log"
	"net"
	"net/http"
	"time"
)

// meshPortRetry is how long a node waits before asking for the mesh port
// again. It is the handover latency when the holder stops: short enough that a
// container restart moves the port within one docker-compose cycle, long
// enough that losing the race costs one syscall a minute rather than a busy
// loop. Not configurable — a knob here would only ever be set wrong. A var
// rather than a const so the handover test does not have to wait it out.
var meshPortRetry = 15 * time.Second

// meshRoot serves the mesh dashboard at "/" and routes everything else
// normally. It rewrites the path rather than writing the page itself, so the
// request still passes through Handler.ServeHTTP and picks up CORS, the panic
// recovery and every other endpoint — which mesh.html needs, since it is not a
// standalone document: it opens /v1/mesh/stream, posts to /v1/mesh/power and
// links each row to /prompt, all same-origin.
type meshRoot struct{ h *Handler }

func (m meshRoot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		// Shallow-copy request and URL instead of mutating: the caller owns
		// both, and Clone would deep-copy headers for a one-field change.
		u := *r.URL
		u.Path = "/mesh"
		r2 := *r
		r2.URL = &u
		r = &r2
	}
	m.h.ServeHTTP(w, r)
}

// ServeMeshPort keeps a well-known port serving the mesh dashboard for as long
// as this process runs, and blocks until ctx is cancelled.
//
// The port is contended, not assigned. A host runs one viiwork instance per
// model, so every instance asks for the same port and the OS hands it to
// exactly one of them; the losers keep asking on meshPortRetry. That is what
// makes the address dependable: it needs no per-host configuration, no
// designated instance, and no reverse proxy, and it survives the holder going
// away because the next instance to ask picks it up. Whichever instance
// answers, the page it serves is the same — the mesh view is assembled from
// peer state that every node already has.
//
// A foreign process holding the port is the same case as a co-tenant holding
// it, and is handled the same way: retry quietly, serve the node's own port
// normally, never fail the process over a dashboard.
func ServeMeshPort(ctx context.Context, addr string, h *Handler) {
	var lc net.ListenConfig
	var lastErr string

	for ctx.Err() == nil {
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			// Losing is the normal case on a multi-instance host, so this
			// logs the first refusal and then only a change of reason. A
			// line per retry would be a line a minute, forever, on every
			// instance but one.
			if msg := err.Error(); msg != lastErr {
				lastErr = msg
				log.Printf("mesh dashboard port %s taken, will retry: %v", addr, err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(meshPortRetry):
			}
			continue
		}
		lastErr = ""
		log.Printf("mesh dashboard on %s", addr)

		srv := &http.Server{
			Handler:           meshRoot{h},
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		// Close rather than Shutdown: this listener serves a dashboard and a
		// pair of SSE streams that never end on their own, so a graceful wait
		// would just burn the shutdown budget the backends need. stop()
		// unregisters, or the registrations would pile up one per iteration.
		stop := context.AfterFunc(ctx, func() { srv.Close() })
		err = srv.Serve(ln)
		stop()
		if ctx.Err() == nil {
			log.Printf("mesh dashboard listener on %s stopped: %v", addr, err)
		}
	}
}
