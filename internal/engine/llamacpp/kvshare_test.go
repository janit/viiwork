package llamacpp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/engine"
)

func kvModel(t *testing.T, path, block string, args ...string) config.Model {
	t.Helper()
	a := ""
	if len(args) > 0 {
		q := make([]string, len(args))
		for i, s := range args {
			q[i] = fmt.Sprintf("%q", s)
		}
		a = "\n    args: [" + strings.Join(q, ", ") + "]"
	}
	return parseModel(t, fmt.Sprintf(`  - name: qwen
    engine: llamacpp
    path: %s
    gpus: [0, 1]
    gpus_per_backend: 2
    context: 65536
    parallel: 2%s
    llamacpp:
%s`, path, a, block))
}

const kvBlock = "      kv_share:\n        group: gfx906\n        save_dir: /var/lib/viiwork/slots\n"

func TestKVShareValidation(t *testing.T) {
	cases := map[string]struct {
		block string
		args  []string
		want  string // "" = valid
	}{
		"valid":             {kvBlock, nil, ""},
		"no group":          {"      kv_share:\n        save_dir: /s\n", nil, "kv_share.group is required"},
		"no save_dir":       {"      kv_share:\n        group: g\n", nil, "kv_share.save_dir must be an absolute directory"},
		"relative save_dir": {"      kv_share:\n        group: g\n        save_dir: slots\n", nil, "save_dir must be an absolute"},
		"own slot path":     {kvBlock, []string{"--slot-save-path", "/x"}, "must not contain --slot-save-path"},
		"parallel in args":  {kvBlock, []string{"-np", "4"}, "must not contain -np"},
		"unified kv":        {kvBlock, []string{"--kv-unified"}, "must not contain --kv-unified"},
		"inline flag":       {kvBlock, []string{"--slot-save-path=/x"}, "must not contain --slot-save-path"},
		"other args fine":   {kvBlock, []string{"-ctk", "q8_0", "--jinja"}, ""},
		"bad transport":     {kvBlock + "        transport: tcp\n", nil, `transport must be "http"`},
		"unknown key":       {kvBlock + "        nope: 1\n", nil, "nope"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := New().ValidateOptions("models[0]", config.ModelSpec(kvModel(t, "/m/q.gguf", c.block, c.args...)))
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("got %v, want an error with %q", err, c.want)
			}
		})
	}
}

func TestKVShareCommandAndSlotField(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "slots", "qwen")
	m := kvModel(t, "/m/q.gguf", fmt.Sprintf("      kv_share:\n        group: g\n        save_dir: %s\n", dir))
	cmd := command(t, testEngine(16, 64*gib, 18*gib, nil), m, 0)
	if v, ok := flagValue(cmd.Args, "--slot-save-path"); !ok || v != dir {
		t.Fatalf("--slot-save-path %q %v in %v", v, ok, cmd.Args)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("save_dir not created: %v", err)
	}
	if f := New().SlotField(config.ModelSpec(m)); f != "id_slot" {
		t.Fatalf("SlotField %q", f)
	}
	plain := kvModel(t, "/m/q.gguf", "      threads: 4\n")
	if f := New().SlotField(config.ModelSpec(plain)); f != "" {
		t.Fatalf("a model without kv_share is slot-addressed: %q", f)
	}
	cmd = command(t, testEngine(16, 64*gib, 18*gib, nil), plain, 0)
	if _, ok := flagValue(cmd.Args, "--slot-save-path"); ok {
		t.Fatal("--slot-save-path without kv_share")
	}
	if _, ok := New().SessionShare(config.ModelSpec(plain)); ok {
		t.Fatal("a model without kv_share shares sessions")
	}
}

func TestKVShareSessionShareConfigAndFingerprints(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, size int, seed byte) string {
		b := make([]byte, size)
		for i := range b {
			b[i] = byte(i) ^ seed
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := write("a.gguf", 40<<20, 1)
	same := write("same.gguf", 40<<20, 1)
	// Equal head, different weights far into the file: another fine-tune.
	other := write("other.gguf", 40<<20, 1)
	f, _ := os.OpenFile(other, os.O_RDWR, 0)
	f.WriteAt([]byte{0xff, 0xfe, 0xfd}, 30<<20+12345)
	f.Close()

	e := New()
	share := func(path string, args ...string) engine.SessionShareConfig {
		t.Helper()
		block := kvBlock + "        max_gib: 8\n        min_tokens: 4096\n"
		cfg, ok := e.SessionShare(config.ModelSpec(kvModel(t, path, block, args...)))
		if !ok {
			t.Fatal("no kv_share config")
		}
		return cfg
	}
	cfg := share(a)
	if cfg.Group != "gfx906" || cfg.SaveDir != "/var/lib/viiwork/slots" || cfg.Transport != "http" ||
		cfg.MaxBytes != 8<<30 || cfg.MinTokens != 4096 || cfg.MaxFileBytes != 65536*slotBytesPerToken+slotBytesFixed {
		t.Fatalf("config %+v", cfg)
	}
	if cfg.Model == 0 {
		t.Fatal("no model fingerprint")
	}
	if share(same).Model != cfg.Model {
		t.Fatal("two identical files fingerprint differently")
	}
	if share(other).Model == cfg.Model {
		t.Fatal("a file differing past its header fingerprints the same")
	}
	if share(filepath.Join(dir, "missing.gguf")).Model != 0 {
		t.Fatal("a missing file has a fingerprint")
	}
	base := share(a).Config
	if share(a, "-ctk", "q8_0").Config == base {
		t.Fatal("a cache type does not change the config fingerprint")
	}
	if share(a, "-ctk", "q8_0", "-ctv", "q8_0").Config != share(a, "--cache-type-v=q8_0", "--cache-type-k", "q8_0").Config {
		t.Fatal("spellings of the same flags fingerprint differently")
	}
	if share(a, "--jinja", "--temp", "0.7").Config != base {
		t.Fatal("a flag that shapes no state changed the config fingerprint")
	}
}

func TestSaveAndRestoreSlot(t *testing.T) {
	type call struct{ path, query, file string }
	var calls []call
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct{ Filename string }
		json.Unmarshal(body, &req)
		calls = append(calls, call{r.URL.Path, r.URL.RawQuery, req.Filename})
		if status != http.StatusOK {
			w.WriteHeader(status)
			io.WriteString(w, `{"error":{"message":"nope"}}`)
			return
		}
		io.WriteString(w, `{"id_slot":1,"n_saved":10}`)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	e := New()
	ctx := context.Background()
	if err := e.SaveSlot(ctx, addr, 1, "kvs-a.bin"); err != nil {
		t.Fatal(err)
	}
	if err := e.RestoreSlot(ctx, addr, 0, "kvi-a.bin"); err != nil {
		t.Fatal(err)
	}
	want := []call{{"/slots/1", "action=save", "kvs-a.bin"}, {"/slots/0", "action=restore", "kvi-a.bin"}}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("calls %v, want %v", calls, want)
	}
	status = http.StatusNotImplemented
	if err := e.SaveSlot(ctx, addr, 1, "x"); !errors.Is(err, engine.ErrSessionGone) {
		t.Fatalf("501 on a save: %v", err)
	}
	status = http.StatusBadRequest
	if err := e.RestoreSlot(ctx, addr, 1, "x"); err == nil || !strings.Contains(err.Error(), "400 nope") {
		t.Fatalf("400 on a restore: %v", err)
	}
}
