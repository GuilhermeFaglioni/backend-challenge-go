package event_test

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/event"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wallet"
)

var (
	eventID       = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	walletID      = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	transactionID = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	playerID      = uuid.MustParse("44444444-4444-4444-8444-444444444444")
	occurredAt    = time.Date(2026, 10, 14, 12, 0, 0, 123_000_000, time.UTC)
)

func brl(t *testing.T, minor int64) money.Money {
	t.Helper()
	m, err := money.FromMinor(minor, money.BRL)
	require.NoError(t, err)
	return m
}

func mustBRL(minor int64) money.Money {
	m, err := money.FromMinor(minor, money.BRL)
	if err != nil {
		panic(err)
	}
	return m
}

func meta() event.Meta {
	return event.Meta{EventID: eventID, CorrelationID: "corr-1", CausationID: "cause-1", OccurredAt: occurredAt}
}

func processedData(t *testing.T) event.WagerTransactionProcessedData {
	t.Helper()
	return event.WagerTransactionProcessedData{
		TransactionID: transactionID, WalletID: walletID, PlayerID: playerID,
		Kind: wager.KindOpening, Origin: wager.OriginInternal,
		Money: brl(t, 5000), Balance: brl(t, 5000), WalletVersion: 1,
	}
}

func changedData(t *testing.T) event.WalletBalanceChangedData {
	t.Helper()
	return event.WalletBalanceChangedData{
		WalletID: walletID, TransactionID: transactionID, Direction: wallet.DirectionCredit,
		Money: brl(t, 5000), BalanceBefore: brl(t, 0), BalanceAfter: brl(t, 5000), WalletVersion: 1,
	}
}

// --- the serialized form, exactly ---------------------------------------------------

func TestWagerTransactionProcessedSerializesDeterministically(t *testing.T) {
	e, err := event.NewWagerTransactionProcessed(meta(), processedData(t))
	require.NoError(t, err)

	want := `{"eventId":"11111111-1111-4111-8111-111111111111","eventType":"WagerTransactionProcessed",` +
		`"aggregateId":"22222222-2222-4222-8222-222222222222","correlationId":"corr-1","causationId":"cause-1",` +
		`"occurredAt":"2026-10-14T12:00:00.123Z","version":1,` +
		`"data":{"transactionId":"33333333-3333-4333-8333-333333333333","walletId":"22222222-2222-4222-8222-222222222222",` +
		`"playerId":"44444444-4444-4444-8444-444444444444","kind":"OPENING","origin":"INTERNAL",` +
		`"money":{"amount":"50.00","currency":"BRL"},"balance":{"amount":"50.00","currency":"BRL"},"walletVersion":1}}`

	first, err := json.Marshal(e)
	require.NoError(t, err)
	require.Equal(t, want, string(first))
	for i := 0; i < 20; i++ {
		again, err := json.Marshal(e)
		require.NoError(t, err)
		require.Equal(t, string(first), string(again), "the same event always serializes to the same bytes")
	}
}

func TestWalletBalanceChangedSerializesDeterministically(t *testing.T) {
	e, err := event.NewWalletBalanceChanged(meta(), changedData(t))
	require.NoError(t, err)

	want := `{"eventId":"11111111-1111-4111-8111-111111111111","eventType":"WalletBalanceChanged",` +
		`"aggregateId":"22222222-2222-4222-8222-222222222222","correlationId":"corr-1","causationId":"cause-1",` +
		`"occurredAt":"2026-10-14T12:00:00.123Z","version":1,` +
		`"data":{"walletId":"22222222-2222-4222-8222-222222222222","transactionId":"33333333-3333-4333-8333-333333333333",` +
		`"direction":"CREDIT","money":{"amount":"50.00","currency":"BRL"},` +
		`"balanceBefore":{"amount":"0.00","currency":"BRL"},"balanceAfter":{"amount":"50.00","currency":"BRL"},"walletVersion":1}}`

	got, err := json.Marshal(e)
	require.NoError(t, err)
	require.Equal(t, want, string(got))
}

func TestTheDataAloneIsTheSnapshotTheOutboxStores(t *testing.T) {
	e, err := event.NewWalletBalanceChanged(meta(), changedData(t))
	require.NoError(t, err)

	data, err := e.DataJSON()
	require.NoError(t, err)
	var whole struct {
		Data json.RawMessage `json:"data"`
	}
	full, err := json.Marshal(e)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(full, &whole))
	require.Equal(t, string(whole.Data), string(data))
}

func TestWithoutACausationIdTheFieldIsLeftOut(t *testing.T) {
	m := meta()
	m.CausationID = ""
	e, err := event.NewWalletBalanceChanged(m, changedData(t))
	require.NoError(t, err)
	got, err := json.Marshal(e)
	require.NoError(t, err)
	require.NotContains(t, string(got), "causationId")
	require.Empty(t, e.CausationID())
}

func TestMoneyTravelsAsDecimalStringsNeverNumbers(t *testing.T) {
	d := changedData(t)
	d.Money, d.BalanceBefore, d.BalanceAfter = brl(t, 1), brl(t, 100), brl(t, 101)
	e, err := event.NewWalletBalanceChanged(meta(), d)
	require.NoError(t, err)
	got, err := json.Marshal(e)
	require.NoError(t, err)
	require.Contains(t, string(got), `"money":{"amount":"0.01","currency":"BRL"}`)
	require.Contains(t, string(got), `"balanceBefore":{"amount":"1.00","currency":"BRL"}`)
	require.Contains(t, string(got), `"balanceAfter":{"amount":"1.01","currency":"BRL"}`)
}

// --- the envelope ------------------------------------------------------------------

func TestTheConstructorFixesTypeVersionAndAggregate(t *testing.T) {
	p, err := event.NewWagerTransactionProcessed(meta(), processedData(t))
	require.NoError(t, err)
	require.Equal(t, "WagerTransactionProcessed", p.Type())
	require.Equal(t, 1, p.Version())
	require.Equal(t, walletID, p.AggregateID(), "events are grouped by wallet")

	c, err := event.NewWalletBalanceChanged(meta(), changedData(t))
	require.NoError(t, err)
	require.Equal(t, "WalletBalanceChanged", c.Type())
	require.Equal(t, 1, c.Version())
	require.Equal(t, walletID, c.AggregateID())

	for _, e := range []event.Event{p, c} {
		require.Equal(t, eventID, e.EventID())
		require.Equal(t, "corr-1", e.CorrelationID())
		require.Equal(t, "cause-1", e.CausationID())
		require.Equal(t, occurredAt, e.OccurredAt())
		require.True(t, e.IsValid())
	}
}

func TestOccurredAtIsUTCAtMillisecondPrecision(t *testing.T) {
	m := meta()
	m.OccurredAt = time.Date(2026, 10, 14, 9, 0, 0, 123_456_789, time.FixedZone("BRT", -3*60*60))
	e, err := event.NewWalletBalanceChanged(m, changedData(t))
	require.NoError(t, err)

	require.Equal(t, time.UTC, e.OccurredAt().Location())
	require.Equal(t, time.Date(2026, 10, 14, 12, 0, 0, 123_000_000, time.UTC), e.OccurredAt(), "the snapshot is what is serialized")
	got, err := json.Marshal(e)
	require.NoError(t, err)
	require.Contains(t, string(got), `"occurredAt":"2026-10-14T12:00:00.123Z"`)
}

func TestTheEventIsASnapshotOfWhatItWasBuiltFrom(t *testing.T) {
	d := changedData(t)
	e, err := event.NewWalletBalanceChanged(meta(), d)
	require.NoError(t, err)
	before, err := json.Marshal(e)
	require.NoError(t, err)

	d.WalletVersion = 99 // the caller keeps changing its own copy
	d.Direction = wallet.DirectionDebit
	after, err := json.Marshal(e)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
}

func TestTheZeroEventIsInvalid(t *testing.T) {
	var e event.Event
	require.False(t, e.IsValid())
	_, err := json.Marshal(e)
	require.ErrorIs(t, err, event.ErrUninitialized)
	_, err = e.DataJSON()
	require.ErrorIs(t, err, event.ErrUninitialized)
	require.Empty(t, e.Type())
	require.Zero(t, e.Version())
	require.Equal(t, uuid.Nil, e.AggregateID())
}

func TestEveryEventNeedsItsEnvelopeFields(t *testing.T) {
	cases := map[string]struct {
		mutate func(*event.Meta)
		want   error
	}{
		"nil event id":   {func(m *event.Meta) { m.EventID = uuid.Nil }, event.ErrInvalidID},
		"no correlation": {func(m *event.Meta) { m.CorrelationID = "" }, event.ErrInvalidCorrelation},
		"zero time":      {func(m *event.Meta) { m.OccurredAt = time.Time{} }, event.ErrInvalidTime},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m := meta()
			c.mutate(&m)
			_, err := event.NewWalletBalanceChanged(m, changedData(t))
			require.ErrorIs(t, err, c.want)
			_, err = event.NewWagerTransactionProcessed(m, processedData(t))
			require.ErrorIs(t, err, c.want)
		})
	}
}

// --- WalletBalanceChanged data -----------------------------------------------------

func TestWalletBalanceChangedArithmeticFollowsTheDirection(t *testing.T) {
	d := changedData(t)
	d.Direction, d.BalanceBefore, d.BalanceAfter = wallet.DirectionDebit, brl(t, 8000), brl(t, 3000)
	_, err := event.NewWalletBalanceChanged(meta(), d)
	require.NoError(t, err)

	d.BalanceAfter = brl(t, 13000) // what a credit would have produced
	_, err = event.NewWalletBalanceChanged(meta(), d)
	require.ErrorIs(t, err, event.ErrInconsistentData)

	d = changedData(t)
	d.BalanceAfter = brl(t, 4999)
	_, err = event.NewWalletBalanceChanged(meta(), d)
	require.ErrorIs(t, err, event.ErrInconsistentData)
}

func TestWalletBalanceChangedRefusesBadData(t *testing.T) {
	usd, err := money.FromMinor(5000, money.USD)
	require.NoError(t, err)
	cases := map[string]struct {
		mutate func(*event.WalletBalanceChangedData)
		want   error
	}{
		"nil wallet":          {func(d *event.WalletBalanceChangedData) { d.WalletID = uuid.Nil }, event.ErrInvalidID},
		"nil transaction":     {func(d *event.WalletBalanceChangedData) { d.TransactionID = uuid.Nil }, event.ErrInvalidID},
		"unknown direction":   {func(d *event.WalletBalanceChangedData) { d.Direction = "SIDEWAYS" }, event.ErrInvalidDirection},
		"zero money":          {func(d *event.WalletBalanceChangedData) { d.Money = mustBRL(0) }, event.ErrInvalidMoney},
		"uninitialized money": {func(d *event.WalletBalanceChangedData) { d.Money = money.Money{} }, event.ErrInvalidMoney},
		"uninitialized before": {func(d *event.WalletBalanceChangedData) { d.BalanceBefore = money.Money{} },
			event.ErrInvalidMoney},
		"negative after":   {func(d *event.WalletBalanceChangedData) { d.BalanceAfter = mustBRL(-1) }, event.ErrInvalidMoney},
		"another currency": {func(d *event.WalletBalanceChangedData) { d.Money = usd }, money.ErrCurrencyMismatch},
		"version zero":     {func(d *event.WalletBalanceChangedData) { d.WalletVersion = 0 }, event.ErrInvalidVersion},
		"negative version": {func(d *event.WalletBalanceChangedData) { d.WalletVersion = -1 }, event.ErrInvalidVersion},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := changedData(t)
			c.mutate(&d)
			_, err := event.NewWalletBalanceChanged(meta(), d)
			require.ErrorIs(t, err, c.want)
		})
	}
}

func TestWalletBalanceChangedRefusesAnOverflowingBalance(t *testing.T) {
	d := changedData(t)
	d.BalanceBefore, d.Money, d.BalanceAfter = brl(t, math.MaxInt64-10), brl(t, 100), brl(t, math.MaxInt64)
	_, err := event.NewWalletBalanceChanged(meta(), d)
	require.ErrorIs(t, err, money.ErrOverflow)
}

// --- WagerTransactionProcessed data ------------------------------------------------

func TestWagerTransactionProcessedRefusesBadData(t *testing.T) {
	cases := map[string]struct {
		mutate func(*event.WagerTransactionProcessedData)
		want   error
	}{
		"nil transaction":     {func(d *event.WagerTransactionProcessedData) { d.TransactionID = uuid.Nil }, event.ErrInvalidID},
		"nil wallet":          {func(d *event.WagerTransactionProcessedData) { d.WalletID = uuid.Nil }, event.ErrInvalidID},
		"nil player":          {func(d *event.WagerTransactionProcessedData) { d.PlayerID = uuid.Nil }, event.ErrInvalidID},
		"no kind":             {func(d *event.WagerTransactionProcessedData) { d.Kind = "" }, event.ErrInvalidKind},
		"no origin":           {func(d *event.WagerTransactionProcessedData) { d.Origin = "" }, event.ErrInvalidKind},
		"uninitialized money": {func(d *event.WagerTransactionProcessedData) { d.Money = money.Money{} }, event.ErrInvalidMoney},
		"negative money":      {func(d *event.WagerTransactionProcessedData) { d.Money = mustBRL(-1) }, event.ErrInvalidMoney},
		"negative balance":    {func(d *event.WagerTransactionProcessedData) { d.Balance = mustBRL(-1) }, event.ErrInvalidMoney},
		"version zero":        {func(d *event.WagerTransactionProcessedData) { d.WalletVersion = 0 }, event.ErrInvalidVersion},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := processedData(t)
			c.mutate(&d)
			_, err := event.NewWagerTransactionProcessed(meta(), d)
			require.ErrorIs(t, err, c.want)
		})
	}
	t.Run("another currency", func(t *testing.T) {
		d := processedData(t)
		usd, err := money.FromMinor(5000, money.USD)
		require.NoError(t, err)
		d.Balance = usd
		_, err = event.NewWagerTransactionProcessed(meta(), d)
		require.ErrorIs(t, err, money.ErrCurrencyMismatch)
	})
}

// An internal event carries no external metadata, because the data type has none to
// carry (ARCHITECTURE.md, section 12).
func TestAnInternalEventCarriesNoExternalMetadata(t *testing.T) {
	e, err := event.NewWagerTransactionProcessed(meta(), processedData(t))
	require.NoError(t, err)
	got, err := json.Marshal(e)
	require.NoError(t, err)
	for _, field := range []string{"providerId", "externalTransactionId", "roundId", "gameId", "idempotencyKey"} {
		require.NotContains(t, string(got), field)
	}
}

// --- the stored form --------------------------------------------------------------

// What the publisher sends is built from the outbox row, not from the typed event.
// It must be byte for byte what the event itself serializes to.
func TestAnEnvelopeBuiltFromStoredPartsIsByteIdenticalToTheEvent(t *testing.T) {
	for name, build := range map[string]func() (event.Event, error){
		"balance changed": func() (event.Event, error) { return event.NewWalletBalanceChanged(meta(), changedData(t)) },
		"processed":       func() (event.Event, error) { return event.NewWagerTransactionProcessed(meta(), processedData(t)) },
		"no causation": func() (event.Event, error) {
			m := meta()
			m.CausationID = ""
			return event.NewWalletBalanceChanged(m, changedData(t))
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, err := build()
			require.NoError(t, err)
			data, err := e.DataJSON()
			require.NoError(t, err)

			envelope := event.Envelope{
				EventID: e.EventID(), EventType: e.Type(), AggregateID: e.AggregateID(),
				CorrelationID: e.CorrelationID(), CausationID: e.CausationID(),
				OccurredAt: e.OccurredAt(), Version: e.Version(), Data: data,
			}
			got, err := json.Marshal(envelope)
			require.NoError(t, err)
			want, err := json.Marshal(e)
			require.NoError(t, err)
			require.Equal(t, string(want), string(got))
		})
	}
}

func TestAnEnvelopeWritesItsTimeInUTCWithMilliseconds(t *testing.T) {
	e, err := event.NewWalletBalanceChanged(meta(), changedData(t))
	require.NoError(t, err)
	data, err := e.DataJSON()
	require.NoError(t, err)
	local := time.Date(2026, 10, 14, 9, 0, 0, 123_456_789, time.FixedZone("BRT", -3*60*60))
	got, err := json.Marshal(event.Envelope{
		EventID: eventID, EventType: "WalletBalanceChanged", AggregateID: walletID, CorrelationID: "c",
		OccurredAt: local, Version: 1, Data: data,
	})
	require.NoError(t, err)
	require.Contains(t, string(got), `"occurredAt":"2026-10-14T12:00:00.123Z"`)
}

func TestAnEnvelopeNeedsItsData(t *testing.T) {
	_, err := json.Marshal(event.Envelope{EventID: eventID, EventType: "X", AggregateID: walletID, CorrelationID: "c", OccurredAt: occurredAt, Version: 1})
	require.ErrorIs(t, err, event.ErrUninitialized)
	_, err = json.Marshal(event.Envelope{EventID: eventID, EventType: "X", AggregateID: walletID, CorrelationID: "c", OccurredAt: occurredAt, Version: 1, Data: []byte("not json")})
	require.Error(t, err, "data that is not JSON would corrupt the envelope")
}
