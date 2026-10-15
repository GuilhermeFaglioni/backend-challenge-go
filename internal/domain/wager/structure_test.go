package wager_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The domain depends on nothing but the standard library, a UUID type and the other
// domain packages: not Fx, HTTP, SQS, the database or the application layers.
func TestTheDomainDependsOnNothingButTheDomainAndTheStandardLibrary(t *testing.T) {
	allowed := map[string]bool{
		"bytes": true, "crypto/sha256": true, "encoding/hex": true, "encoding/json": true,
		"errors": true, "fmt": true, "regexp": true, "slices": true, "strings": true, "time": true,
		"unicode": true, "unicode/utf8": true,
		"github.com/google/uuid": true,
		"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money":  true,
		"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet": true,
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
