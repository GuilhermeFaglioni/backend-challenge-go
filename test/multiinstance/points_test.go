//go:build multiinstance

package multiinstance

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/platform/faultinject"
)

// crashesWhenItsWorkArrives starts an instance with a point that its work reaches
// (not its startup), gives it that work, and expects the crash. A second instance
// that has nothing to do with it must not notice.
func crashesWhenItsWorkArrives(t *testing.T, point faultinject.Point) {
	t.Helper()
	h := newEventsHarness(t)
	// The bystander publishes only once an hour, so it cannot take the event first.
	bystander := h.start(t, "bystander", map[string]string{"OUTBOX_POLL_INTERVAL": "1h"})
	crashing := h.start(t, "crashing", map[string]string{faultinject.EnvVar: string(point)})

	h.openWallet(t, crashing)

	ctx, cancel := context.WithTimeout(t.Context(), eventsWithin)
	defer cancel()
	require.NoError(t, crashing.WaitMessage(ctx, "fault point reached: "+string(point)))
	code, err := crashing.WaitExit(ctx)
	require.NoError(t, err)
	require.Equal(t, faultinject.ExitCode, code, "exit status of a fault point, not of a normal stop or a config error")
	require.True(t, bystander.Ready(t.Context()), "an unrelated instance must not notice")
}
