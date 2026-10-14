//go:build integration

// Package integration tests the application against real PostgreSQL,
// LocalStack and Keycloak containers. Run with:
//
//	go test -tags integration ./test/...
//
// The containers are started once for the package; tests that need isolation
// create their own database (infra.Postgres.NewDatabase) or their own
// containers.
package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"

	"github.com/GuilhermeFaglioni/backend-challenge-go/test/infra"
)

var (
	pg *infra.Postgres
	ls *infra.LocalStack
	kc *infra.Keycloak
)

// watchdogAfter is when the container logs are written if the run is still
// going. CI gives `go test` a -timeout just above it: a timeout kills the test
// binary without running anything after m.Run, so this is the only chance to
// keep the logs of a run that hung.
const watchdogAfter = 12 * time.Minute

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	hermeticAWSEnvironment()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	// Keycloak is the slowest to boot, so start everything together.
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	start := func(name string, fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				mu.Unlock()
			}
		}()
	}
	// The container handle is kept even when startup fails, so its logs can be
	// read and it can be terminated.
	start("postgres", func() (err error) { pg, err = infra.StartPostgres(ctx); return })
	start("localstack", func() (err error) { ls, err = infra.StartLocalStack(ctx); return })
	start("keycloak", func() (err error) { kc, err = infra.StartKeycloak(ctx); return })
	wg.Wait()

	dumpLogs := func() {
		dir := os.Getenv("INTEGRATION_LOG_DIR")
		if dir == "" {
			return
		}
		dumpCtx, dumpCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dumpCancel()
		containers := map[string]testcontainers.Container{}
		if pg != nil {
			containers["postgres"] = pg.Container
		}
		if ls != nil {
			containers["localstack"] = ls.Container
		}
		if kc != nil {
			containers["keycloak"] = kc.Container
		}
		for name, c := range containers {
			if err := infra.DumpLogs(dumpCtx, dir, name, c); err != nil {
				fmt.Fprintf(os.Stderr, "could not save %s logs: %v\n", name, err)
			}
		}
	}
	watchdog := time.AfterFunc(watchdogAfter, dumpLogs)
	defer watchdog.Stop()

	code := 1
	if err := errors.Join(errs...); err != nil {
		fmt.Fprintf(os.Stderr, "starting infrastructure: %v\n", err)
	} else {
		code = m.Run()
	}
	if code != 0 {
		dumpLogs()
	}

	terminateCtx, terminateCancel := context.WithTimeout(context.Background(), time.Minute)
	defer terminateCancel()
	if pg != nil {
		_ = pg.Container.Terminate(terminateCtx)
	}
	if ls != nil {
		_ = ls.Container.Terminate(terminateCtx)
	}
	if kc != nil {
		_ = kc.Container.Terminate(terminateCtx)
	}
	return code
}

// hermeticAWSEnvironment gives the AWS SDK fixed fake credentials and keeps it
// from reading the developer's own profile or the instance metadata service,
// so a test run depends on nothing outside this repository. LocalStack accepts
// any credentials.
func hermeticAWSEnvironment() {
	for key, value := range map[string]string{
		"AWS_ACCESS_KEY_ID":           "test",
		"AWS_SECRET_ACCESS_KEY":       "test",
		"AWS_SESSION_TOKEN":           "",
		"AWS_PROFILE":                 "",
		"AWS_CONFIG_FILE":             os.DevNull,
		"AWS_SHARED_CREDENTIALS_FILE": os.DevNull,
		"AWS_EC2_METADATA_DISABLED":   "true",
	} {
		_ = os.Setenv(key, value)
	}
}
