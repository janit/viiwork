package mesh

import (
	"errors"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/meshapi"
)

func TestEncodeMetaExactBytes(t *testing.T) {
	b, err := EncodeMeta(NodeMeta{V: MetaVersion, API: 8086, Ver: "2.0.0", Role: meshapi.RoleNode})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"v":2,"api":8086,"ver":"2.0.0","role":"node"}`; string(b) != want {
		t.Errorf("EncodeMeta = %s, want %s", b, want)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	in := NodeMeta{V: MetaVersion, API: 8090, Ver: "2.0.0-alpha.1", Role: meshapi.RoleGateway}
	b, err := EncodeMeta(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeMeta(b)
	if err != nil || out != in {
		t.Errorf("round trip = %+v, %v; want %+v", out, err, in)
	}
}

func TestEncodeMetaRejects(t *testing.T) {
	cases := []struct {
		name string
		m    NodeMeta
		want string
	}{
		{"wrong version", NodeMeta{V: 1, API: 8086, Role: meshapi.RoleNode}, "not viiwork v2"},
		{"api port", NodeMeta{V: MetaVersion, API: 0, Role: meshapi.RoleNode}, "api port"},
		{"unknown role", NodeMeta{V: MetaVersion, API: 8086, Role: "collector"}, "unknown role"},
		{"too large", NodeMeta{V: MetaVersion, API: 8086, Ver: strings.Repeat("x", 600), Role: meshapi.RoleNode}, "memberlist allows 512"},
	}
	for _, tc := range cases {
		if _, err := EncodeMeta(tc.m); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", tc.name, err, tc.want)
		}
	}
}

func TestDecodeMetaRejectsOtherVersionsAndGarbage(t *testing.T) {
	if _, err := DecodeMeta([]byte(`{"v":1,"api":8086,"ver":"1.8.1","role":"node"}`)); !errors.Is(err, ErrMetaVersion) {
		t.Errorf("v1 metadata: err = %v, want ErrMetaVersion", err)
	}
	if _, err := DecodeMeta([]byte("not json")); err == nil {
		t.Error("garbage must be rejected")
	}
}
