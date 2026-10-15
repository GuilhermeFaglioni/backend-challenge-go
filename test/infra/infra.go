//go:build integration || multiinstance

// Package infra starts the real infrastructure the integration tests run
// against: PostgreSQL, LocalStack (SQS) and Keycloak, in containers. Nothing
// here is mocked. The multi-instance layer reuses it.
//
// Every Start function returns the container handle even when startup fails
// (as long as a container was created), so the caller can read its logs and
// terminate it.
package infra

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // pgx5:// driver
	_ "github.com/golang-migrate/migrate/v4/source/file"     // file:// source
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image versions are the ones docker-compose.yml runs, so the tests exercise
// what a reviewer runs.
const (
	postgresImage   = "postgres:16-alpine"
	localstackImage = "localstack/localstack:3.8.1"
	keycloakImage   = "quay.io/keycloak/keycloak:26.0.7"

	// Credentials created by deploy/postgres/init and the realm import. Local
	// test values that already live in the repository.
	ownerUser = "wagering_owner"
	ownerPass = "owner_local_pw" //nolint:gosec // local test credential, also in deploy/postgres and docker-compose.yml
	appUser   = "wagering_app"
	appPass   = "app_local_pw" //nolint:gosec // local test credential, also in deploy/postgres and docker-compose.yml
)

// Images returns the images the harness starts, by name. A test compares them
// with docker-compose.yml: Dependabot updates the compose file, and the two
// must not drift apart.
func Images() map[string]string {
	return map[string]string{
		"postgres":   postgresImage,
		"localstack": localstackImage,
		"keycloak":   keycloakImage,
	}
}

var (
	repoRootOnce sync.Once
	repoRootPath string
)

// RepoRoot returns the repository root: the closest parent of the working
// directory that holds go.mod. It does not depend on where the source file was
// compiled, so it also works with -trimpath.
func RepoRoot() string {
	repoRootOnce.Do(func() {
		dir, err := os.Getwd()
		if err != nil {
			panic(fmt.Sprintf("infra: working directory: %v", err))
		}
		for {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				repoRootPath = dir
				return
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				panic("infra: go.mod not found in any parent of the working directory")
			}
			dir = parent
		}
	})
	return repoRootPath
}

// endpoint returns the host and mapped port of a container's port.
func endpoint(ctx context.Context, c testcontainers.Container, port string) (host, mapped string, err error) {
	if host, err = c.Host(ctx); err != nil {
		return "", "", fmt.Errorf("container host: %w", err)
	}
	p, err := c.MappedPort(ctx, port)
	if err != nil {
		return "", "", fmt.Errorf("mapped port %s: %w", port, err)
	}
	return host, p.Port(), nil
}

// --- PostgreSQL ------------------------------------------------------------

type Postgres struct {
	Container *postgres.PostgresContainer
	host      string
	port      string
}

// StartPostgres runs PostgreSQL with the same role bootstrap compose uses.
func StartPostgres(ctx context.Context) (*Postgres, error) {
	c, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("wagering"),
		postgres.WithUsername(ownerUser),
		postgres.WithPassword(ownerPass),
		postgres.WithInitScripts(filepath.Join(RepoRoot(), "deploy", "postgres", "init", "01-roles.sql")),
		postgres.BasicWaitStrategies(),
	)
	if c == nil {
		return nil, fmt.Errorf("start postgres: %w", err)
	}
	p := &Postgres{Container: c}
	if err != nil {
		return p, fmt.Errorf("start postgres: %w", err)
	}
	if p.host, p.port, err = endpoint(ctx, c, "5432/tcp"); err != nil {
		return p, fmt.Errorf("postgres: %w", err)
	}
	return p, nil
}

func (p *Postgres) dsn(user, pass, db string) string {
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, pass),
		Host:     p.host + ":" + p.port,
		Path:     "/" + db,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

// Database is an isolated database inside the shared server. Roles are
// cluster-wide, so the application role works in every one of them.
type Database struct {
	OwnerDSN string
	AppDSN   string
}

// NewDatabase creates an empty database and drops it when the test ends.
func (p *Postgres) NewDatabase(ctx context.Context, t testing.TB) Database {
	t.Helper()
	name := "it_" + strings.ToLower(rand.Text()[:12])

	admin, err := pgx.Connect(ctx, p.dsn(ownerUser, ownerPass, "wagering"))
	if err != nil {
		t.Fatalf("connect as owner: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := pgx.Connect(cleanupCtx, p.dsn(ownerUser, ownerPass, "wagering"))
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(cleanupCtx) }()
		_, _ = conn.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	return Database{
		OwnerDSN: p.dsn(ownerUser, ownerPass, name),
		AppDSN:   p.dsn(appUser, appPass, name),
	}
}

// Migrator opens the repository's migrations against dsn. The caller closes it.
func Migrator(dsn string) (*migrate.Migrate, error) {
	source := "file://" + filepath.ToSlash(filepath.Join(RepoRoot(), "migrations"))
	return migrate.New(source, strings.Replace(dsn, "postgres://", "pgx5://", 1))
}

// Migrate applies every migration.
func Migrate(dsn string) error {
	m, err := Migrator(dsn)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

// --- LocalStack (SQS) ------------------------------------------------------

type LocalStack struct {
	Container testcontainers.Container
	Endpoint  string
}

// StartLocalStack runs LocalStack and its ready.d init script, the same one
// compose mounts, and waits until the script finished.
func StartLocalStack(ctx context.Context) (*LocalStack, error) {
	req := testcontainers.ContainerRequest{
		Image:        localstackImage,
		ExposedPorts: []string{"4566/tcp"},
		Env:          map[string]string{"SERVICES": "sqs,iam", "AWS_DEFAULT_REGION": "us-east-1"},
		Files: []testcontainers.ContainerFile{
			{
				HostFilePath:      filepath.Join(RepoRoot(), "deploy", "localstack", "init", "01-sqs.sh"),
				ContainerFilePath: "/etc/localstack/init/ready.d/01-sqs.sh",
				FileMode:          0o755,
			},
			{
				HostFilePath:      filepath.Join(RepoRoot(), "deploy", "localstack", "init", "02-iam.sh"),
				ContainerFilePath: "/etc/localstack/init/ready.d/02-iam.sh",
				FileMode:          0o755,
			},
			{
				HostFilePath:      filepath.Join(RepoRoot(), "deploy", "aws", "iam", "consumer-policy.json"),
				ContainerFilePath: "/etc/wagering/iam/consumer-policy.json",
				FileMode:          0o644,
			},
			{
				HostFilePath:      filepath.Join(RepoRoot(), "deploy", "aws", "iam", "publisher-policy.json"),
				ContainerFilePath: "/etc/wagering/iam/publisher-policy.json",
				FileMode:          0o644,
			},
		},
		WaitingFor: wait.ForHTTP("/_localstack/init/ready").
			WithPort("4566/tcp").
			WithResponseMatcher(func(body io.Reader) bool {
				var status struct {
					Completed bool `json:"completed"`
				}
				return json.NewDecoder(body).Decode(&status) == nil && status.Completed
			}).
			WithStartupTimeout(3 * time.Minute),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: req, Started: true})
	if c == nil {
		return nil, fmt.Errorf("start localstack: %w", err)
	}
	l := &LocalStack{Container: c}
	if err != nil {
		return l, fmt.Errorf("start localstack: %w", err)
	}
	host, port, err := endpoint(ctx, c, "4566/tcp")
	if err != nil {
		return l, fmt.Errorf("localstack: %w", err)
	}
	l.Endpoint = fmt.Sprintf("http://%s:%s", host, port)
	return l, nil
}

// --- Keycloak --------------------------------------------------------------

type Keycloak struct {
	Container testcontainers.Container
	baseURL   string
}

// StartKeycloak runs Keycloak with the repository's realm imported.
func StartKeycloak(ctx context.Context) (*Keycloak, error) {
	req := testcontainers.ContainerRequest{
		Image:        keycloakImage,
		Cmd:          []string{"start-dev", "--import-realm"},
		ExposedPorts: []string{"8080/tcp", "9000/tcp"},
		Env: map[string]string{
			"KC_BOOTSTRAP_ADMIN_USERNAME": "admin",
			"KC_BOOTSTRAP_ADMIN_PASSWORD": "admin",
			"KC_HEALTH_ENABLED":           "true",
			// The mapped port is random, so the issuer follows the request's host.
			"KC_HOSTNAME_STRICT": "false",
		},
		Files: []testcontainers.ContainerFile{{
			HostFilePath:      filepath.Join(RepoRoot(), "deploy", "keycloak", "wagering-realm.json"),
			ContainerFilePath: "/opt/keycloak/data/import/wagering-realm.json",
			FileMode:          0o644,
		}},
		WaitingFor: wait.ForHTTP("/health/ready").
			WithPort("9000/tcp").
			WithStartupTimeout(4 * time.Minute),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{ContainerRequest: req, Started: true})
	if c == nil {
		return nil, fmt.Errorf("start keycloak: %w", err)
	}
	k := &Keycloak{Container: c}
	if err != nil {
		return k, fmt.Errorf("start keycloak: %w", err)
	}
	host, port, err := endpoint(ctx, c, "8080/tcp")
	if err != nil {
		return k, fmt.Errorf("keycloak: %w", err)
	}
	k.baseURL = fmt.Sprintf("http://%s:%s", host, port)
	return k, nil
}

// IssuerURL is the realm URL tokens are issued for.
func (k *Keycloak) IssuerURL() string { return k.baseURL + "/realms/wagering" }

// TokenError is returned by Token when the token endpoint refuses the request.
type TokenError struct{ Status int }

func (e *TokenError) Error() string { return fmt.Sprintf("token endpoint answered %d", e.Status) }

var keycloakClient = &http.Client{Timeout: 15 * time.Second}

// AlternateIssuerURL is the same realm reached under another host name
// (127.0.0.1 instead of localhost, or the reverse). The test Keycloak derives the
// issuer from the request's host, so a token requested here carries a different
// `iss`: a real token from the wrong issuer.
func (k *Keycloak) AlternateIssuerURL() string {
	switch {
	case strings.Contains(k.baseURL, "localhost"):
		return strings.Replace(k.IssuerURL(), "localhost", "127.0.0.1", 1)
	case strings.Contains(k.baseURL, "127.0.0.1"):
		return strings.Replace(k.IssuerURL(), "127.0.0.1", "localhost", 1)
	}
	return k.IssuerURL()
}

// Token performs a client_credentials grant and returns the access token.
func (k *Keycloak) Token(ctx context.Context, clientID, secret string) (string, error) {
	return k.TokenAt(ctx, k.IssuerURL(), clientID, secret)
}

// TokenAt does the same against the realm as reached at realmURL.
func (k *Keycloak) TokenAt(ctx context.Context, realmURL, clientID, secret string) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {secret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, realmURL+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := keycloakClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", &TokenError{Status: resp.StatusCode}
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	return body.AccessToken, nil
}

// --- diagnostics -----------------------------------------------------------

// DumpLogs writes a container's logs to <dir>/<name>.log. Used on failure so CI
// can upload them. Logs hold only local test credentials; the application
// never logs tokens (ARCHITECTURE.md, section 15).
func DumpLogs(ctx context.Context, dir, name string, c testcontainers.Container) (err error) {
	if dir == "" || c == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // directory chosen by the CI environment, not by input
		return err
	}
	logs, err := c.Logs(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = logs.Close() }()
	// The directory comes from the CI environment, not from user input.
	out, err := os.Create(filepath.Join(dir, name+".log")) //nolint:gosec // path chosen by the test runner
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	_, err = io.Copy(out, logs)
	return err
}
