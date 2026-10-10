package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capabilities.go claims to hold every optional capability, and the engine
// author's guide lists them: an interface declared anywhere else in the
// package, or missing from the guide, is one an engine author will not find.
func TestEveryCapabilityIsInCapabilitiesGoAndTheGuide(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "adding-an-engine.md"))
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				if _, ok := ts.Type.(*ast.InterfaceType); !ok || ts.Name.Name == "Engine" {
					continue
				}
				found++
				if name != "capabilities.go" {
					t.Errorf("capability %s is declared in %s, not capabilities.go", ts.Name.Name, name)
				}
				if !strings.Contains(string(guide), "type "+ts.Name.Name+" interface") {
					t.Errorf("capability %s is not documented in docs/adding-an-engine.md", ts.Name.Name)
				}
			}
		}
	}
	if found == 0 {
		t.Fatal("no capability interfaces found")
	}
}

type namedEngine struct {
	fakeEngine
	display string
}

func (e namedEngine) DisplayName() string { return e.display }

func TestDisplayName(t *testing.T) {
	Register(namedEngine{fakeEngine{name: "displaytest"}, "Display Test"})
	Register(namedEngine{fakeEngine{name: "displayblank"}, "  "})
	Register(fakeEngine{name: "displaynone"})
	if got := DisplayName("displaytest"); got != "Display Test" {
		t.Errorf("DisplayName of a declaring engine = %q, want its own name", got)
	}
	if got := DisplayName("displayblank"); got != "displayblank" {
		t.Errorf("DisplayName of an engine with a blank name = %q, want the id", got)
	}
	if got := DisplayName("displaynone"); got != "displaynone" {
		t.Errorf("DisplayName of an engine that declares none = %q, want the id", got)
	}
	if got := DisplayName("no-such-engine"); got != "no-such-engine" {
		t.Errorf("DisplayName of an unregistered id = %q, want the id", got)
	}
}
