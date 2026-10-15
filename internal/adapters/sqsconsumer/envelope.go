package sqsconsumer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application/processwager"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/wager"
)

// RequestedType is the only message type the input queue carries.
const RequestedType = "WagerTransactionRequested"

const maxMessageIDLength = 128

// Reason says why a message can never be handled. It is a label of the dead-letter
// metric, so the set is closed.
type Reason string

const (
	ReasonMalformed       Reason = "malformed"             // not the JSON object of the envelope
	ReasonUnknownType     Reason = "unknown_type"          // a type this consumer does not handle
	ReasonInvalid         Reason = "invalid"               // a field the use case or the envelope refuses
	ReasonUnsupportedKind Reason = "unsupported_kind"      // a valid kind not processed yet
	ReasonUnsupportedRef  Reason = "unsupported_reference" // a reference, which is not resolved yet
	ReasonKeyConflict     Reason = "key_conflict"          // the idempotency key was used with other content
	ReasonDuplicateExtID  Reason = "duplicate_external"    // the external id was used under another key
	ReasonMessageConflict Reason = "message_conflict"      // the message id was used with other content
)

// Reasons lists every Reason, so a metric can start each at zero.
var Reasons = []Reason{
	ReasonMalformed, ReasonUnknownType, ReasonInvalid, ReasonUnsupportedKind, ReasonUnsupportedRef, ReasonKeyConflict, ReasonDuplicateExtID, ReasonMessageConflict,
}

// DecodeError is a message that cannot become a request. Detail says which field or
// what is wrong with the JSON, never what the value was.
type DecodeError struct {
	Reason Reason
	Detail string
}

func (e *DecodeError) Error() string { return string(e.Reason) + ": " + e.Detail }

func malformed(format string, args ...any) *DecodeError {
	return &DecodeError{Reason: ReasonMalformed, Detail: fmt.Sprintf(format, args...)}
}

func invalid(field, issue string) *DecodeError {
	return &DecodeError{Reason: ReasonInvalid, Detail: field + " " + issue}
}

type envelope struct {
	MessageID  json.RawMessage `json:"messageId"`
	Type       json.RawMessage `json:"type"`
	OccurredAt json.RawMessage `json:"occurredAt"`
	Data       json.RawMessage `json:"data"`
}

type requestData struct {
	ProviderID                     json.RawMessage `json:"providerId"`
	ExternalTransactionID          json.RawMessage `json:"externalTransactionId"`
	IdempotencyKey                 json.RawMessage `json:"idempotencyKey"`
	PlayerID                       json.RawMessage `json:"playerId"`
	WalletID                       json.RawMessage `json:"walletId"`
	RoundID                        json.RawMessage `json:"roundId"`
	GameID                         json.RawMessage `json:"gameId"`
	Kind                           json.RawMessage `json:"kind"`
	Money                          json.RawMessage `json:"money"`
	ReferenceExternalTransactionID json.RawMessage `json:"referenceExternalTransactionId"`
}

// Decoded is a message body read into what the use case takes.
type Decoded struct {
	MessageID      string
	OccurredAt     time.Time
	Params         wager.RequestParams
	IdempotencyKey string
}

// Decode reads a message body. It is as strict as the HTTP endpoint: unknown fields,
// wrong types, a second document after the first and values that are not in their one
// canonical spelling are refused, never corrected. The business rules (which kinds
// are accepted, the amounts, the lengths) are the use case's, applied to the result.
func Decode(body []byte) (Decoded, *DecodeError) {
	var env envelope
	if err := strictUnmarshal(body, &env); err != nil {
		return Decoded{}, malformed("the body is not the JSON object of an envelope (%s)", jsonProblem(err))
	}

	var out Decoded
	var problem *DecodeError
	if out.MessageID, problem = messageID(env.MessageID); problem != nil {
		return Decoded{}, problem
	}
	// The type is checked before anything else about the content: a message of another
	// kind is not a bad WagerTransactionRequested, it is not one at all.
	typ, problem := text(env.Type, "type")
	if problem != nil {
		return Decoded{}, problem
	}
	if typ != RequestedType {
		return Decoded{}, &DecodeError{Reason: ReasonUnknownType, Detail: "the type is not " + RequestedType}
	}
	occurred, problem := text(env.OccurredAt, "occurredAt")
	if problem != nil {
		return Decoded{}, problem
	}
	var err error
	if out.OccurredAt, err = time.Parse(time.RFC3339Nano, occurred); err != nil {
		return Decoded{}, invalid("occurredAt", "must be an RFC 3339 timestamp")
	}
	if isNullOrAbsent(env.Data) {
		return Decoded{}, invalid("data", "is required")
	}

	var data requestData
	if err := strictUnmarshal(env.Data, &data); err != nil {
		return Decoded{}, invalid("data", "must be an object with the fields of a wager request ("+jsonProblem(err)+")")
	}
	p := &out.Params
	for _, f := range []struct {
		raw  json.RawMessage
		name string
		into *string
	}{
		{data.ProviderID, "data.providerId", &p.ProviderID},
		{data.ExternalTransactionID, "data.externalTransactionId", &p.ExternalTransactionID},
		{data.IdempotencyKey, "data.idempotencyKey", &out.IdempotencyKey},
		{data.RoundID, "data.roundId", &p.RoundID},
		{data.GameID, "data.gameId", &p.GameID},
	} {
		if *f.into, problem = text(f.raw, f.name); problem != nil {
			return Decoded{}, problem
		}
	}
	if p.PlayerID, problem = canonicalUUID(data.PlayerID, "data.playerId"); problem != nil {
		return Decoded{}, problem
	}
	if p.WalletID, problem = canonicalUUID(data.WalletID, "data.walletId"); problem != nil {
		return Decoded{}, problem
	}
	kind, problem := text(data.Kind, "data.kind")
	if problem != nil {
		return Decoded{}, problem
	}
	p.Kind = wager.Kind(kind)
	if isNullOrAbsent(data.Money) {
		return Decoded{}, invalid("data.money", "is required")
	}
	if err := json.Unmarshal(data.Money, &p.Amount); err != nil {
		return Decoded{}, invalid("data.money", "must be {\"amount\": \"25.00\", \"currency\": \"BRL\"}")
	}
	if !isNullOrAbsent(data.ReferenceExternalTransactionID) {
		if p.ReferenceExternalTransactionID, problem = text(data.ReferenceExternalTransactionID, "data.referenceExternalTransactionId"); problem != nil {
			return Decoded{}, problem
		}
	}
	return out, nil
}

// Input is what the use case takes for this message.
func (d Decoded) Input(consumer, correlationID string) processwager.Input {
	return processwager.Input{
		Request:        d.Params,
		IdempotencyKey: d.IdempotencyKey,
		CorrelationID:  correlationID,
		CausationID:    d.MessageID,
		Message:        &processwager.Message{Consumer: consumer, ID: d.MessageID},
	}
}

func strictUnmarshal(data []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the JSON object")
	}
	return nil
}

// jsonProblem describes what is wrong with a document without quoting it.
func jsonProblem(err error) string {
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		return "syntax error"
	case errors.As(err, &typ):
		return "a field has the wrong type: " + typ.Field
	case strings.HasPrefix(err.Error(), "json: unknown field"):
		return "an unknown field"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "empty or cut short"
	}
	return err.Error()
}

func isNullOrAbsent(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

// text reads a required JSON string.
func text(raw json.RawMessage, field string) (string, *DecodeError) {
	if isNullOrAbsent(raw) {
		return "", invalid(field, "is required")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", invalid(field, "must be a string")
	}
	return s, nil
}

func messageID(raw json.RawMessage) (string, *DecodeError) {
	id, problem := text(raw, "messageId")
	if problem != nil {
		return "", problem
	}
	if id == "" || utf8.RuneCountInString(id) > maxMessageIDLength || !utf8.ValidString(id) {
		return "", invalid("messageId", "must be 1 to 128 characters")
	}
	if strings.IndexFunc(id, unicode.IsControl) >= 0 || strings.TrimSpace(id) != id {
		return "", invalid("messageId", "must not have control characters or spaces at its ends")
	}
	return id, nil
}

// canonicalUUID accepts only the 36-character form with dashes and not the nil UUID,
// like the HTTP endpoint: the library also takes braces, a urn: prefix and no dashes,
// and one identifier should have one spelling.
func canonicalUUID(raw json.RawMessage, field string) (uuid.UUID, *DecodeError) {
	s, problem := text(raw, field)
	if problem != nil {
		return uuid.Nil, problem
	}
	id, err := uuid.Parse(s)
	if len(s) != 36 || err != nil || id == uuid.Nil {
		return uuid.Nil, invalid(field, "must be a UUID in the form xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx, and not the nil UUID")
	}
	return id, nil
}
