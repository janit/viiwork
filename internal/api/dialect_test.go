package api

import (
	"net/http"
	"testing"
)

type stub struct {
	name  string
	paths []string
}

func (s stub) Name() string    { return s.name }
func (s stub) Paths() []string { return s.paths }
func (s stub) Decode(*http.Request, []byte) (Request, bool, error) {
	return Request{}, true, nil
}

func TestRegisterAndLookup(t *testing.T) {
	Register(stub{name: "a", paths: []string{"/a/one", "/a/two"}})
	for _, p := range []string{"/a/one", "/a/two"} {
		d, ok := Lookup(p)
		if !ok || d.Name() != "a" {
			t.Errorf("Lookup(%q) = %v, %v", p, d, ok)
		}
	}
	if _, ok := Lookup("/nowhere"); ok {
		t.Error("Lookup of an unowned path must report false")
	}
}

// A path two dialects both claim must not resolve to whichever package
// happened to load first, so it panics at init like a duplicate engine name.
func TestRegisterPanicsOnCollision(t *testing.T) {
	Register(stub{name: "holder", paths: []string{"/held"}})
	for _, tc := range []struct {
		name string
		d    Dialect
	}{
		{"duplicate path", stub{name: "other", paths: []string{"/held"}}},
		{"duplicate name", stub{name: "holder", paths: []string{"/elsewhere"}}},
		{"empty name", stub{name: "", paths: []string{"/x"}}},
		{"no paths", stub{name: "pathless"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("Register must panic")
				}
			}()
			Register(tc.d)
		})
	}
}
