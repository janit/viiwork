package meshapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidAliasName(t *testing.T) {
	for _, ok := range []string{"stable-coder", "stable.prose_v2", "A", strings.Repeat("a", 64)} {
		if !ValidAliasName(ok) {
			t.Errorf("%q must be valid", ok)
		}
	}
	for _, bad := range []string{"", "has space", "slash/name", "tab\there", strings.Repeat("a", 65)} {
		if ValidAliasName(bad) {
			t.Errorf("%q must be invalid", bad)
		}
	}
}

func TestCompareAliasEntries(t *testing.T) {
	cases := []struct {
		name string
		a, b AliasEntry
		want int
	}{
		{"higher ver beats a newer clock", AliasEntry{Ver: 6, TS: 100, By: "node-a"}, AliasEntry{Ver: 5, TS: 999, By: "gb1"}, 1},
		{"equal ver, newer ts wins", AliasEntry{Ver: 5, TS: 200, By: "gb1"}, AliasEntry{Ver: 5, TS: 100, By: "node-a"}, 1},
		{"equal ver and ts, larger node name wins", AliasEntry{Ver: 5, TS: 100, By: "gb1"}, AliasEntry{Ver: 5, TS: 100, By: "node-a"}, -1},
		{"identical keys tie", AliasEntry{Ver: 5, TS: 100, By: "gb1"}, AliasEntry{Ver: 5, TS: 100, By: "gb1"}, 0},
		{"newer tombstone beats an older set", AliasEntry{Ver: 7, Deleted: true, By: "gb1"}, AliasEntry{Ver: 6, Target: "Qwen3.8-27B", By: "node-a"}, 1},
	}
	for _, tc := range cases {
		if got := CompareAliasEntries(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: Compare(a, b) = %d, want %d", tc.name, got, tc.want)
		}
		if got := CompareAliasEntries(tc.b, tc.a); got != -tc.want {
			t.Errorf("%s: Compare(b, a) = %d, want %d (the rule must be antisymmetric)", tc.name, got, -tc.want)
		}
	}
}

func TestIsAliasConflict(t *testing.T) {
	a := AliasEntry{Ver: 5, TS: 100, By: "gb1"}
	if IsAliasConflict(a, a) {
		t.Error("an entry does not conflict with itself")
	}
	if !IsAliasConflict(a, AliasEntry{Ver: 5, TS: 101, By: "node-a"}) {
		t.Error("the same version from two different writes is a conflict")
	}
	if IsAliasConflict(a, AliasEntry{Ver: 6, TS: 90, By: "node-a"}) {
		t.Error("a newer version is an ordinary update, not a conflict")
	}
}

func TestAliasWireFields(t *testing.T) {
	assertFields(t, AliasTable{}, []string{"v", "aliases"})
	assertFields(t, AliasEntry{}, []string{"target", "fallbacks", "ver", "ts", "by", "deleted", "history"})
	assertFields(t, AliasVersion{}, []string{"target", "fallbacks", "ts", "by"})
	assertFields(t, AliasBroadcast{}, []string{"name", "entry"})
	assertFields(t, AliasesResponse{}, []string{"aliases"})
	assertFields(t, AliasInfo{}, []string{"name", "target", "fallbacks", "updated_at", "updated_by", "resolved", "state"})
	assertFields(t, AliasWriteRequest{}, []string{"target", "fallbacks", "force"})
}

func TestAliasInfoResolvedIsNullWhenUnavailable(t *testing.T) {
	b, err := json.Marshal(AliasInfo{Name: "stable-coder", Target: "Qwen3.8-27B", State: AliasStateUnavailable})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"resolved":null`) {
		t.Errorf("resolved must serialise as null when nothing is served, got %s", b)
	}
}
