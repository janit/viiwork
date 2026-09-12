package alias

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/internal/meshauth"
	"github.com/janit/viiwork/v2/meshapi"
)

var (
	secretA = bytes.Repeat([]byte{0xa}, 32)
	secretB = bytes.Repeat([]byte{0xb}, 32)
)

type apiFx struct {
	*serviceFx
	h http.Handler
}

func newAPIFx(t *testing.T, primary, previous []byte) *apiFx {
	t.Helper()
	fx := &apiFx{serviceFx: newServiceFx(t)}
	auth, err := NewAuthorizer("gb1", primary, previous)
	if err != nil {
		t.Fatal(err)
	}
	fx.h = NewHandler(fx.svc, auth)
	return fx
}

// do sends a request; signWith signs it with that secret, and remote sets
// RemoteAddr.
func (fx *apiFx) do(t *testing.T, method, target, body string, signWith []byte, remote string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if remote != "" {
		req.RemoteAddr = remote
	}
	if signWith != nil {
		signer, err := meshauth.NewSigner(signWith, "viiwork-cli@testhost")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := signer.SignRequest(req, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	fx.h.ServeHTTP(rec, req)
	return rec
}

func apiError(t *testing.T, rec *httptest.ResponseRecorder) meshapi.ErrorBody {
	t.Helper()
	var e meshapi.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body %q: %v", rec.Body.String(), err)
	}
	return e.Error
}

const coderBody = `{"target":"Qwen3.8-27B","fallbacks":[],"force":false}`

func TestAPIRead(t *testing.T) {
	fx := newAPIFx(t, nil, nil)
	if _, err := fx.svc.Set("stable-coder", coderReq()); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.Set("gone", coderReq()); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.Delete("gone"); err != nil {
		t.Fatal(err)
	}
	rec := fx.do(t, http.MethodGet, "/v1/aliases", "", nil, "")
	var list meshapi.AliasesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || rec.Code != 200 || len(list.Aliases) != 1 || list.Aliases[0].Name != "stable-coder" {
		t.Errorf("A1: %d %s", rec.Code, rec.Body.String())
	}
	rec = fx.do(t, http.MethodGet, "/v1/aliases?table=1", "", nil, "")
	var table meshapi.AliasTable
	if err := json.Unmarshal(rec.Body.Bytes(), &table); err != nil || rec.Code != 200 || table.V != 1 || !table.Aliases["gone"].Deleted || len(table.Aliases["gone"].History) != 1 {
		t.Errorf("A2: %d %s", rec.Code, rec.Body.String())
	}
	if rec := fx.do(t, http.MethodGet, "/v1/aliases/stable-coder", "", nil, ""); rec.Code != 405 {
		t.Errorf("A15: %d", rec.Code)
	}
	if rec := fx.do(t, http.MethodGet, "/v1/aliases/a/b/c", "", nil, ""); rec.Code != 404 {
		t.Errorf("unknown path: %d", rec.Code)
	}
}

func TestAPISecured(t *testing.T) {
	t.Run("A3 signed write", func(t *testing.T) {
		fx := newAPIFx(t, secretA, nil)
		rec := fx.do(t, http.MethodPut, "/v1/aliases/stable-coder", coderBody, secretA, "100.64.0.9:5555")
		var b meshapi.AliasBroadcast
		if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil || rec.Code != 200 || b.Entry.Ver != 1 || b.Name != "stable-coder" {
			t.Errorf("%d %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("A4 unsigned", func(t *testing.T) {
		fx := newAPIFx(t, secretA, nil)
		rec := fx.do(t, http.MethodPut, "/v1/aliases/stable-coder", coderBody, nil, "127.0.0.1:5555")
		if rec.Code != 401 || !strings.Contains(apiError(t, rec).Message, "valid X-Viiwork-Auth signature") {
			t.Errorf("%d %s", rec.Code, rec.Body.String())
		}
		if fx.store.Live() != 0 {
			t.Error("an unsigned write was stored")
		}
	})

	t.Run("A5 previous secret", func(t *testing.T) {
		fx := newAPIFx(t, secretA, secretB)
		if rec := fx.do(t, http.MethodPut, "/v1/aliases/stable-coder", coderBody, secretB, ""); rec.Code != 200 {
			t.Errorf("%d %s", rec.Code, rec.Body.String())
		}
		if rec := fx.do(t, http.MethodPut, "/v1/aliases/other", coderBody, bytes.Repeat([]byte{0xc}, 32), ""); rec.Code != 401 {
			t.Errorf("an unknown secret: %d", rec.Code)
		}
	})

	t.Run("A6 replay", func(t *testing.T) {
		fx := newAPIFx(t, secretA, nil)
		req := httptest.NewRequest(http.MethodPut, "/v1/aliases/stable-coder", strings.NewReader(coderBody))
		signer, _ := meshauth.NewSigner(secretA, "viiwork-cli@testhost")
		if _, err := signer.SignRequest(req, []byte(coderBody)); err != nil {
			t.Fatal(err)
		}
		first := httptest.NewRecorder()
		fx.h.ServeHTTP(first, req)
		replay := httptest.NewRequest(http.MethodPut, "/v1/aliases/stable-coder", strings.NewReader(coderBody))
		replay.Header = req.Header.Clone()
		second := httptest.NewRecorder()
		fx.h.ServeHTTP(second, replay)
		if first.Code != 200 || second.Code != 401 {
			t.Errorf("first %d, replay %d", first.Code, second.Code)
		}
	})

	t.Run("A7 validation", func(t *testing.T) {
		fx := newAPIFx(t, secretA, nil)
		rec := fx.do(t, http.MethodPut, "/v1/aliases/stable-coder", `{"target":"nosuch"}`, secretA, "")
		if e := apiError(t, rec); rec.Code != 409 || !strings.Contains(e.Message, "--force") || e.Type != "invalid_request" {
			t.Errorf("%d %s", rec.Code, rec.Body.String())
		}
	})
}

func TestAPIOpen(t *testing.T) {
	cases := []struct {
		id, remote string
		want       int
	}{
		{"A8", "127.0.0.1:5555", 200},
		{"A9", "[::1]:5555", 200},
		{"A10", "100.64.0.2:5555", 403},
	}
	for _, c := range cases {
		fx := newAPIFx(t, nil, nil)
		rec := fx.do(t, http.MethodPut, "/v1/aliases/stable-coder", coderBody, nil, c.remote)
		if rec.Code != c.want {
			t.Errorf("%s: %d %s", c.id, rec.Code, rec.Body.String())
		}
		if c.want == 403 && !strings.Contains(apiError(t, rec).Message, "only from this machine") {
			t.Errorf("%s: %s", c.id, rec.Body.String())
		}
	}

	fx := newAPIFx(t, nil, nil)
	const lo = "127.0.0.1:5555"
	if rec := fx.do(t, http.MethodDelete, "/v1/aliases/nosuch", "", nil, lo); rec.Code != 404 || apiError(t, rec).Type != "not_found" {
		t.Errorf("A11: %d %s", rec.Code, rec.Body.String())
	}
	if rec := fx.do(t, http.MethodPut, "/v1/aliases/stable-coder", coderBody, nil, lo); rec.Code != 200 {
		t.Fatalf("setup: %d", rec.Code)
	}
	if rec := fx.do(t, http.MethodPost, "/v1/aliases/stable-coder/revert", "", nil, lo); rec.Code != 409 {
		t.Errorf("A12: %d %s", rec.Code, rec.Body.String())
	}
	if rec := fx.do(t, http.MethodPut, "/v1/aliases/big", `{"target":"`+strings.Repeat("x", 70<<10)+`"}`, nil, lo); rec.Code != 413 {
		t.Errorf("A13: %d", rec.Code)
	}
	if rec := fx.do(t, http.MethodPut, "/v1/aliases/stable-coder", "{", nil, lo); rec.Code != 400 {
		t.Errorf("A14: %d", rec.Code)
	}
	if rec := fx.do(t, http.MethodPut, "/v1/aliases/a%2Fb", coderBody, nil, lo); rec.Code != 409 || !strings.Contains(apiError(t, rec).Message, "must match") {
		t.Errorf("A16: %d %s", rec.Code, rec.Body.String())
	}
	if rec := fx.do(t, http.MethodPost, "/v1/aliases/stable-coder", "", nil, lo); rec.Code != 405 {
		t.Errorf("POST on an alias: %d", rec.Code)
	}
	if rec := fx.do(t, http.MethodDelete, "/v1/aliases/stable-coder", "", nil, lo); rec.Code != 200 {
		t.Errorf("DELETE: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAPIPersistFailure(t *testing.T) {
	fx := newAPIFx(t, nil, nil)
	if _, err := fx.svc.Set("stable-coder", coderReq()); err != nil {
		t.Fatal(err)
	}
	fx.store.mu.Lock()
	fx.store.write = func(string, string, []byte) error { return errors.New("disk full") }
	fx.store.mu.Unlock()
	rec := fx.do(t, http.MethodPut, "/v1/aliases/stable-coder", `{"target":"gemma-4-31B-it"}`, nil, "127.0.0.1:5555")
	if rec.Code != 500 || !strings.Contains(apiError(t, rec).Message, "alias write failed") {
		t.Errorf("A17: %d %s", rec.Code, rec.Body.String())
	}
	rec = fx.do(t, http.MethodGet, "/v1/aliases", "", nil, "")
	var list meshapi.AliasesResponse
	if json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Aliases) != 1 || list.Aliases[0].Target != "Qwen3.8-27B" {
		t.Errorf("A17: after the failure GET shows %s", rec.Body.String())
	}
}
