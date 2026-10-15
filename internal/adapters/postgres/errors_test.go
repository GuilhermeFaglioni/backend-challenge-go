package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
)

func TestOnlyInfrastructureTroubleIsMarkedTransient(t *testing.T) {
	pg := func(code string) error { return &pgconn.PgError{Code: code} }
	transient := map[string]error{
		"the caller's context ended":  context.Canceled,
		"the deadline passed":         fmt.Errorf("query: %w", context.DeadlineExceeded),
		"a serialization failure":     pg("40001"),
		"a deadlock":                  pg("40P01"),
		"a connection exception":      pg("08006"),
		"too many connections":        pg("53300"),
		"the server is shutting down": pg("57P01"),
		"a query cancelled":           pg("57014"),
		"a network error":             &net.OpError{Op: "dial", Err: errors.New("refused")},
		"a connect error":             &pgconn.ConnectError{},
		"a connection that dropped":   fmt.Errorf("failed to receive message: %w", io.ErrUnexpectedEOF),
		"the other side closed":       io.EOF,
		"a closed connection":         net.ErrClosed,
		"a concurrent update":         application.ErrConcurrentUpdate,
	}
	for name, err := range transient {
		marked := markTransient(fmt.Errorf("wrapped: %w", err))
		require.ErrorIs(t, marked, application.ErrTemporarilyUnavailable, name)
		require.ErrorIs(t, marked, err, "%s: the cause stays reachable", name)
	}

	permanent := map[string]error{
		"a unique violation":  pg("23505"),
		"a check violation":   pg("23514"),
		"a foreign key error": pg("23503"),
		"a privilege error":   pg("42501"),
		"a plain error":       errors.New("boom"),
		"a business conflict": &application.WalletAlreadyExistsError{},
	}
	for name, err := range permanent {
		require.NotErrorIs(t, markTransient(err), application.ErrTemporarilyUnavailable, name)
		require.Equal(t, err, markTransient(err), "%s is returned untouched", name)
	}
	require.NoError(t, markTransient(nil))
}

func TestMarkingTwiceDoesNotWrapTwice(t *testing.T) {
	once := markTransient(context.Canceled)
	require.Equal(t, once, markTransient(once))
}
