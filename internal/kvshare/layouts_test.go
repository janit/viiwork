package kvshare

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestRegisterLayoutDispatchesByMagic(t *testing.T) {
	const magic = "TESTFMT\x00"
	called := false
	RegisterLayout(magic, func(r io.ReaderAt, size int64) (*Layout, error) {
		called = true
		return &Layout{Size: size, Sections: []Section{{Name: "all", Off: int64(len(magic)), Len: size - int64(len(magic))}}}, nil
	})
	b := append([]byte(magic), make([]byte, 100)...)
	l, err := ParseLayout(bytes.NewReader(b), int64(len(b)))
	if err != nil || !called {
		t.Fatalf("registered format not used: %v", err)
	}
	if len(l.Sections) != 1 || l.Sections[0].Len != 100 {
		t.Fatalf("layout %+v", l)
	}
	// Strata's format still parses through the same entry point.
	s := fakeSession{model: 1, config: 2, cells: 100, stateSeed: 1}.bytes()
	if _, err := ParseLayout(bytes.NewReader(s), int64(len(s))); err != nil {
		t.Fatalf("STRSESS: %v", err)
	}
	if _, err := ParseLayout(bytes.NewReader([]byte("nothing known here")), 18); err == nil || !strings.Contains(err.Error(), "not a session file") {
		t.Fatalf("unknown magic: %v", err)
	}
}

func TestRegisterLayoutRefusesDuplicates(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registering a magic twice must panic")
		}
	}()
	RegisterLayout(sessMagic, func(io.ReaderAt, int64) (*Layout, error) { return nil, nil })
}
