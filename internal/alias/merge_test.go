package alias

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/janit/viiwork/v2/meshapi"
)

// E is a hand-built entry.
func E(ver uint64, ts int64, by, target string) meshapi.AliasEntry {
	return meshapi.AliasEntry{Target: target, Fallbacks: []string{}, Ver: ver, TS: ts, By: by}
}

func V(ts int64, by, target string) meshapi.AliasVersion {
	return meshapi.AliasVersion{Target: target, Fallbacks: []string{}, TS: ts, By: by}
}

func withHistory(e meshapi.AliasEntry, h ...meshapi.AliasVersion) meshapi.AliasEntry {
	e.History = h
	return e
}

// mergeStore is a store holding the given entries, with a counting writer.
func mergeStore(t *testing.T, entries map[string]meshapi.AliasEntry) (*Store, *int, *bool) {
	t.Helper()
	writes, fail := new(int), new(bool)
	writer := func(dir, name string, data []byte) error {
		if *fail {
			return errors.New("disk full")
		}
		*writes++
		return writeFileDurable(dir, name, data)
	}
	s, err := openWith(t.TempDir(), "gb1", newClock().now, writer)
	if err != nil {
		t.Fatal(err)
	}
	for name, e := range entries {
		s.table.Aliases[name] = copyEntry(e)
	}
	return s, writes, fail
}

func mustMerge(t *testing.T, s *Store, name string, remote meshapi.AliasEntry) (bool, *Conflict) {
	t.Helper()
	changed, conflict, err := s.Merge(name, remote)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	return changed, conflict
}

func TestMergeRule(t *testing.T) {
	t.Run("M1 no local entry", func(t *testing.T) {
		s, _, _ := mergeStore(t, nil)
		changed, conflict := mustMerge(t, s, "a", E(1, 100, "node-a", "A"))
		if got, _ := s.Get("a"); !changed || conflict != nil || got.Target != "A" || got.By != "node-a" {
			t.Errorf("changed=%v conflict=%v entry=%+v", changed, conflict, got)
		}
	})

	t.Run("M2 newer version, older clock", func(t *testing.T) {
		s, _, _ := mergeStore(t, map[string]meshapi.AliasEntry{"a": E(5, 100, "gb1", "A")})
		changed, conflict := mustMerge(t, s, "a", E(6, 50, "node-a", "B"))
		if got, _ := s.Get("a"); !changed || conflict != nil || got.Target != "B" || got.Ver != 6 {
			t.Errorf("changed=%v conflict=%v entry=%+v", changed, conflict, got)
		}
	})

	t.Run("M3 a history-less broadcast inherits the local history", func(t *testing.T) {
		h1 := V(10, "gb1", "old")
		s, _, _ := mergeStore(t, map[string]meshapi.AliasEntry{"a": withHistory(E(5, 100, "gb1", "A"), h1)})
		mustMerge(t, s, "a", E(6, 150, "node-a", "B"))
		got, _ := s.Get("a")
		if want := []meshapi.AliasVersion{V(100, "gb1", "A"), h1}; !reflect.DeepEqual(got.History, want) {
			t.Errorf("history = %+v, want %+v", got.History, want)
		}
	})

	t.Run("M4 tie won by the remote", func(t *testing.T) {
		s, _, _ := mergeStore(t, map[string]meshapi.AliasEntry{"a": E(5, 100, "gb1", "A")})
		changed, conflict := mustMerge(t, s, "a", E(5, 200, "node-a", "B"))
		got, _ := s.Get("a")
		if !changed || got.Target != "B" || conflict == nil || conflict.Name != "a" || conflict.Winner.By != "node-a" || conflict.Loser.By != "gb1" {
			t.Fatalf("changed=%v conflict=%+v entry=%+v", changed, conflict, got)
		}
		if len(got.History) == 0 || got.History[0].By != "gb1" || got.History[0].TS != 100 {
			t.Errorf("history = %+v, want the gb1 version first", got.History)
		}
	})

	t.Run("M5 M6 tie won by the local entry", func(t *testing.T) {
		s, _, _ := mergeStore(t, map[string]meshapi.AliasEntry{"a": E(5, 200, "node-a", "B")})
		changed, conflict := mustMerge(t, s, "a", E(5, 100, "gb1", "A"))
		got, _ := s.Get("a")
		if !changed || got.Target != "B" || conflict == nil || conflict.Winner.By != "node-a" || conflict.Loser.By != "gb1" ||
			len(got.History) != 1 || got.History[0].By != "gb1" || got.History[0].Target != "A" {
			t.Fatalf("M5: changed=%v conflict=%+v entry=%+v", changed, conflict, got)
		}
		changed, conflict = mustMerge(t, s, "a", E(5, 100, "gb1", "A"))
		if again, _ := s.Get("a"); changed || conflict == nil || len(again.History) != 1 {
			t.Errorf("M6: changed=%v conflict=%+v history=%+v", changed, conflict, again.History)
		}
	})

	t.Run("M7 equal clocks, larger By wins", func(t *testing.T) {
		s, _, _ := mergeStore(t, map[string]meshapi.AliasEntry{"a": E(5, 100, "gb1", "A")})
		mustMerge(t, s, "a", E(5, 100, "node-a", "B"))
		if got, _ := s.Get("a"); got.By != "node-a" {
			t.Errorf("entry = %+v", got)
		}
	})

	t.Run("M8 identical", func(t *testing.T) {
		s, writes, _ := mergeStore(t, map[string]meshapi.AliasEntry{"a": E(5, 100, "gb1", "A")})
		changed, conflict := mustMerge(t, s, "a", E(5, 100, "gb1", "A"))
		if changed || conflict != nil || *writes != 0 {
			t.Errorf("changed=%v conflict=%v writes=%d", changed, conflict, *writes)
		}
	})

	t.Run("M9 a newer tombstone wins", func(t *testing.T) {
		s, _, _ := mergeStore(t, map[string]meshapi.AliasEntry{"a": E(6, 100, "gb1", "A")})
		tomb := E(7, 150, "node-a", "")
		tomb.Deleted = true
		mustMerge(t, s, "a", tomb)
		if got, _ := s.Get("a"); !got.Deleted || got.Ver != 7 {
			t.Errorf("entry = %+v", got)
		}
	})

	t.Run("M14 invalid name", func(t *testing.T) {
		s, _, _ := mergeStore(t, nil)
		if changed, conflict, err := s.Merge("bad name", E(1, 1, "x", "A")); changed || conflict != nil || err != nil {
			t.Errorf("changed=%v conflict=%v err=%v", changed, conflict, err)
		}
		if len(s.Table().Aliases) != 0 {
			t.Error("the invalid entry was stored")
		}
	})
}

func TestMergeOrderIndependent(t *testing.T) {
	writes := []struct {
		name string
		e    meshapi.AliasEntry
	}{
		{"a", E(1, 100, "gb1", "A")},
		{"a", E(2, 200, "gb2", "B")},
		{"a", E(2, 150, "node-a", "C")},
		{"b", E(1, 120, "gb1", "D")},
		{"b", E(1, 120, "gb3", "E")},
	}
	orders := [][]int{{0, 1, 2, 3, 4}, {4, 3, 2, 1, 0}, {2, 4, 0, 3, 1}}
	var tables []map[string]meshapi.AliasEntry
	for _, order := range orders {
		s, _, _ := mergeStore(t, nil)
		for _, i := range order {
			mustMerge(t, s, writes[i].name, writes[i].e)
		}
		stripped := map[string]meshapi.AliasEntry{}
		for name, e := range s.Table().Aliases {
			e.History = nil
			stripped[name] = e
		}
		tables = append(tables, stripped)
	}
	for i := 1; i < len(tables); i++ {
		if !reflect.DeepEqual(tables[0], tables[i]) {
			t.Errorf("M10: order %v gives %+v, order %v gives %+v", orders[0], tables[0], orders[i], tables[i])
		}
	}
}

func TestMergeTable(t *testing.T) {
	t.Run("M11 wrong version", func(t *testing.T) {
		s, _, _ := mergeStore(t, nil)
		_, _, err := s.MergeTable(meshapi.AliasTable{V: 2, Aliases: map[string]meshapi.AliasEntry{"a": E(1, 1, "x", "A")}})
		if err == nil || len(s.Table().Aliases) != 0 {
			t.Errorf("err=%v table=%+v", err, s.Table())
		}
	})

	t.Run("M12 one persist", func(t *testing.T) {
		s, writes, _ := mergeStore(t, map[string]meshapi.AliasEntry{"c": E(5, 200, "node-a", "B")})
		changed, conflicts, err := s.MergeTable(meshapi.AliasTable{V: 1, Aliases: map[string]meshapi.AliasEntry{
			"b": E(1, 100, "gb2", "X"),
			"a": E(1, 100, "gb2", "Y"),
			"c": E(5, 100, "gb1", "A"),
		}})
		if err != nil || !reflect.DeepEqual(changed, []string{"a", "b", "c"}) || len(conflicts) != 1 || conflicts[0].Name != "c" || *writes != 1 {
			t.Errorf("changed=%v conflicts=%+v err=%v writes=%d", changed, conflicts, err, *writes)
		}
	})

	t.Run("rollback", func(t *testing.T) {
		s, _, fail := mergeStore(t, map[string]meshapi.AliasEntry{"c": E(1, 100, "gb1", "C")})
		*fail = true
		_, _, err := s.MergeTable(meshapi.AliasTable{V: 1, Aliases: map[string]meshapi.AliasEntry{"a": E(1, 1, "x", "A"), "c": E(2, 1, "x", "D")}})
		if got, _ := s.Get("c"); err == nil || len(s.Table().Aliases) != 1 || got.Target != "C" {
			t.Errorf("err=%v table=%+v", err, s.Table())
		}
	})
}

func TestMergePersistFailure(t *testing.T) {
	s, _, fail := mergeStore(t, map[string]meshapi.AliasEntry{"a": E(1, 100, "gb1", "A")})
	*fail = true
	if _, _, err := s.Merge("a", E(2, 200, "node-a", "B")); err == nil {
		t.Fatal("M13: a failed persist must fail the merge")
	}
	if got, _ := s.Get("a"); got.Target != "A" || got.Ver != 1 {
		t.Errorf("M13: entry = %+v", got)
	}
	if _, _, err := s.Merge("new", E(1, 1, "node-a", "N")); err == nil {
		t.Fatal("M13: a failed persist must fail the merge of a new name")
	}
	if _, ok := s.Get("new"); ok {
		t.Error("M13: the failed new entry is in memory")
	}
}

// Equal versions unite their histories (Decision 6, review finding 2).
func TestMergeEqualVersionHistories(t *testing.T) {
	h1, h2, h3 := V(10, "gb1", "h1"), V(50, "gb2", "h2"), V(90, "gb1", "h3")

	t.Run("M15 the broadcast copy gains the push/pull history", func(t *testing.T) {
		s, writes, _ := mergeStore(t, map[string]meshapi.AliasEntry{"a": E(5, 100, "gb1", "A")})
		changed, conflict := mustMerge(t, s, "a", withHistory(E(5, 100, "gb1", "A"), h2, h1))
		got, _ := s.Get("a")
		if !changed || conflict != nil || !reflect.DeepEqual(got.History, []meshapi.AliasVersion{h2, h1}) || *writes != 1 {
			t.Errorf("changed=%v conflict=%v history=%+v writes=%d", changed, conflict, got.History, *writes)
		}
	})

	t.Run("M16 two histories unite newest first", func(t *testing.T) {
		local, remote := withHistory(E(5, 100, "gb1", "A"), h3, h1), withHistory(E(5, 100, "gb1", "A"), h2, h1)
		s, _, _ := mergeStore(t, map[string]meshapi.AliasEntry{"a": local})
		changed, _ := mustMerge(t, s, "a", remote)
		got, _ := s.Get("a")
		want := []meshapi.AliasVersion{h3, h2, h1}
		if !changed || !reflect.DeepEqual(got.History, want) {
			t.Fatalf("changed=%v history=%+v", changed, got.History)
		}
		other, _, _ := mergeStore(t, map[string]meshapi.AliasEntry{"a": remote})
		mustMerge(t, other, "a", got)
		if back, _ := other.Get("a"); !reflect.DeepEqual(back.History, want) {
			t.Errorf("merged back: history = %+v", back.History)
		}
		if changed, _ := mustMerge(t, s, "a", got); changed {
			t.Error("merging the united history again reported a change")
		}
	})

	t.Run("M17 the union is capped", func(t *testing.T) {
		var a, b []meshapi.AliasVersion
		for i := 0; i < 10; i++ {
			a = append(a, V(int64(200-2*i), "gb1", fmt.Sprintf("a%d", i))) // 200, 198, ...
			b = append(b, V(int64(199-2*i), "gb2", fmt.Sprintf("b%d", i))) // 199, 197, ...
		}
		s, _, _ := mergeStore(t, map[string]meshapi.AliasEntry{"a": withHistory(E(5, 300, "gb1", "A"), a...)})
		mustMerge(t, s, "a", withHistory(E(5, 300, "gb1", "A"), b...))
		got, _ := s.Get("a")
		if len(got.History) != meshapi.MaxAliasHistory || got.History[0].TS != 200 || got.History[9].TS != 191 {
			t.Errorf("history = %+v", got.History)
		}
	})
}
