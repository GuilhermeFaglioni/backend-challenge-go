package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
)

// markTransient wraps errors the infrastructure caused, and that trying again later
// may fix, with application.ErrTemporarilyUnavailable. The original error stays
// reachable through errors.Is and errors.As. Anything else is returned untouched:
// a constraint violation, a business error or a bug is not "temporary".
func markTransient(err error) error {
	if err == nil || errors.Is(err, application.ErrTemporarilyUnavailable) || !isTransient(err) {
		return err
	}
	return fmt.Errorf("%w: %w", application.ErrTemporarilyUnavailable, err)
}

func isTransient(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == sqlStateSerializationFailure, pgErr.Code == sqlStateDeadlockDetected:
			return true // only reaches the caller after the retries were used up
		case strings.HasPrefix(pgErr.Code, "08"): // connection exception
			return true
		case strings.HasPrefix(pgErr.Code, "53"): // insufficient resources (too many connections...)
			return true
		case strings.HasPrefix(pgErr.Code, "57"): // operator intervention (shutdown, cancelled)
			return true
		}
	}
	return false
}
