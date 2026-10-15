package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
)

const (
	sqlStateSerializationFailure = "40001"
	sqlStateDeadlockDetected     = "40P01"

	// DefaultMaxAttempts is how many times a unit of work runs before a
	// serialization failure or deadlock is given up on.
	DefaultMaxAttempts = 3
	// rollbackTimeout bounds the rollback after the caller's context is gone.
	rollbackTimeout = 5 * time.Second
)

// UnitOfWork runs a function inside a READ COMMITTED database transaction on the
// pool (ARCHITECTURE.md, section 6). It implements application.UnitOfWork.
type UnitOfWork struct {
	pool        *pgxpool.Pool
	maxAttempts int
	backoff     func(attempt int) time.Duration
	observer    application.RetryObserver
}

// NewUnitOfWork builds a unit of work with the default bounds.
func NewUnitOfWork(pool *pgxpool.Pool) *UnitOfWork {
	return &UnitOfWork{
		pool:        pool,
		maxAttempts: DefaultMaxAttempts,
		// Short and growing, so colliding transactions do not retry in lockstep.
		backoff: func(attempt int) time.Duration { return time.Duration(attempt) * 20 * time.Millisecond },
	}
}

// WithRetry returns a copy that runs at most maxAttempts times, waiting
// backoff(attempt) between attempts. For tests.
func (u *UnitOfWork) WithRetry(maxAttempts int, backoff func(attempt int) time.Duration) *UnitOfWork {
	return &UnitOfWork{pool: u.pool, maxAttempts: maxAttempts, backoff: backoff, observer: u.observer}
}

// WithObserver returns a copy that tells the observer about every retry.
func (u *UnitOfWork) WithObserver(o application.RetryObserver) *UnitOfWork {
	return &UnitOfWork{pool: u.pool, maxAttempts: u.maxAttempts, backoff: u.backoff, observer: o}
}

var _ application.UnitOfWork = (*UnitOfWork)(nil)

// Do runs fn in a database transaction and commits it. Any error from fn and any
// panic roll the transaction back (a panic then continues). A serialization failure
// or deadlock, from fn or from the commit, runs fn again in a new transaction, up to
// the attempt bound. The context is honoured between attempts and by every query,
// beginning with BeginTx: a caller that is already gone starts no work.
func (u *UnitOfWork) Do(ctx context.Context, fn func(ctx context.Context, repos application.Repositories) error) error {
	for attempt := 1; ; attempt++ {
		err := u.once(ctx, fn)
		if err == nil || !isRetryable(err) || attempt >= u.maxAttempts {
			return markTransient(err)
		}
		if u.observer != nil {
			u.observer.Retried(retryReason(err))
		}
		select {
		case <-ctx.Done():
			return markTransient(fmt.Errorf("%w (giving up retrying: %w)", err, ctx.Err()))
		case <-time.After(u.backoff(attempt)):
		}
	}
}

func (u *UnitOfWork) once(ctx context.Context, fn func(context.Context, application.Repositories) error) (err error) {
	tx, err := u.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		// Covers an error return and a panic alike (after a successful commit it does
		// nothing). The caller's context may be the reason we are here, so the
		// rollback does not depend on it.
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	if err := fn(ctx, newRepositories(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// retryReason says which kind of conflict made a unit of work retry.
func retryReason(err error) application.RetryReason {
	if errors.Is(err, application.ErrConcurrentUpdate) {
		return application.RetryConcurrentUpdate
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlStateDeadlockDetected {
		return application.RetryDeadlock
	}
	return application.RetrySerializationFailure
}

// isRetryable reports whether the error is one PostgreSQL says to retry the whole
// transaction for.
func isRetryable(err error) bool {
	if errors.Is(err, application.ErrConcurrentUpdate) {
		return true
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == sqlStateSerializationFailure || pgErr.Code == sqlStateDeadlockDetected
}
