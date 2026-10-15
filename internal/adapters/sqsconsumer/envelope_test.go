package sqsconsumer_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/sqsconsumer"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
)

const (
	playerID = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"
	walletID = "0192f291-27dd-7d3f-8071-5f8685deef37"
)

// body builds the example of the challenge, with the given fields replaced. A nil value
// removes the field; the replacements are JSON text.
func body(replace map[string]string) string {
	fields := map[string]string{
		"messageId":  `"msg-123"`,
		"type":       `"WagerTransactionRequested"`,
		"occurredAt": `"2026-09-08T12:00:00.000Z"`,
		"data": `{"providerId":"provider-a","externalTransactionId":"transaction-123","idempotencyKey":"provider-a:transaction-123",
			"playerId":"` + playerID + `","walletId":"` + walletID + `","roundId":"round-987","gameId":"fortune-chimp","kind":"BET",
			"money":{"amount":"25.00","currency":"BRL"}}`,
	}
	for k, v := range replace {
		fields[k] = v
	}
	var parts []string
	for _, k := range []string{"messageId", "type", "occurredAt", "data"} {
		if fields[k] != "" {
			parts = append(parts, fmt.Sprintf("%q:%s", k, fields[k]))
		}
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// data builds the data object with one field replaced (empty removes it).
func data(field, value string) string {
	fields := map[string]string{
		"providerId": `"provider-a"`, "externalTransactionId": `"transaction-123"`, "idempotencyKey": `"provider-a:transaction-123"`,
		"playerId": `"` + playerID + `"`, "walletId": `"` + walletID + `"`, "roundId": `"round-987"`, "gameId": `"fortune-chimp"`,
		"kind": `"BET"`, "money": `{"amount":"25.00","currency":"BRL"}`,
	}
	if value == "" {
		delete(fields, field)
	} else {
		fields[field] = value
	}
	var parts []string
	for k, v := range fields {
		parts = append(parts, fmt.Sprintf("%q:%s", k, v))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// withExtra adds fields to the end of an object.
func withExtra(object, extra string) string { return object[:len(object)-1] + "," + extra + "}" }

func TestTheExampleOfTheChallengeIsRead(t *testing.T) {
	got, problem := sqsconsumer.Decode([]byte(body(nil)))
	require.Nil(t, problem)

	require.Equal(t, "msg-123", got.MessageID)
	require.True(t, got.OccurredAt.Equal(time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)))
	require.Equal(t, "provider-a:transaction-123", got.IdempotencyKey)
	p := got.Params
	require.Equal(t, "provider-a", p.ProviderID)
	require.Equal(t, "transaction-123", p.ExternalTransactionID)
	require.Equal(t, uuid.MustParse(playerID), p.PlayerID)
	require.Equal(t, uuid.MustParse(walletID), p.WalletID)
	require.Equal(t, "round-987", p.RoundID)
	require.Equal(t, "fortune-chimp", p.GameID)
	require.Equal(t, wager.KindBet, p.Kind)
	require.Equal(t, int64(2500), p.Amount.Minor())
	require.Equal(t, "BRL", string(p.Amount.Currency()))
	require.Empty(t, p.ReferenceExternalTransactionID)

	in := got.Input("wager-transactions.fifo", "corr-1")
	require.Equal(t, "corr-1", in.CorrelationID)
	require.Equal(t, "msg-123", in.CausationID, "the message is what caused the events")
	require.Equal(t, "wager-transactions.fifo", in.Message.Consumer)
	require.Equal(t, "msg-123", in.Message.ID)
}

func TestAReferenceIsRead(t *testing.T) {
	d := withExtra(data("kind", `"WIN"`), `"referenceExternalTransactionId":"transaction-1"`)
	got, problem := sqsconsumer.Decode([]byte(body(map[string]string{"data": d})))
	require.Nil(t, problem)
	require.Equal(t, "transaction-1", got.Params.ReferenceExternalTransactionID)
	require.Equal(t, wager.KindWin, got.Params.Kind)

	d = withExtra(data("kind", `"WIN"`), `"referenceExternalTransactionId":null`)
	got, problem = sqsconsumer.Decode([]byte(body(map[string]string{"data": d})))
	require.Nil(t, problem)
	require.Empty(t, got.Params.ReferenceExternalTransactionID, "a null reference is no reference")
}

func TestWhatIsNotAnEnvelopeIsMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":                   ``,
		"not json":                `hello`,
		"cut short":               `{"messageId":"m"`,
		"an array":                `[]`,
		"a string":                `"text"`,
		"a second document":       body(nil) + `{}`,
		"trailing garbage":        body(nil) + ` x`,
		"an unknown field":        strings.Replace(body(nil), `{"messageId"`, `{"extra":1,"messageId"`, 1),
		"an unknown data field":   body(map[string]string{"data": withExtra(data("kind", `"BET"`), `"surprise":true`)}),
		"messageId of wrong type": body(map[string]string{"messageId": `7`}),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, problem := sqsconsumer.Decode([]byte(input))
			require.NotNil(t, problem)
			require.Contains(t, []sqsconsumer.Reason{sqsconsumer.ReasonMalformed, sqsconsumer.ReasonInvalid}, problem.Reason, problem.Detail)
		})
	}
}

func TestAMessageOfAnotherTypeIsAnUnknownType(t *testing.T) {
	for _, typ := range []string{`"WagerTransactionCancelled"`, `""`, `"wagertransactionrequested"`} {
		_, problem := sqsconsumer.Decode([]byte(body(map[string]string{"type": typ})))
		require.NotNil(t, problem, typ)
		require.Equal(t, sqsconsumer.ReasonUnknownType, problem.Reason, typ)
	}
	_, problem := sqsconsumer.Decode([]byte(body(map[string]string{"type": ""})))
	require.Equal(t, sqsconsumer.ReasonInvalid, problem.Reason, "a missing type is a missing field")
}

func TestEveryFieldIsRequiredAndCheckedForItsForm(t *testing.T) {
	cases := map[string]string{
		"no messageId":            body(map[string]string{"messageId": `null`}),
		"empty messageId":         body(map[string]string{"messageId": `""`}),
		"long messageId":          body(map[string]string{"messageId": `"` + strings.Repeat("m", 129) + `"`}),
		"messageId with control":  body(map[string]string{"messageId": `"msg\u0000"`}),
		"messageId with spaces":   body(map[string]string{"messageId": `" msg"`}),
		"no occurredAt":           body(map[string]string{"occurredAt": `null`}),
		"occurredAt not a time":   body(map[string]string{"occurredAt": `"yesterday"`}),
		"occurredAt a number":     body(map[string]string{"occurredAt": `1757332800`}),
		"no data":                 body(map[string]string{"data": `null`}),
		"data is a list":          body(map[string]string{"data": `[]`}),
		"no providerId":           body(map[string]string{"data": data("providerId", "")}),
		"providerId a number":     body(map[string]string{"data": data("providerId", "7")}),
		"no externalTransaction":  body(map[string]string{"data": data("externalTransactionId", "")}),
		"no idempotencyKey":       body(map[string]string{"data": data("idempotencyKey", "")}),
		"null idempotencyKey":     body(map[string]string{"data": data("idempotencyKey", "null")}),
		"no playerId":             body(map[string]string{"data": data("playerId", "")}),
		"playerId not canonical":  body(map[string]string{"data": data("playerId", `"0192F28F5DC07D58BDB2814AD6A0F4A1"`)}),
		"playerId with braces":    body(map[string]string{"data": data("playerId", `"{`+playerID+`}"`)}),
		"playerId nil":            body(map[string]string{"data": data("playerId", `"00000000-0000-0000-0000-000000000000"`)}),
		"walletId not a uuid":     body(map[string]string{"data": data("walletId", `"wallet-1"`)}),
		"no roundId":              body(map[string]string{"data": data("roundId", "")}),
		"no gameId":               body(map[string]string{"data": data("gameId", "")}),
		"no kind":                 body(map[string]string{"data": data("kind", "")}),
		"no money":                body(map[string]string{"data": data("money", "")}),
		"money a number":          body(map[string]string{"data": data("money", `25.00`)}),
		"amount a number":         body(map[string]string{"data": data("money", `{"amount":25.00,"currency":"BRL"}`)}),
		"amount without decimals": body(map[string]string{"data": data("money", `{"amount":"25","currency":"BRL"}`)}),
		"money without currency":  body(map[string]string{"data": data("money", `{"amount":"25.00"}`)}),
		"unknown currency":        body(map[string]string{"data": data("money", `{"amount":"25.00","currency":"XXX"}`)}),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, problem := sqsconsumer.Decode([]byte(input))
			require.NotNil(t, problem)
			require.Equal(t, sqsconsumer.ReasonInvalid, problem.Reason, problem.Detail)
		})
	}
}

// The detail travels to the dead-letter queue and the logs: it says which field is
// wrong, never what was in it.
func TestTheDetailNeverQuotesTheMessage(t *testing.T) {
	secrets := []string{"SECRET-AMOUNT", "SECRET-PLAYER", "SECRET-TYPE", "SECRET-TIME", "SECRET-ID"}
	cases := []string{
		body(map[string]string{"data": data("money", `{"amount":"SECRET-AMOUNT","currency":"BRL"}`)}),
		body(map[string]string{"data": data("playerId", `"SECRET-PLAYER"`)}),
		body(map[string]string{"type": `"SECRET-TYPE"`}),
		body(map[string]string{"occurredAt": `"SECRET-TIME"`}),
		body(map[string]string{"messageId": `"SECRET-ID\u0001"`}),
		strings.Replace(body(nil), `{"messageId"`, `{"SECRET-AMOUNT":1,"messageId"`, 1),
	}
	for _, input := range cases {
		_, problem := sqsconsumer.Decode([]byte(input))
		require.NotNil(t, problem)
		for _, secret := range secrets {
			require.NotContains(t, problem.Error(), secret)
		}
	}
}

// The business rules belong to the use case, which the consumer asks; the decoder lets
// a well-formed OPENING through to it, where it is refused for HTTP and SQS alike.
func TestTheDecoderDoesNotDecideWhichKindsAreAccepted(t *testing.T) {
	got, problem := sqsconsumer.Decode([]byte(body(map[string]string{"data": data("kind", `"OPENING"`)})))
	require.Nil(t, problem)
	require.Equal(t, wager.KindOpening, got.Params.Kind)
	_, err := wager.NewRequest(got.Params)
	require.ErrorIs(t, err, wager.ErrInvalidKind)
}
