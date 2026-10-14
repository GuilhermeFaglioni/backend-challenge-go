//go:build integration

// Package integration tests the application against real PostgreSQL,
// LocalStack and Keycloak containers. Run with:
//
//	go test -tags integration ./test/integration/
//
// The containers are started once for the package; tests that need isolation
// create their own database (infra.Postgres.NewDatabase) or their own
// containers.
package integration

import (
	"os"
	"testing"

	"github.com/GuilhermeFaglioni/backend-challenge-go/test/infra"
)

var (
	pg *infra.Postgres
	ls *infra.LocalStack
	kc *infra.Keycloak
)

func TestMain(m *testing.M) {
	os.Exit(infra.RunSuite(m, infra.SuiteOptions{Keycloak: true}, func(s *infra.Shared) error {
		pg, ls, kc = s.PG, s.LS, s.KC
		return nil
	}))
}
