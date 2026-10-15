//go:build multiinstance

// Package multiinstance tests the application as several independent OS
// processes, each with its own connections and memory, against real PostgreSQL
// and LocalStack. Run with:
//
//	go test -tags multiinstance ./test/multiinstance/
//
// The challenge requires at least three independent instances, and in-process
// Fx applications do not count: here every instance is the compiled binary.
package multiinstance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GuilhermeFaglioni/backend-challenge-go/test/infra"
)

var (
	pg *infra.Postgres
	ls *infra.LocalStack
	// kc is needed because the application will not start without a reachable
	// identity provider to fetch its signing keys from.
	kc *infra.Keycloak

	// binary is the instrumented application executable shared by every test.
	binary string
	// coverDir receives the coverage data the processes write on a clean exit.
	coverDir string
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	buildDir, err := os.MkdirTemp("", "multiinstance-bin-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "temp dir: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(buildDir) }()

	// Coverage of the processes goes where the CI job collects it, or into a
	// throwaway directory on a local run.
	coverDir = os.Getenv("MULTIINSTANCE_COVERDIR")
	if coverDir == "" {
		coverDir = filepath.Join(buildDir, "cover")
	}
	if err := os.MkdirAll(coverDir, 0o750); err != nil {
		fmt.Fprintf(os.Stderr, "cover dir: %v\n", err)
		return 1
	}

	// Compile while the containers boot: both take a while.
	type built struct {
		path string
		err  error
	}
	buildDone := make(chan built, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		path, err := infra.BuildBinary(ctx, buildDir)
		buildDone <- built{path, err}
	}()

	return infra.RunSuite(m, infra.SuiteOptions{Keycloak: true}, func(s *infra.Shared) error {
		pg, ls, kc = s.PG, s.LS, s.KC
		b := <-buildDone
		if b.err != nil {
			return fmt.Errorf("building the application: %w", b.err)
		}
		binary = b.path
		return nil
	})
}
