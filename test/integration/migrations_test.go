//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"
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
	undefinedTable        = "42P01"
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
// reads and writes ordinary tables, cannot change the schema and cannot
// TRUNCATE (the protection ARCHITECTURE.md relies on for the append-only
// ledger). Tables created by the owner after the migrations are the ones the
// default privileges are meant to cover.
//
// The migrations create no tables yet, so this uses throwaway tables. When the
// wallet, transaction and ledger tables land, point the "append-only" cases at
// the real ledger table: the REVOKE below is then the migration's, not the
// test's.
func TestApplicationRolePrivileges(t *testing.T) {
	ctx := t.Context()
	db := pg.NewDatabase(ctx, t)
	require.NoError(t, infra.Migrate(db.OwnerDSN))

	owner := connect(ctx, t, db.OwnerDSN)
	_, err := owner.Exec(ctx, `CREATE TABLE probe (id int PRIMARY KEY, note text)`)
	require.NoError(t, err)
	// Same shape as the ledger will have: writes allowed, edits and deletes not.
	_, err = owner.Exec(ctx, `CREATE TABLE append_only (id int PRIMARY KEY)`)
	require.NoError(t, err)
	_, err = owner.Exec(ctx, `REVOKE UPDATE, DELETE ON append_only FROM wagering_app`)
	require.NoError(t, err)

	app := connect(ctx, t, db.AppDSN)

	t.Run("reads and writes ordinary tables", func(t *testing.T) {
		_, err := app.Exec(ctx, `INSERT INTO probe VALUES (1, 'a')`)
		require.NoError(t, err)
		_, err = app.Exec(ctx, `UPDATE probe SET note = 'b' WHERE id = 1`)
		require.NoError(t, err)
		var note string
		require.NoError(t, app.QueryRow(ctx, `SELECT note FROM probe WHERE id = 1`).Scan(&note))
		require.Equal(t, "b", note)
		_, err = app.Exec(ctx, `DELETE FROM probe WHERE id = 1`)
		require.NoError(t, err)
	})

	denied := map[string]string{
		"cannot truncate":                    `TRUNCATE probe`,
		"cannot drop a table":                `DROP TABLE probe`,
		"cannot create a table":              `CREATE TABLE intruder (id int)`,
		"cannot alter a table":               `ALTER TABLE probe ADD COLUMN extra int`,
		"cannot update an append-only table": `UPDATE append_only SET id = 2`,
		"cannot delete from append-only":     `DELETE FROM append_only`,
	}
	for name, statement := range denied {
		t.Run(name, func(t *testing.T) {
			_, err := app.Exec(ctx, statement)
			require.Error(t, err)
			require.Equal(t, insufficientPrivilege, sqlState(err), "want permission denied, got: %v", err)
		})
	}

	t.Run("still inserts into the append-only table", func(t *testing.T) {
		_, err := app.Exec(ctx, `INSERT INTO append_only VALUES (1)`)
		require.NoError(t, err)
	})
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
