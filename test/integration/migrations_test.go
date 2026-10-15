//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/test/infra"
)

// sqlState extracts the PostgreSQL error code, or "" when err is not one.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

const (
	insufficientPrivilege = "42501"
)

func connect(ctx context.Context, t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func TestMigrationsApplyRevertAndReapply(t *testing.T) {
	ctx := t.Context()
	db := pg.NewDatabase(ctx, t)

	m, err := infra.Migrator(db.OwnerDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })

	require.NoError(t, m.Up(), "apply all migrations")
	version, dirty, err := m.Version()
	require.NoError(t, err)
	require.False(t, dirty)
	require.NotZero(t, version)

	require.NoError(t, m.Down(), "revert all migrations")
	_, _, err = m.Version()
	require.ErrorIs(t, err, migrate.ErrNilVersion, "nothing is applied after reverting everything")

	require.NoError(t, m.Up(), "apply again after reverting")
	version2, dirty, err := m.Version()
	require.NoError(t, err)
	require.False(t, dirty)
	require.Equal(t, version, version2)
}

// The application role must only hold the privileges the migrations grant: it
// reads and writes ordinary tables, cannot change the schema and cannot TRUNCATE,
// and may only read and append to the ledger (the protection ARCHITECTURE.md relies
// on). Both run against the real tables the migrations create.
func TestApplicationRolePrivileges(t *testing.T) {
	ctx := t.Context()
	db := pg.NewDatabase(ctx, t)
	require.NoError(t, infra.Migrate(db.OwnerDSN))

	owner := connect(ctx, t, db.OwnerDSN)
	wallet, player := uuid.New(), uuid.New()
	_, err := owner.Exec(ctx, `INSERT INTO wallets (id, player_id, currency, balance_minor, version, created_at, updated_at) VALUES ($1, $2, 'BRL', 0, 1, now(), now())`, wallet, player)
	require.NoError(t, err)

	app := connect(ctx, t, db.AppDSN)

	t.Run("reads and writes ordinary tables", func(t *testing.T) {
		_, err := app.Exec(ctx, `UPDATE wallets SET updated_at = now() WHERE id = $1`, wallet)
		require.NoError(t, err)
		var count int
		require.NoError(t, app.QueryRow(ctx, `SELECT count(*) FROM wallets WHERE id = $1`, wallet).Scan(&count))
		require.Equal(t, 1, count)
		_, err = app.Exec(ctx, `INSERT INTO outbox (event_id, aggregate_id, event_type, version, payload, occurred_at) VALUES (gen_random_uuid(), $1, 'X', 1, '{}', now())`, wallet)
		require.NoError(t, err)
	})

	denied := map[string]string{
		"cannot truncate":                                             `TRUNCATE wallets`,
		"cannot drop a table":                                         `DROP TABLE wallets`,
		"cannot create a table":                                       `CREATE TABLE intruder (id int)`,
		"cannot alter a table":                                        `ALTER TABLE wallets ADD COLUMN extra int`,
		"cannot update the ledger":                                    `UPDATE wallet_ledger_entries SET created_at = now()`,
		"cannot delete from the ledger":                               `DELETE FROM wallet_ledger_entries`,
		"cannot truncate the ledger":                                  `TRUNCATE wallet_ledger_entries`,
		"cannot disable the ledger triggers":                          `ALTER TABLE wallet_ledger_entries DISABLE TRIGGER ALL`,
		"deletes from the outbox only through the retention function": `DELETE FROM outbox`,
	}
	for name, statement := range denied {
		t.Run(name, func(t *testing.T) {
			_, err := app.Exec(ctx, statement)
			require.Error(t, err)
			require.Equal(t, insufficientPrivilege, sqlState(err), "want permission denied, got: %v", err)
		})
	}
}

// Reverting must take the grants back, not just the schema objects.
func TestRevertingMigrationsRevokesApplicationAccess(t *testing.T) {
	ctx := t.Context()
	db := pg.NewDatabase(ctx, t)
	require.NoError(t, infra.Migrate(db.OwnerDSN))

	m, err := infra.Migrator(db.OwnerDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })
	require.NoError(t, m.Down())

	owner := connect(ctx, t, db.OwnerDSN)
	_, err = owner.Exec(ctx, `CREATE TABLE after_revert (id int)`)
	require.NoError(t, err)

	app := connect(ctx, t, db.AppDSN)
	_, err = app.Exec(ctx, `SELECT * FROM after_revert`)
	require.Error(t, err)
	require.Equal(t, insufficientPrivilege, sqlState(err), "got: %v", err)
}
