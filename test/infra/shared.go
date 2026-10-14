//go:build integration || multiinstance

package infra

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// Shared is the infrastructure one test package shares: containers started
// once, used by every test in it.
type Shared struct {
	PG *Postgres
	LS *LocalStack
	KC *Keycloak // nil unless SuiteOptions.Keycloak
}

// SuiteOptions selects what a package needs. Keycloak is the slowest to boot,
// so packages that do not exercise authentication leave it out.
type SuiteOptions struct {
	Keycloak bool
}

// watchdogAfter is when the container logs are written if a run is still going.
// CI gives `go test` a -timeout just above it: a timeout kills the test binary
// without running anything after m.Run, so this is the only chance to keep the
// logs of a run that hung.
const watchdogAfter = 12 * time.Minute

// RunSuite is the body of a package's TestMain: it starts the shared
// infrastructure, hands it to ready (which may fail the run), runs the tests
// and cleans up. When a test
// fails (or startup does), the container logs are written to
// INTEGRATION_LOG_DIR so CI can upload them. Use as:
//
//	func TestMain(m *testing.M) { os.Exit(infra.RunSuite(m, opts, func(s *infra.Shared) error { ...; return nil })) }
func RunSuite(m *testing.M, opts SuiteOptions, ready func(*Shared) error) int {
	for key, value := range HermeticAWSEnv() {
		_ = os.Setenv(key, value)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	shared, err := StartAll(ctx, opts)
	dumpLogs := func() { shared.DumpLogs(os.Getenv("INTEGRATION_LOG_DIR")) }
	watchdog := time.AfterFunc(watchdogAfter, dumpLogs)
	defer watchdog.Stop()

	code := 1
	if err != nil {
		fmt.Fprintf(os.Stderr, "starting infrastructure: %v\n", err)
	} else if err := ready(shared); err != nil {
		fmt.Fprintf(os.Stderr, "preparing the test run: %v\n", err)
	} else {
		code = m.Run()
	}
	if code != 0 {
		dumpLogs()
	}
	shared.Terminate()
	return code
}

// StartAll starts the containers in parallel. The returned value is never nil
// and holds every container that was created, even when another failed, so the
// caller can still read their logs and terminate them.
func StartAll(ctx context.Context, opts SuiteOptions) (*Shared, error) {
	s := &Shared{}
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
	start("postgres", func() (err error) { s.PG, err = StartPostgres(ctx); return })
	start("localstack", func() (err error) { s.LS, err = StartLocalStack(ctx); return })
	if opts.Keycloak {
		start("keycloak", func() (err error) { s.KC, err = StartKeycloak(ctx); return })
	}
	wg.Wait()
	return s, errors.Join(errs...)
}

func (s *Shared) containers() map[string]testcontainers.Container {
	containers := map[string]testcontainers.Container{}
	if s.PG != nil {
		containers["postgres"] = s.PG.Container
	}
	if s.LS != nil {
		containers["localstack"] = s.LS.Container
	}
	if s.KC != nil {
		containers["keycloak"] = s.KC.Container
	}
	return containers
}

// DumpLogs writes every container's log, and every application instance's,
// into dir. Nothing happens when dir is empty (a local run).
func (s *Shared) DumpLogs(dir string) {
	if dir == "" {
		return
	}
	// Instance processes (multi-instance layer), if any were started.
	if err := DumpInstanceLogs(dir); err != nil {
		fmt.Fprintf(os.Stderr, "could not save instance logs: %v\n", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for name, c := range s.containers() {
		if err := DumpLogs(ctx, dir, name, c); err != nil {
			fmt.Fprintf(os.Stderr, "could not save %s logs: %v\n", name, err)
		}
	}
}

// Terminate removes every container.
func (s *Shared) Terminate() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, c := range s.containers() {
		_ = c.Terminate(ctx)
	}
}

// HermeticAWSEnv is the environment that gives the AWS SDK fixed fake
// credentials and keeps it from reading the developer's own profile or the
// instance metadata service, so a run depends on nothing outside this
// repository. LocalStack accepts any credentials.
func HermeticAWSEnv() map[string]string {
	return map[string]string{
		"AWS_ACCESS_KEY_ID":           "test",
		"AWS_SECRET_ACCESS_KEY":       "test",
		"AWS_SESSION_TOKEN":           "",
		"AWS_PROFILE":                 "",
		"AWS_CONFIG_FILE":             os.DevNull,
		"AWS_SHARED_CREDENTIALS_FILE": os.DevNull,
		"AWS_EC2_METADATA_DISABLED":   "true",
	}
}
