package alias

import (
	"encoding/json"
	"testing"

	"github.com/janit/viiwork/v2/meshapi"
)

// Both of these consume bytes that arrive from another mesh member, so their
// input is only as well-formed as that member chooses to make it. Neither may
// panic, and neither may leave the table in a state the store's own invariants
// reject — a replicated table that one member can corrupt is corrupt
// everywhere.
//
// Seeds cover the shapes worth starting from; the fuzzer explores around them.

func FuzzMergeRemoteState(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"aliases":{}}`))
	f.Add([]byte(`{"aliases":{"a":{"target":"m","ver":1,"ts":1,"by":"node-a"}}}`))
	f.Add([]byte(`{"aliases":{"a":{"target":"m","ver":18446744073709551615,"ts":-1,"by":""}}}`))
	f.Add([]byte(`{"aliases":{"":{"target":"","ver":0}}}`))
	f.Add([]byte(`{"aliases":{"../../etc/passwd":{"target":"m","ver":1}}}`))
	f.Add([]byte(`{"aliases":{"a":{"target":"m","ver":1,"deleted":true,"history":[{"ver":1}]}}}`))
	f.Add([]byte(`not json at all`))
	f.Add([]byte(``))
	f.Add([]byte(`null`))
	f.Add([]byte(`[]`))

	f.Fuzz(func(t *testing.T, buf []byte) {
		fx := newServiceFx(t)
		fx.svc.MergeRemoteState(buf, true)
		fx.svc.MergeRemoteState(buf, false)
		assertTableSane(t, fx.store)
	})
}

func FuzzNotifyMsg(f *testing.F) {
	seed := func(name string, e meshapi.AliasEntry) []byte {
		b, _ := json.Marshal(meshapi.AliasBroadcast{Name: name, Entry: e})
		return b
	}
	f.Add(seed("stable", meshapi.AliasEntry{Target: "m", Ver: 1, TS: 1, By: "node-a"}))
	f.Add(seed("", meshapi.AliasEntry{}))
	f.Add(seed("a", meshapi.AliasEntry{Target: "m", Ver: 1, Fallbacks: []string{"n", "n", ""}}))
	f.Add([]byte(`{"name":"a"}`))
	f.Add([]byte(`{`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, msg []byte) {
		fx := newServiceFx(t)
		fx.svc.NotifyMsg(msg)
		fx.svc.NotifyMsg(msg) // twice: a repeat must be idempotent, not additive
		assertTableSane(t, fx.store)
	})
}

// assertTableSane checks what must hold however hostile the input was.
func assertTableSane(t *testing.T, s *Store) {
	t.Helper()
	tbl := s.Table()
	if n := len(tbl.Aliases); n > meshapi.MaxAliases {
		t.Fatalf("table holds %d aliases, above the cap of %d: a member could grow it without bound", n, meshapi.MaxAliases)
	}
	for name, e := range tbl.Aliases {
		if name == "" {
			t.Fatal("an entry was stored under an empty name")
		}
		if !meshapi.ValidAliasName(name) {
			t.Fatalf("stored an alias whose name does not validate: %q", name)
		}
		if e.Target == "" && !e.Deleted {
			t.Fatalf("alias %q is live with no target", name)
		}
		// The table has to survive the round trip it will make on the wire.
		if _, err := json.Marshal(e); err != nil {
			t.Fatalf("alias %q does not marshal: %v", name, err)
		}
	}
}
