package strata

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/internal/engine"
)

// shareConfig is a one-card Strata JSON with what kv_share needs.
const shareConfig = `{
 "args": ["--max-context", "65536", "--conversation-cache-mib", "8192"],
 "cwd": "/opt/strata",
 "model_name": "qwen3.8-flash-next",
 "slot_save_path": "/k/sessions"
}`

const shareBlock = "dir: /opt/strata\nkv_share:\n  group: flash\n"

func shareSpec(path string) engine.Spec {
	return engine.Spec{Name: "qwen3.8-flash-next", Path: path, GPUs: []int{1}, Port: 18090, Context: 65536, Parallel: 1, Backends: 1}
}

func TestKVShareOptions(t *testing.T) {
	path := writeConfig(t, shareConfig)
	for _, tc := range []struct {
		name, block, want string
		gpus              []int
	}{
		{name: "minimal", block: shareBlock},
		{name: "no group", block: "dir: /opt/strata\nkv_share:\n  max_gib: 4\n", want: "models[0].strata.kv_share.group is required"},
		{name: "tcp", block: shareBlock + "  transport: tcp\n", want: `models[0].strata.kv_share.transport must be "http"`},
		{name: "relative dir", block: shareBlock + "  dir: kv\n", want: "models[0].strata.kv_share.dir must be absolute"},
		{name: "negative budget", block: shareBlock + "  max_gib: -1\n", want: "max_gib must not be negative"},
		{name: "negative min", block: shareBlock + "  min_tokens: -5\n", want: "min_tokens must not be negative"},
		{name: "unknown key", block: shareBlock + "  grop: x\n", want: "grop"},
		{name: "layer split", block: shareBlock, gpus: []int{0, 1}, want: "one GPU per backend"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := shareSpec(path)
			if tc.gpus != nil {
				s.GPUs = tc.gpus
			}
			err := New().ValidateOptions("models[0]", spec(t, s, tc.block))
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}

func TestKVShareCommandRefusesTheFile(t *testing.T) {
	for _, tc := range []struct{ name, file, want string }{
		{"no slot_save_path", `{"args": ["--max-context", "65536", "--conversation-cache-mib", "8192"]}`, `"slot_save_path"`},
		{"relative without cwd", `{"args": ["--max-context", "65536", "--conversation-cache-mib", "8192"], "slot_save_path": "sess"}`, "is relative"},
		{"parking off", `{"args": ["--max-context", "65536", "--conversation-cache-mib", "0"], "slot_save_path": "/k/s"}`, "--conversation-cache-mib"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, strings.Replace(tc.file, "{", `{"model_name": "qwen3.8-flash-next", `, 1))
			_, err := New().Command(spec(t, shareSpec(path), shareBlock))
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), path) {
				t.Fatalf("error %v, want %q naming %s", err, tc.want, path)
			}
			// without kv_share the same file starts
			if _, err := New().Command(spec(t, shareSpec(path), "dir: /opt/strata\n")); err != nil {
				t.Fatalf("without kv_share: %v", err)
			}
		})
	}
	if _, err := New().Command(spec(t, shareSpec(writeConfig(t, shareConfig)), shareBlock)); err != nil {
		t.Fatal(err)
	}
	// Strata 0.2 turns the conversation cache on by default
	noFlag := strings.Replace(shareConfig, `, "--conversation-cache-mib", "8192"`, "", 1)
	if _, err := New().Command(spec(t, shareSpec(writeConfig(t, noFlag)), shareBlock)); err != nil {
		t.Fatalf("default cache: %v", err)
	}
}

func TestSessionShare(t *testing.T) {
	path := writeConfig(t, strings.Replace(shareConfig, `"/k/sessions"`, `"sessions"`, 1))
	cfg, ok := New().SessionShare(spec(t, shareSpec(path), shareBlock+"  min_tokens: 100\n  max_gib: 0.5\n"))
	if !ok {
		t.Fatal("kv_share is set")
	}
	want := engine.SessionShareConfig{
		Group: "flash", Transport: "http", SaveDir: "/opt/strata/sessions",
		MaxBytes: 1 << 29, MinTokens: 100, MaxFileBytes: 65536*sessionBytesPerToken + sessionBytesFixed,
	}
	if cfg != want {
		t.Fatalf("got %+v\nwant %+v", cfg, want)
	}
	cfg, _ = New().SessionShare(spec(t, shareSpec(path), shareBlock))
	if cfg.MinTokens != engine.DefaultKVShareMinTokens || cfg.MaxBytes != engine.DefaultKVShareGiB<<30 {
		t.Fatalf("defaults: %+v", cfg)
	}
	if _, ok := New().SessionShare(spec(t, shareSpec(path), "dir: /opt/strata\n")); ok {
		t.Fatal("no kv_share block shares nothing")
	}
}

func TestSaveAndImportSession(t *testing.T) {
	var got []string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]string
		_ = json.Unmarshal(b, &body)
		got = append(got, r.Method+" "+r.URL.RequestURI()+" "+body["session"]+" "+body["filename"]+" "+body["base"])
		w.WriteHeader(status)
		if status == http.StatusOK && body["base"] != "" {
			_, _ = w.Write([]byte(`{"save_id": "00000000000000bb", "base": "` + body["base"] + `", "delta": true}`))
			return
		}
		_, _ = w.Write([]byte(`{"error": {"message": "why"}}`))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	ctx := context.Background()
	e := New()
	if saved, err := e.SaveSession(ctx, addr, "sess 1", "00ab.bin", ""); err != nil || saved != (engine.SavedSession{}) {
		t.Fatal(saved, err)
	}
	if saved, err := e.SaveSession(ctx, addr, "sess 1", "00ab.bin", "00000000000000aa"); err != nil || saved != (engine.SavedSession{ID: "00000000000000bb", Delta: true}) {
		t.Fatal(saved, err)
	}
	if err := e.ImportSession(ctx, addr, "00ab.bin"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /slots/session?action=save sess 1 00ab.bin ",
		"POST /slots/session?action=save sess 1 00ab.bin 00000000000000aa",
		"POST /slots/0?action=import  00ab.bin ",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s", strings.Join(got, "\n"))
	}
	for _, code := range []int{http.StatusNotFound, http.StatusConflict} {
		status = code
		if _, err := e.SaveSession(ctx, addr, "s", "x.bin", ""); !errors.Is(err, engine.ErrSessionGone) {
			t.Fatalf("save %d: %v", code, err)
		}
		if err := e.ImportSession(ctx, addr, "x.bin"); err == nil || errors.Is(err, engine.ErrSessionGone) {
			t.Fatalf("import %d: %v", code, err)
		}
	}
	status = http.StatusNotImplemented
	if _, err := e.SaveSession(ctx, addr, "s", "x.bin", ""); err == nil || errors.Is(err, engine.ErrSessionGone) || !strings.Contains(err.Error(), "501 why") {
		t.Fatalf("save 501: %v", err)
	}
}

func TestSessionHeld(t *testing.T) {
	answer, status := `{"session": "s", "n_held": 51234}`, http.StatusOK
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = r.Method + " " + r.URL.RequestURI() + " " + string(b)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	ctx := context.Background()
	e := New()
	if n, err := e.SessionHeld(ctx, addr, "s", 0, 0); err != nil || n != 51234 {
		t.Fatal(n, err)
	}
	if got != `POST /slots/session?action=held {"session":"s"}` {
		t.Fatalf("request %q", got)
	}
	if n, err := e.SessionHeld(ctx, addr, "s", 50000, 0xab); err != nil || n != 51234 {
		t.Fatal(n, err)
	}
	if got != `POST /slots/session?action=held {"hash":"00000000000000ab","n":50000,"session":"s"}` {
		t.Fatalf("request by ids %q", got)
	}
	answer = `{"session": "s", "n_held": 0}`
	if n, err := e.SessionHeld(ctx, addr, "s", 0, 0); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	// a build without the query, and an answer without the count: cannot say
	answer, status = `{"error": {"message": "/slots/session supports action=save"}}`, http.StatusBadRequest
	if _, err := e.SessionHeld(ctx, addr, "s", 0, 0); err == nil {
		t.Fatal("an older build's 400 must be an error")
	}
	answer, status = `{}`, http.StatusOK
	if _, err := e.SessionHeld(ctx, addr, "s", 0, 0); err == nil {
		t.Fatal("no n_held must be an error")
	}
}
