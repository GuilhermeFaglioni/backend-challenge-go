package faultinject

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"
)

// Every Point constant declared in points.go must be returned by Points(),
// the list the `faultinject` build registers. Without this, a new constant
// that is forgotten there only fails when its Hit call runs.
func TestEveryDeclaredPointIsListed(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "points.go", nil, 0)
	if err != nil {
		t.Fatalf("parse points.go: %v", err)
	}

	var declared []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value := spec.(*ast.ValueSpec)
			ident, ok := value.Type.(*ast.Ident)
			if !ok || ident.Name != "Point" {
				continue
			}
			for _, expr := range value.Values {
				if lit, ok := expr.(*ast.BasicLit); ok {
					declared = append(declared, lit.Value[1:len(lit.Value)-1]) // strip the quotes
				}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no Point constants in points.go: the test is not looking in the right place")
	}

	var listed []string
	for _, p := range Points() {
		listed = append(listed, string(p))
	}
	slices.Sort(declared)
	slices.Sort(listed)
	if !slices.Equal(declared, listed) {
		t.Fatalf("declared points %v differ from Points() %v", declared, listed)
	}
}
