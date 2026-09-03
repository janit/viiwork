package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/meshauth"
	"github.com/janit/viiwork/internal/peer"
)

func testSigners(t *testing.T) (caller, responder *meshauth.Signer) {
	t.Helper()
	secret := []byte("0123456789abcdef0123456789abcdef")
	c, err := meshauth.NewSigner(secret, "gb1-a1b2")
	if err != nil {
		t.Fatal(err)
	}
	r, err := meshauth.NewSigner(secret, "gb2-c3d4")
	if err != nil {
		t.Fatal(err)
	}
	return c, r
}

func plainJSON() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"node_id": "gb2-c3d4"})
	})
}

func readAllBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSignedJSONSignsAProvenRequest(t *testing.T) {
	caller, responder := testSigners(t)
	srv := httptest.NewServer(signedJSON(plainJSON(), responder))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/cluster", nil)
	nonce, err := caller.SignRequest(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAllBody(t, resp)

	if err := caller.VerifyResponse(resp.Header, "/v1/cluster", nonce, body); err != nil {
		t.Fatalf("response did not verify: %v", err)
	}
}

func TestSignedJSONLeavesUnsignedRequestsAlone(t *testing.T) {
	// The browser and the gateway call these endpoints with no proof at all,
	// and must get exactly today's response.
	_, responder := testSigners(t)
	srv := httptest.NewServer(signedJSON(plainJSON(), responder))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/cluster")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get(meshauth.HeaderAuth); got != "" {
		t.Fatalf("unsigned request got a signature header %q", got)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body := string(readAllBody(t, resp)); body == "" {
		t.Fatal("body was empty")
	}
}

func TestSignedJSONRefusesABadProof(t *testing.T) {
	caller, responder := testSigners(t)
	srv := httptest.NewServer(signedJSON(plainJSON(), responder))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/cluster", nil)
	caller.SignRequest(req, nil)
	req.Header.Set(meshauth.HeaderAuth, "v1=notthemac")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a proof that was offered and failed", resp.StatusCode)
	}
}

func TestSignedJSONWithNoSignerIsATransparentPassthrough(t *testing.T) {
	srv := httptest.NewServer(signedJSON(plainJSON(), nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRegistryPollVerifiesASignedJSONEndpoint(t *testing.T) {
	// The seam the packages share: the registry's status poll must accept
	// what signedJSON produces, or a real fleet verifies nothing while every
	// package's own tests stay green.
	caller, responder := testSigners(t)
	srv := httptest.NewServer(signedJSON(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(peer.StatusResponse{NodeID: "gb2-c3d4", Models: []string{"m1"}})
	}), responder))
	defer srv.Close()

	p := peer.NewPeerState(srv.Listener.Addr().String())
	reg := peer.NewRegistry("gb1-a1b2", "m1", nil, []*peer.PeerState{p}, 2*time.Second)
	reg.SetSigner(caller)
	reg.PollOnce(context.Background())

	if !p.Verified() {
		t.Fatal("the registry did not verify a response signed by signedJSON")
	}
}
