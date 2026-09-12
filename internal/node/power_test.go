package node

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/janit/viiwork/v2/internal/power"
	"github.com/janit/viiwork/v2/mesh"
	"github.com/janit/viiwork/v2/meshapi"
)

// fakeIpmitool puts an ipmitool on PATH that answers every chassis command.
// The controller execs ipmitool by name, so PATH is its seam from outside the
// power package.
func fakeIpmitool(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"Chassis Power is on\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ipmitool"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

type powerResult struct {
	Host   string `json:"host"`
	Action string `json:"action"`
	Result string `json:"result"`
	Via    string `json:"via"`
	Error  string `json:"error"`
}

func postPowerTo(t *testing.T, h http.Handler, path, body string) (int, powerResult) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	var out powerResult
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func powerServer(t *testing.T, members []mesh.Member, cfg power.ControlConfig) http.Handler {
	t.Helper()
	d, f := fakeServer(t)
	f.members = members
	d.PowerControl = power.NewController(cfg, "gb1")
	return NewServer(d)
}

func TestPowerDisabled(t *testing.T) {
	d, _ := fakeServer(t)
	h := NewServer(d)
	for _, path := range []string{meshapi.PathPower, meshapi.PathMeshPower} {
		if code, _ := postPowerTo(t, h, path, `{"host":"gb1","action":"status"}`); code != 503 {
			t.Errorf("PW1 %s: %d", path, code)
		}
	}
}

func TestPowerGuards(t *testing.T) {
	fakeIpmitool(t)
	h := powerServer(t, nil, power.ControlConfig{Enabled: true, Hosts: []string{"gb1", "gb2"}})
	if code, out := postPowerTo(t, h, meshapi.PathMeshPower, `{"host":"gb9","action":"off"}`); code != 403 || !strings.Contains(out.Error, "power.control.hosts") {
		t.Errorf("PW2: %d %+v", code, out)
	}
	if code, out := postPowerTo(t, h, meshapi.PathMeshPower, `{"host":"gb1","action":"off"}`); code != 403 || !strings.Contains(out.Error, "gb1") {
		t.Errorf("PW3: %d %+v", code, out)
	}
	if code, out := postPowerTo(t, h, meshapi.PathMeshPower, `{"host":"gb1","action":"status"}`); code != 200 || out.Via != "in-band" || out.Result != "Chassis Power is on" {
		t.Errorf("PW4: %d %+v", code, out)
	}
	if code, _ := postPowerTo(t, h, meshapi.PathMeshPower, `{"host":"gb2",`); code != 400 {
		t.Errorf("PW7: %d", code)
	}
	if code, _ := postPowerTo(t, h, meshapi.PathMeshPower, `{"host":"gb2","action":"reset; rm -rf /"}`); code != 400 {
		t.Errorf("unknown action: %d", code)
	}
	if code, out := postPowerTo(t, h, meshapi.PathPower, `{"host":"gb2","action":"off"}`); code != 400 || !strings.Contains(out.Error, "gb1") {
		t.Errorf("executor aimed elsewhere: %d %+v", code, out)
	}
}

func TestPowerForwardsToMember(t *testing.T) {
	var gotAction atomic.Value
	gb2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotAction.Store(r.URL.Path + " " + string(body))
		_, _ = io.WriteString(w, `{"host":"gb2","action":"on","result":"Chassis Power Control: Up/On","via":"in-band"}`)
	}))
	t.Cleanup(gb2.Close)
	members := []mesh.Member{memberAt(t, "gb2", gb2, meshapi.MemberAlive, meshapi.RoleNode, false)}
	h := powerServer(t, members, power.ControlConfig{Enabled: true, Hosts: []string{"gb1", "gb2"}})
	code, out := postPowerTo(t, h, meshapi.PathMeshPower, `{"host":"gb2","action":"on"}`)
	if code != 200 || out.Via != "peer" || out.Result != "Chassis Power Control: Up/On" {
		t.Errorf("PW5: %d %+v", code, out)
	}
	if got, _ := gotAction.Load().(string); !strings.HasPrefix(got, "/v1/power ") || !strings.Contains(got, `"action":"on"`) {
		t.Errorf("PW5: member received %q", got)
	}
}

func TestPowerFallsBackToOutOfBand(t *testing.T) {
	fakeIpmitool(t)
	gone := httptest.NewServer(http.NotFoundHandler())
	members := []mesh.Member{memberAt(t, "gb2", gone, meshapi.MemberAlive, meshapi.RoleNode, false)}
	gone.Close()
	h := powerServer(t, members, power.ControlConfig{Enabled: true, Hosts: []string{"gb1", "gb2"},
		BMCs: map[string]power.BMC{"gb2": {Addr: "192.0.2.2", Username: "admin", Password: "secret"}}})
	code, out := postPowerTo(t, h, meshapi.PathMeshPower, `{"host":"gb2","action":"on"}`)
	if code != 200 || out.Via != "out-of-band" {
		t.Errorf("PW6: %d %+v", code, out)
	}
}
