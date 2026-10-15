package wallet_test

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

// The domain depends on nothing but the standard library, a UUID type and Money:
// not Fx, HTTP, SQS, the database or the application layers (ARCHITECTURE.md §3).
// The allowlist, rather than a list of forbidden packages, means a new kind of
// dependency has to be argued for here.
func TestTheDomainDependsOnNothingButMoneyAndTheStandardLibrary(t *testing.T) {
	allowed := map[string]bool{
		"errors": true, "fmt": true, "time": true, "math": true,
		"github.com/google/uuid": true,
		"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money": true,
	}
	paths, err := filepath.Glob("*.go")
	require.NoError(t, err)
	checked := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path) //nolint:gosec // the package's own sources
		require.NoError(t, err)
		file, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
		require.NoError(t, err)
		checked++
		for _, imp := range file.Imports {
			name, err := strconv.Unquote(imp.Path.Value)
			require.NoError(t, err)
			require.True(t, allowed[name], "%s imports %q, which the domain must not depend on", path, name)
		}
	}
	require.NotZero(t, checked, "no files were checked: the test is not looking in the right place")
}

// Money is the only way to hold an amount: a bare integer or float named like one
// would bypass its overflow and currency checks.
func TestNoFloatingPointInTheDomain(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path) //nolint:gosec // the package's own sources
		require.NoError(t, err)
		file, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
		require.NoError(t, err)
		ast.Inspect(file, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && (id.Name == "float32" || id.Name == "float64") {
				t.Errorf("%s uses %s: amounts are money.Money", path, id.Name)
			}
			return true
		})
	}
}
