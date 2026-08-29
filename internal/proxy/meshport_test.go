package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The whole point of the second port is that "/" is the mesh view, so a URL
// with nothing after the host reaches it. "/mesh" has to keep working on the
// same listener, because every link the dashboard hands out is same-origin.
func TestMeshRootServesMeshDashboardAtSlash(t *testing.T) {
	h := &Handler{}
	srv := httptest.NewServer(meshRoot{h})
	defer srv.Close()

	for _, path := range []string{"/", "/mesh"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, resp.StatusCode)
		}
		if !strings.Contains(string(body), "/v1/mesh/stream") {
			t.Errorf("GET %s did not serve the mesh dashboard", path)
		}
	}
}

// Only "/" moves. The rewrite has to leave the API alone, because this
// listener is the whole origin as far as the page is concerned: mesh.html
// opens /v1/mesh/stream and posts to /v1/mesh/power against it, and a node
// reached on the mesh port is otherwise an ordinary node.
func TestMeshRootLeavesOtherRoutesAlone(t *testing.T) {
	srv := httptest.NewServer(meshRoot{&Handler{}})
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models: status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET /v1/models: content-type %q, want application/json", ct)
	}
}

// Contention is the mechanism, so it is worth pinning both halves: an instance
// that loses the port must keep running and serving its own, and it must take
// the port over once the holder lets go. Without the second half a
// docker-compose restart of whichever instance happened to hold 8086 would
// take the fleet's dashboard address down until the next full redeploy.
func TestMeshPortIsContendedAndHandedOver(t *testing.T) {
	restore := meshPortRetry
	meshPortRetry = 50 * time.Millisecond
	t.Cleanup(func() { meshPortRetry = restore })

	addr, err := freeAddr()
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}

	// Stand in for the instance that won the race.
	holder, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("holding %s: %v", addr, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		ServeMeshPort(ctx, addr, &Handler{})
	}()

	// The loser must not have taken the port from under the holder, and must
	// not have died trying.
	time.Sleep(200 * time.Millisecond)
	if probeMesh(addr) {
		t.Fatal("mesh listener bound a port already held by another instance")
	}

	holder.Close()

	deadline := time.Now().Add(10 * time.Second)
	for !probeMesh(addr) {
		if time.Now().After(deadline) {
			t.Fatalf("mesh listener never took over %s after the holder released it", addr)
		}
		time.Sleep(200 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeMeshPort did not return after its context was cancelled")
	}
}

// probeMesh reports whether addr is serving the mesh dashboard at "/".
func probeMesh(addr string) bool {
	c := &http.Client{Timeout: time.Second}
	resp, err := c.Get("http://" + addr + "/")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode == http.StatusOK && strings.Contains(string(body), "/v1/mesh/stream")
}

func freeAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	return fmt.Sprintf("127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port), nil
}
