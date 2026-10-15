package money_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// productionFiles parses the package's non-test sources.
func productionFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	require.NoError(t, err)

	files := map[string]*ast.File{}
	fset := token.NewFileSet()
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path) //nolint:gosec // the package's own source files
		require.NoError(t, err)
		file, err := parser.ParseFile(fset, path, src, 0)
		require.NoError(t, err)
		files[path] = file
	}
	require.NotEmpty(t, files, "no production files found: the test is not looking in the right place")
	return files
}

// Money must never touch floating point, not even to parse or print: a float
// cannot represent most decimal amounts exactly.
func TestNoFloatingPointAnywhereInThePackage(t *testing.T) {
	forbidden := map[string]bool{
		"float32": true, "float64": true, "complex64": true, "complex128": true,
		"ParseFloat": true, "FormatFloat": true, "AppendFloat": true,
		"Float32": true, "Float64": true, "Float": true, // math/big's Float and friends
		"NewFloat": true, "Float32bits": true, "Float64bits": true,
		"Float32frombits": true, "Float64frombits": true,
	}
	for path, file := range productionFiles(t) {
		ast.Inspect(file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				require.False(t, forbidden[x.Name], "%s uses %q", path, x.Name)
			case *ast.BasicLit:
				require.NotEqual(t, token.FLOAT, x.Kind, "%s has a floating-point literal %s", path, x.Value)
				require.NotEqual(t, token.IMAG, x.Kind, "%s has an imaginary literal %s", path, x.Value)
			}
			return true
		})
	}
}

// The domain stays independent of Fx, HTTP, SQS and the database. Nothing outside
// the standard library may be imported, and within it, nothing that reaches the
// network, the operating system, a database or C.
func TestDomainDependenciesAreForbidden(t *testing.T) {
	forbiddenStdlib := []string{"net", "os", "syscall", "database", "unsafe", "plugin", "C"}
	for path, file := range productionFiles(t) {
		for _, imp := range file.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)

			first, _, _ := strings.Cut(importPath, "/")
			require.NotContains(t, first, ".", "%s imports %q, which is not the standard library", path, importPath)
			require.NotContains(t, forbiddenStdlib, first, "%s imports %q: the domain must not reach the network, the OS, a database or C", path, importPath)
		}
	}
}
