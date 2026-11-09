package sessionshare

import (
	"context"
	"io"
	"net/http"

	"github.com/janit/viiwork/v2/internal/httpjson"
	"github.com/janit/viiwork/v2/internal/kvshare"
	"github.com/janit/viiwork/v2/meshapi"
)

// deltaOverhead is what a delta may carry beyond the file it rebuilds: its
// header and one item header per section.
const deltaOverhead = 1 << 20

// ServeHTTP is the receiving side, on meshapi.PathKVShareManifest and
// meshapi.PathKVSharePut. Only a member may call it, authorised like a
// forward: a replica is a conversation's prompt, so neither a client nor a
// program on this machine may read or write one.
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if m.d.Verify == nil || !m.d.Verify(r) {
		httpjson.Error(w, http.StatusForbidden, "forbidden", "kv_share accepts members only")
		return
	}
	name := r.URL.Query().Get("model")
	md := m.model(name)
	if md == nil {
		httpjson.Error(w, http.StatusNotFound, "not_found", "model "+name+" shares no sessions here")
		return
	}
	h := storeHandler{md: md}
	if r.URL.Path == meshapi.PathKVSharePut {
		meta, err := kvshare.FileMetaFromQuery(r.URL.Query())
		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if meta.Size > md.cfg.MaxFileBytes {
			httpjson.Error(w, http.StatusRequestEntityTooLarge, "invalid_request", "the session file is larger than the model's context allows")
			return
		}
		if meta.Size > md.cfg.MaxBytes {
			httpjson.Error(w, http.StatusInsufficientStorage, "server_error", "no room for the replica")
			return
		}
		// Room first when the store looks full, so it makes way for the
		// newest session; never at the cost of the replica being updated.
		// Eviction walks every replica, so a put that fits skips it.
		md.makeRoom(meta.Size, r.URL.Query().Get("key"))
		r.Body = http.MaxBytesReader(w, r.Body, meta.Size+deltaOverhead)
		h.meta = &meta
	}
	kvshare.HTTPHandler(h).ServeHTTP(w, r)
	if h.meta != nil {
		md.sweep()
	}
}

// storeHandler is one request's view of a model's store: a put must build
// the file its query declared.
type storeHandler struct {
	md   *model
	meta *kvshare.FileMeta
}

func (h storeHandler) Manifest(ctx context.Context, key string) (*kvshare.Manifest, error) {
	return h.md.store.Manifest(ctx, key)
}

func (h storeHandler) Put(ctx context.Context, key string, delta io.Reader) (kvshare.PutResult, error) {
	if h.meta == nil {
		return kvshare.PutResult{}, kvshare.Statusf(http.StatusBadRequest, "kvshare: a put must declare size and digest")
	}
	put := h.md.store.PutExpect
	if h.meta.Format == kvshare.FormatEngineDelta {
		put = h.md.store.PutEngineDelta
	}
	meta := *h.meta
	if h.md.slots != nil {
		// The node stamps a slot model's replicas, and restores only its own
		// stamp: storing another would cost the room of one it can use.
		if id := h.md.ownID(); id != 0 {
			meta.Identity = kvshare.Identity{Model: id, Config: h.md.cfg.Config}
		}
	}
	res, err := put(ctx, key, delta, meta)
	if err == nil {
		h.md.puts.Add(1)
	}
	return res, err
}
