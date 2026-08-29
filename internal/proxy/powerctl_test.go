package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/internal/peer"
	"github.com/janit/viiwork/internal/power"
)

func powerHandler(t *testing.T, self string, hosts []string) *Handler {
	t.Helper()
	reg := peer.NewRegistry("node-a", "m", nil, nil, time.Second)
	reg.SetLocation(self, self+":8080")
	h := NewMeshHandler(nil, reg, time.Second)
	h.SetPowerControl(power.NewController(power.ControlConfig{Enabled: true, Hosts: hosts}, self))
	return h
}

func postPower(t *testing.T, h *Handler, path, body string) (int, powerResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	var out powerResponse
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// The node serving the dashboard must refuse to switch its own machine off.
// Powering it down destroys the answer to the request and the page that asked,
// and the UI's disabled button is a courtesy -- this is the actual guard.
func TestMeshPowerRefusesSelf(t *testing.T) {
	h := powerHandler(t, "gb1", []string{"gb1", "gb2"})
	for _, action := range []string{"off", "cycle"} {
		code, out := postPower(t, h, "/v1/mesh/power", `{"host":"gb1","action":"`+action+`"}`)
		if code != http.StatusForbidden {
			t.Errorf("%s on self: got %d, want 403", action, code)
		}
		if !strings.Contains(out.Error, "gb1") {
			t.Errorf("%s on self: error should name the host, got %q", action, out.Error)
		}
	}
	// Reading its own state is harmless and stays allowed.
	if code, _ := postPower(t, h, "/v1/mesh/power", `{"host":"gb1","action":"status"}`); code == http.StatusForbidden {
		t.Error("status on self should not be refused; it changes nothing")
	}
}

// The allowlist is the only guard standing in for authentication, so a host
// that nobody wrote down must be untargetable even though it is a real peer.
func TestMeshPowerRefusesUnlistedHost(t *testing.T) {
	h := powerHandler(t, "gb1", []string{"gb1", "gb2"})
	code, out := postPower(t, h, "/v1/mesh/power", `{"host":"gb9","action":"off"}`)
	if code != http.StatusForbidden {
		t.Fatalf("unlisted host: got %d, want 403", code)
	}
	if !strings.Contains(out.Error, "power.control.hosts") {
		t.Errorf("error should point at the allowlist, got %q", out.Error)
	}
}

// The action name selects the ipmitool subcommand, so anything not on the list
// has to be rejected by name rather than passed through.
func TestPowerRejectsUnknownAction(t *testing.T) {
	h := powerHandler(t, "gb1", []string{"gb1"})
	for _, body := range []string{
		`{"host":"gb2","action":"chassis bootdev pxe"}`,
		`{"host":"gb2","action":"reset; rm -rf /"}`,
		`{"host":"gb2","action":""}`,
	} {
		if code, _ := postPower(t, h, "/v1/mesh/power", body); code != http.StatusBadRequest {
			t.Errorf("body %s: got %d, want 400", body, code)
		}
	}
}

// The executor acts on its own host only. Honouring a host field naming
// somebody else would let a request aimed at gb2 switch off gb1.
func TestPowerExecutorRejectsOtherHost(t *testing.T) {
	h := powerHandler(t, "gb1", []string{"gb1", "gb2"})
	code, out := postPower(t, h, "/v1/power", `{"host":"gb2","action":"off"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", code)
	}
	if !strings.Contains(out.Error, "gb1") {
		t.Errorf("error should name the host it does act on, got %q", out.Error)
	}
}

// A node without power control answers 503, not 404: the route exists on every
// build, so a consumer can tell "will not" from "does not know how".
func TestPowerDisabledAnswers503(t *testing.T) {
	reg := peer.NewRegistry("node-a", "m", nil, nil, time.Second)
	reg.SetLocation("gb1", "gb1:8080")
	h := NewMeshHandler(nil, reg, time.Second)
	for _, path := range []string{"/v1/power", "/v1/mesh/power"} {
		if code, _ := postPower(t, h, path, `{"host":"gb1","action":"status"}`); code != http.StatusServiceUnavailable {
			t.Errorf("%s: got %d, want 503", path, code)
		}
	}
}

// An enabled controller with an empty allowlist must permit nothing. The
// config layer rejects that combination, but the controller cannot assume it.
func TestControllerEmptyAllowlistPermitsNothing(t *testing.T) {
	c := power.NewController(power.ControlConfig{Enabled: true}, "gb1")
	if c.Allowed("gb1") {
		t.Error("an empty allowlist must not permit the local host")
	}
	if _, err := c.Local(t.Context(), "status"); err == nil {
		t.Error("expected an error acting on a host that is not allowlisted")
	}
}

// Without a BMC entry a powered-off host cannot be reached, and the error has
// to say why rather than surfacing an ipmitool exit status.
func TestControllerRemoteNeedsBMC(t *testing.T) {
	c := power.NewController(power.ControlConfig{Enabled: true, Hosts: []string{"gb2"}}, "gb1")
	_, err := c.Remote(t.Context(), "gb2", "on")
	if err == nil || !strings.Contains(err.Error(), "out-of-band") {
		t.Fatalf("expected an out-of-band explanation, got %v", err)
	}
}

// Reading the local host's chassis state must go in-band, not out to a BMC over
// the network. Without a local path the request goes looking for a peer and
// then for credentials, and fails on the one machine it is actually running on.
func TestMeshPowerStatusOnSelfGoesInBand(t *testing.T) {
	h := powerHandler(t, "gb1", []string{"gb1"})
	code, out := postPower(t, h, "/v1/mesh/power", `{"host":"gb1","action":"status"}`)
	// No ipmitool in the test environment, so the command itself fails — but it
	// must fail having tried in-band, not having demanded a BMC address.
	if strings.Contains(out.Error, "out-of-band") {
		t.Fatalf("status on the local host must not require out-of-band access, got %q", out.Error)
	}
	if code == http.StatusBadGateway {
		t.Errorf("got 502 (unreachable host); the local host is reachable in-band")
	}
}
