package application_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
)

func policy(jitter float64) application.PendingPolicy {
	return application.PendingPolicy{
		MaxAttempts: 10, TTL: 15 * time.Minute, BackoffBase: 2 * time.Second, BackoffMax: 60 * time.Second,
		Jitter: func() float64 { return jitter },
	}
}

func TestTheGapDoublesFromTheBaseUpToTheCeiling(t *testing.T) {
	want := []time.Duration{2, 4, 8, 16, 32, 60, 60, 60}
	for i, w := range want {
		require.Equal(t, w*time.Second, policy(0.5).Gap(i+1), "after attempt %d", i+1)
	}
	require.Equal(t, 60*time.Second, policy(0.5).Gap(100000), "a huge count does not overflow")
	require.Equal(t, 2*time.Second, policy(0.5).Gap(0), "before the first attempt it is the base")
}

func TestTheGapIsSpreadByAFifthEachWay(t *testing.T) {
	require.Equal(t, 1600*time.Millisecond, policy(0).Gap(1), "the shortest")
	require.InDelta(t, float64(2400*time.Millisecond), float64(policy(1).Gap(1)), float64(time.Millisecond), "the longest")
	for _, j := range []float64{0, 0.25, 0.5, 0.75, 0.99} {
		gap := policy(j).Gap(3)
		require.GreaterOrEqual(t, gap, 6400*time.Millisecond)
		require.LessOrEqual(t, gap, 9600*time.Millisecond)
	}
	p := policy(0)
	p.Jitter = nil
	seen := map[time.Duration]bool{}
	for range 50 {
		seen[p.Gap(4)] = true
	}
	require.Greater(t, len(seen), 1, "without a source of its own it is random")
}

func TestTheNextAttemptNeverFallsAfterTheEndOfTheWait(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	p := policy(0.5)
	require.Equal(t, now.Add(2*time.Second), p.NextAttempt(now, 1, now.Add(time.Hour)))
	require.Equal(t, now.Add(time.Second), p.NextAttempt(now, 1, now.Add(time.Second)), "cut to the expiry")
	require.Equal(t, now, p.NextAttempt(now, 1, now), "already at the expiry")
}

func TestTheWaitEndsAtTheLastAttemptOrAtTheExpiryWhicheverComesFirst(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	p := policy(0.5)
	expires := p.ExpiresAt(now)
	require.Equal(t, now.Add(15*time.Minute), expires)

	require.False(t, p.Exhausted(1, now, expires))
	require.False(t, p.Exhausted(9, now.Add(14*time.Minute), expires))
	require.True(t, p.Exhausted(10, now, expires), "the tenth attempt is the last")
	require.True(t, p.Exhausted(11, now, expires))
	require.True(t, p.Exhausted(2, expires, expires), "the time is up")
	require.True(t, p.Exhausted(2, expires.Add(time.Second), expires))
	require.False(t, p.Exhausted(2, expires.Add(-time.Nanosecond), expires))
}
