//go:build integration

package integration

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/postgres"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

// appPool connects as the application role, so these tests also prove the role has
// what the real flows need.
func appPool(ctx context.Context, t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func count(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n))
	return n
}

func emptyWallet(t *testing.T, player uuid.UUID) wallet.Wallet {
	t.Helper()
	w, err := wallet.Open(uuid.New(), player, money.BRL, time.Now())
	require.NoError(t, err)
	return w
}

func newUnitOfWork(pool *pgxpool.Pool) application.UnitOfWork { return postgres.NewUnitOfWork(pool) }

func noWait(int) time.Duration { return time.Millisecond }

type uowEnv struct {
	pool *pgxpool.Pool
	uow  *postgres.UnitOfWork
}

func newUowEnv(t *testing.T) uowEnv {
	t.Helper()
	ctx := t.Context()
	db := pg.NewDatabase(ctx, t)
	require.NoError(t, migrateDatabase(db))
	pool := appPool(ctx, t, db.AppDSN)
	return uowEnv{pool: pool, uow: postgres.NewUnitOfWork(pool).WithRetry(3, noWait)}
}

func TestUnitOfWorkCommitsWhatTheFunctionWrote(t *testing.T) {
	e := newUowEnv(t)
	err := e.uow.Do(t.Context(), func(ctx context.Context, repos application.Repositories) error {
		return repos.Wallets().Insert(ctx, emptyWallet(t, uuid.New()))
	})
	require.NoError(t, err)
	require.Equal(t, 1, count(t.Context(), t, e.pool, "wallets"))
}

func TestUnitOfWorkRollsBackOnError(t *testing.T) {
	e := newUowEnv(t)
	boom := errors.New("boom")
	err := e.uow.Do(t.Context(), func(ctx context.Context, repos application.Repositories) error {
		require.NoError(t, repos.Wallets().Insert(ctx, emptyWallet(t, uuid.New())))
		return boom
	})
	require.ErrorIs(t, err, boom)
	require.Zero(t, count(t.Context(), t, e.pool, "wallets"), "nothing written before the error is kept")
}

func TestUnitOfWorkRollsBackOnPanicAndLetsItContinue(t *testing.T) {
	e := newUowEnv(t)
	require.PanicsWithValue(t, "kaboom", func() {
		_ = e.uow.Do(t.Context(), func(ctx context.Context, repos application.Repositories) error {
			require.NoError(t, repos.Wallets().Insert(ctx, emptyWallet(t, uuid.New())))
			panic("kaboom")
		})
	})
	require.Zero(t, count(t.Context(), t, e.pool, "wallets"))
	// The connection went back to the pool clean: the next unit of work is fine.
	require.NoError(t, e.uow.Do(t.Context(), func(ctx context.Context, repos application.Repositories) error {
		return repos.Wallets().Insert(ctx, emptyWallet(t, uuid.New()))
	}))
	require.Equal(t, 1, count(t.Context(), t, e.pool, "wallets"))
}

func pgError(code string) error {
	return &pgconn.PgError{Code: code, Message: "injected"}
}

func TestUnitOfWorkRetriesSerializationFailuresAndDeadlocks(t *testing.T) {
	for _, code := range []string{"40001", "40P01"} {
		t.Run(code, func(t *testing.T) {
			e := newUowEnv(t)
			player := uuid.New()
			var attempts int
			err := e.uow.Do(t.Context(), func(ctx context.Context, repos application.Repositories) error {
				attempts++
				require.NoError(t, repos.Wallets().Insert(ctx, emptyWallet(t, player)))
				if attempts < 3 {
					return pgError(code)
				}
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, 3, attempts)
			require.Equal(t, 1, count(t.Context(), t, e.pool, "wallets"), "only the attempt that succeeded is kept")
		})
	}
}

func TestUnitOfWorkGivesUpAfterTheBound(t *testing.T) {
	e := newUowEnv(t)
	var attempts int
	err := e.uow.Do(t.Context(), func(context.Context, application.Repositories) error {
		attempts++
		return pgError("40001")
	})
	require.Error(t, err)
	require.Equal(t, "40001", sqlState(err), "the last error is reported as it was")
	require.Equal(t, 3, attempts, "bounded")
}

func TestUnitOfWorkDoesNotRetryWhatRetryingCannotFix(t *testing.T) {
	e := newUowEnv(t)
	for name, failure := range map[string]error{
		"a plain error":          errors.New("boom"),
		"a unique violation":     pgError("23505"),
		"a check violation":      pgError("23514"),
		"a business conflict":    &application.WalletAlreadyExistsError{WalletID: uuid.New()},
		"insufficient privilege": pgError("42501"),
	} {
		var attempts int
		err := e.uow.Do(t.Context(), func(context.Context, application.Repositories) error {
			attempts++
			return failure
		})
		require.ErrorIs(t, err, failure, name)
		require.Equal(t, 1, attempts, name)
	}
}

func TestUnitOfWorkHonoursContextCancellation(t *testing.T) {
	e := newUowEnv(t)

	t.Run("already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		called := false
		err := e.uow.Do(ctx, func(context.Context, application.Repositories) error {
			called = true
			return nil
		})
		require.ErrorIs(t, err, context.Canceled)
		require.False(t, called, "no work is started for a caller that is gone")
	})

	t.Run("while waiting to retry", func(t *testing.T) {
		slow := e.uow.WithRetry(5, func(int) time.Duration { return time.Minute })
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		var attempts int
		err := slow.Do(ctx, func(context.Context, application.Repositories) error {
			attempts++
			return pgError("40001")
		})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, "40001", sqlState(err), "and the reason it was retrying is not lost")
		require.Less(t, time.Since(start), 5*time.Second, "the wait for the retry was cut short")
		require.Equal(t, 1, attempts)
	})
}

// Two units of work insert the same two keys in opposite orders, which PostgreSQL
// can only resolve by aborting one with a deadlock. The unit of work retries it, so
// the caller never sees the 40P01.
func TestARealDeadlockIsRetriedTransparently(t *testing.T) {
	e := newUowEnv(t)
	playerX, playerY := uuid.New(), uuid.New()

	var firstInserts sync.WaitGroup
	firstInserts.Add(2)
	var deadlocked atomic.Bool
	run := func(first, second uuid.UUID) error {
		var attempt int
		return e.uow.Do(t.Context(), func(ctx context.Context, repos application.Repositories) error {
			attempt++
			if err := repos.Wallets().Insert(ctx, emptyWallet(t, first)); err != nil {
				return err
			}
			if attempt == 1 {
				firstInserts.Done()
				firstInserts.Wait() // both hold their first key before asking for the second
			}
			err := repos.Wallets().Insert(ctx, emptyWallet(t, second))
			if sqlState(err) == "40P01" {
				deadlocked.Store(true)
			}
			return err
		})
	}

	errs := make(chan error, 2)
	go func() { errs <- run(playerX, playerY) }()
	go func() { errs <- run(playerY, playerX) }()
	for i := 0; i < 2; i++ {
		err := <-errs
		if err != nil {
			require.ErrorIs(t, err, application.ErrWalletAlreadyExists,
				"the retried unit finds the other one's wallets there: a business answer, never the deadlock")
		}
	}
	require.True(t, deadlocked.Load(), "the scenario must really have deadlocked, or this test proves nothing")
	require.Equal(t, 2, count(t.Context(), t, e.pool, "wallets"))
}
