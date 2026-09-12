package alias

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

const startMS = 1789000000000

type testClock struct {
	mu sync.Mutex
	ms int64
}

func newClock() *testClock { return &testClock{ms: startMS} }

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.UnixMilli(c.ms)
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.ms += d.Milliseconds()
	c.mu.Unlock()
}

func openTest(t *testing.T, dir string, clk *testClock) *Store {
	t.Helper()
	s, err := Open(dir, "gb1", clk.now)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustSet(t *testing.T, s *Store, name, target string, fallbacks ...string) meshapi.AliasEntry {
	t.Helper()
	e, err := s.Set(name, target, fallbacks)
	if err != nil {
		t.Fatalf("Set(%s): %v", name, err)
	}
	return e
}

// setupS3 is S2 followed by S3: two versions of stable-coder, one second apart.
func setupS3(t *testing.T) (*Store, *testClock, string) {
	t.Helper()
	dir, clk := t.TempDir(), newClock()
	s := openTest(t, dir, clk)
	mustSet(t, s, "stable-coder", "Qwen3.8-27B", "gemma-4-31B-it")
	clk.advance(time.Second)
	mustSet(t, s, "stable-coder", "gemma-4-31B-it")
	return s, clk, dir
}

func TestStoreOpenEmpty(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, newClock())
	if tb := s.Table(); tb.V != meshapi.AliasTableVersion || len(tb.Aliases) != 0 {
		t.Errorf("S1: table = %+v", tb)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("S1: directory holds %v, want nothing (no aliases.json yet, no probe left)", entries)
	}
}

func TestStoreSet(t *testing.T) {
	dir, clk := t.TempDir(), newClock()
	s := openTest(t, dir, clk)
	e := mustSet(t, s, "stable-coder", "Qwen3.8-27B", "gemma-4-31B-it")
	if e.Ver != 1 || e.TS != startMS || e.By != "gb1" || e.Deleted || len(e.History) != 0 || !reflect.DeepEqual(e.Fallbacks, []string{"gemma-4-31B-it"}) {
		t.Errorf("S2: entry = %+v", e)
	}
	if _, err := os.Stat(filepath.Join(dir, "aliases.json")); err != nil {
		t.Fatalf("S2: %v", err)
	}
	if again := openTest(t, dir, clk); !reflect.DeepEqual(again.Table(), s.Table()) {
		t.Errorf("S2: reopened table = %+v, want %+v", again.Table(), s.Table())
	}

	clk.advance(time.Second)
	e = mustSet(t, s, "stable-coder", "gemma-4-31B-it")
	want := []meshapi.AliasVersion{{Target: "Qwen3.8-27B", Fallbacks: []string{"gemma-4-31B-it"}, TS: startMS, By: "gb1"}}
	if e.Ver != 2 || !reflect.DeepEqual(e.History, want) || e.Fallbacks == nil || len(e.Fallbacks) != 0 {
		t.Errorf("S3: entry = %+v", e)
	}
}

func TestStoreHistoryCap(t *testing.T) {
	s := openTest(t, t.TempDir(), newClock())
	for i := 1; i <= 12; i++ {
		mustSet(t, s, "a", fmt.Sprintf("m%d", i))
	}
	e, _ := s.Get("a")
	if len(e.History) != meshapi.MaxAliasHistory || e.History[0].Target != "m11" || e.History[9].Target != "m2" {
		t.Errorf("S4: history = %+v", e.History)
	}
}

func TestStoreDelete(t *testing.T) {
	s, _, _ := setupS3(t)
	e, err := s.Delete("stable-coder")
	if err != nil || e.Ver != 3 || !e.Deleted || e.Target != "" || len(e.History) != 2 || e.History[0].Target != "gemma-4-31B-it" {
		t.Errorf("S5: %+v, %v", e, err)
	}
	if _, err := s.Delete("stable-coder"); !errors.Is(err, ErrNotFound) {
		t.Errorf("S6: err = %v", err)
	}
	if _, err := s.Delete("nosuch"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing name: err = %v", err)
	}
	if s.Live() != 0 {
		t.Errorf("Live = %d after delete", s.Live())
	}
}

func TestStoreRevert(t *testing.T) {
	t.Run("S7 S8 revert swaps", func(t *testing.T) {
		s, _, _ := setupS3(t)
		e, err := s.Revert("stable-coder")
		if err != nil || e.Ver != 3 || e.Target != "Qwen3.8-27B" || !reflect.DeepEqual(e.Fallbacks, []string{"gemma-4-31B-it"}) ||
			len(e.History) != 1 || e.History[0].Target != "gemma-4-31B-it" {
			t.Errorf("S7: %+v, %v", e, err)
		}
		e, err = s.Revert("stable-coder")
		if err != nil || e.Ver != 4 || e.Target != "gemma-4-31B-it" {
			t.Errorf("S8: %+v, %v", e, err)
		}
	})

	t.Run("S9 revert undeletes", func(t *testing.T) {
		s, _, _ := setupS3(t)
		if _, err := s.Delete("stable-coder"); err != nil {
			t.Fatal(err)
		}
		e, err := s.Revert("stable-coder")
		if err != nil || e.Ver != 4 || e.Deleted || e.Target != "gemma-4-31B-it" || len(e.History) != 1 || e.History[0].Target != "Qwen3.8-27B" {
			t.Errorf("S9: %+v, %v", e, err)
		}
	})

	t.Run("S10 nothing to revert to", func(t *testing.T) {
		s := openTest(t, t.TempDir(), newClock())
		mustSet(t, s, "a", "m")
		if _, err := s.Revert("a"); !errors.Is(err, ErrNoHistory) {
			t.Errorf("err = %v", err)
		}
		if _, err := s.Revert("nosuch"); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing name: err = %v", err)
		}
	})
}

func TestStoreInvalidName(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, newClock())
	if _, err := s.Set("bad name", "m", nil); err == nil {
		t.Error("S11: invalid name accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "aliases.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("S11: something was persisted: %v", err)
	}
}

func TestStoreTooMany(t *testing.T) {
	s := openTest(t, t.TempDir(), newClock())
	for i := 0; i < meshapi.MaxAliases; i++ {
		mustSet(t, s, fmt.Sprintf("a%03d", i), "m")
	}
	if _, err := s.Set("one-more", "m", nil); !errors.Is(err, ErrTooMany) {
		t.Errorf("S12: err = %v", err)
	}
	if _, err := s.Set("a000", "n", nil); err != nil {
		t.Errorf("S12: rewriting an existing alias: %v", err)
	}
}

func TestStorePersistFailure(t *testing.T) {
	dir, clk := t.TempDir(), newClock()
	var fail bool
	writer := func(dir, name string, data []byte) error {
		if fail {
			return errors.New("rename failed")
		}
		return writeFileDurable(dir, name, data)
	}
	s, err := openWith(dir, "gb1", clk.now, writer)
	if err != nil {
		t.Fatal(err)
	}
	before := mustSet(t, s, "a", "m")
	onDisk, _ := os.ReadFile(filepath.Join(dir, "aliases.json"))
	fail = true
	if _, err := s.Set("a", "n", nil); err == nil {
		t.Fatal("S13: a failed persist must fail the write")
	}
	if got, _ := s.Get("a"); !reflect.DeepEqual(got, before) {
		t.Errorf("S13: Get = %+v, want the previous %+v", got, before)
	}
	if _, err := s.Set("b", "m", nil); err == nil {
		t.Error("S13: a new alias must fail too")
	}
	if _, ok := s.Get("b"); ok {
		t.Error("S13: the failed new alias is in memory")
	}
	if now, _ := os.ReadFile(filepath.Join(dir, "aliases.json")); string(now) != string(onDisk) {
		t.Error("S13: the file on disk changed")
	}
}

func writeTable(t *testing.T, dir string, body string) string {
	t.Helper()
	path := filepath.Join(dir, "aliases.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStorePurgeOnOpen(t *testing.T) {
	dir, clk := t.TempDir(), newClock()
	day := int64(24 * time.Hour / time.Millisecond)
	writeTable(t, dir, fmt.Sprintf(`{"v":1,"aliases":{
		"old":{"target":"","fallbacks":[],"ver":2,"ts":%d,"by":"gb1","deleted":true},
		"recent":{"target":"","fallbacks":[],"ver":2,"ts":%d,"by":"gb1","deleted":true},
		"live":{"target":"m","fallbacks":[],"ver":1,"ts":%d,"by":"gb1","deleted":false}}}`,
		startMS-31*day, startMS-29*day, startMS-40*day))
	s := openTest(t, dir, clk)
	if _, ok := s.Get("old"); ok {
		t.Error("S14: a 31-day-old tombstone survived Open")
	}
	if _, ok := s.Get("recent"); !ok {
		t.Error("S14: a 29-day-old tombstone was purged")
	}
	if _, ok := s.Get("live"); !ok {
		t.Error("S14: an old live alias was purged")
	}
	if again := openTest(t, dir, clk); len(again.Table().Aliases) != 2 {
		t.Errorf("S14: the purge was not persisted: %+v", again.Table())
	}
}

func TestStoreRefusesBadFile(t *testing.T) {
	for _, body := range []string{"garbage{", `{"v":2,"aliases":{}}`} {
		dir := t.TempDir()
		path := writeTable(t, dir, body)
		if _, err := Open(dir, "gb1", newClock().now); err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("S15 %q: err = %v, want one naming %s", body, err, path)
		}
		if got, _ := os.ReadFile(path); string(got) != body {
			t.Errorf("S15 %q: the file was changed to %q", body, got)
		}
	}
}

func TestStoreUnwritableDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if _, err := Open(dir, "gb1", newClock().now); err == nil || !strings.Contains(err.Error(), dir) {
		t.Errorf("S16: err = %v, want one naming %s", err, dir)
	}
}

func TestStoreCopies(t *testing.T) {
	s, _, _ := setupS3(t)
	e, _ := s.Get("stable-coder")
	e.Fallbacks = append(e.Fallbacks, "x")
	e.History[0].Fallbacks[0] = "changed"
	e.History = append(e.History, meshapi.AliasVersion{Target: "y"})
	tb := s.Table()
	tb.Aliases["stable-coder"] = meshapi.AliasEntry{Target: "z"}
	got, _ := s.Get("stable-coder")
	if len(got.Fallbacks) != 0 || len(got.History) != 1 || got.History[0].Fallbacks[0] != "gemma-4-31B-it" || got.Target != "gemma-4-31B-it" {
		t.Errorf("S17: the store's entry changed through a copy: %+v", got)
	}
}
