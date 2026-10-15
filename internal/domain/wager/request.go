package wager

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/domain/money"
)

const maxIdentifierRunes = 128

// providerIDPattern is the shape of a provider id; it is the same the authentication
// accepts in the `provider_id` claim.
var providerIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// PayloadHash is the SHA-256, in lower-case hex, of a Request's canonical JSON.
type PayloadHash string

// RequestParams are the business fields of an Operation request. Everything that
// belongs to the transport (the idempotency key, a message id, headers, timestamps)
// is deliberately not here: it cannot influence the hash, because it cannot reach it.
type RequestParams struct {
	ProviderID                     string
	ExternalTransactionID          string
	PlayerID                       uuid.UUID
	WalletID                       uuid.UUID
	RoundID                        string
	GameID                         string
	Kind                           Kind
	Amount                         money.Money
	ReferenceExternalTransactionID string // required for REFUND and ROLLBACK, optional for WIN, absent otherwise
}

// Request is a validated Operation request: what a provider asked, whichever way it
// arrived (HTTP or SQS). It is immutable. The zero value is invalid.
type Request struct {
	p RequestParams
}

// NewRequest validates the fields of an Operation request.
func NewRequest(p RequestParams) (Request, error) {
	if !providerIDPattern.MatchString(p.ProviderID) {
		return Request{}, fmt.Errorf("%w: lower-case letters, digits, '.', '_' and '-', up to 63 characters", ErrInvalidProvider)
	}
	for name, value := range map[string]string{"externalTransactionId": p.ExternalTransactionID, "roundId": p.RoundID, "gameId": p.GameID} {
		if !validIdentifier(value) {
			return Request{}, fmt.Errorf("%w: %s", ErrInvalidIdentifier, name)
		}
	}
	if p.PlayerID == uuid.Nil || p.WalletID == uuid.Nil {
		return Request{}, ErrInvalidID
	}
	if !p.Kind.IsExternal() {
		return Request{}, fmt.Errorf("%w: %q", ErrInvalidKind, p.Kind)
	}
	if err := checkAmountForKind(p.Kind, p.Amount); err != nil {
		return Request{}, err
	}
	if err := checkReference(p); err != nil {
		return Request{}, err
	}
	return Request{p: p}, nil
}

func checkAmountForKind(kind Kind, amount money.Money) error {
	if kind == KindLoss {
		// A LOSS moves nothing, and says so: exactly 0.00.
		if !amount.IsZero() {
			return fmt.Errorf("%w: a LOSS must be 0.00", ErrInvalidAmount)
		}
		return nil
	}
	if !amount.IsPositive() {
		return fmt.Errorf("%w: a %s must be greater than zero", ErrInvalidAmount, kind)
	}
	return nil
}

func checkReference(p RequestParams) error {
	reference := p.ReferenceExternalTransactionID
	switch p.Kind {
	case KindRefund, KindRollback:
		if reference == "" {
			return fmt.Errorf("%w: a %s", ErrReferenceRequired, p.Kind)
		}
	case KindWin:
		// Optional.
	default:
		if reference != "" {
			return fmt.Errorf("%w: a %s", ErrReferenceNotAllowed, p.Kind)
		}
	}
	if reference == "" {
		return nil
	}
	if !validIdentifier(reference) {
		return fmt.Errorf("%w: referenceExternalTransactionId", ErrInvalidIdentifier)
	}
	if reference == p.ExternalTransactionID {
		return fmt.Errorf("%w: an operation cannot reference itself", ErrReferenceNotAllowed)
	}
	return nil
}

// validIdentifier accepts an identifier a provider chose: valid UTF-8, one to 128
// characters, no control characters and no padding. It is compared and hashed byte for
// byte; nothing is normalized.
func validIdentifier(s string) bool {
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxIdentifierRunes || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (r Request) IsValid() bool                          { return r.p.ProviderID != "" }
func (r Request) ProviderID() string                     { return r.p.ProviderID }
func (r Request) ExternalTransactionID() string          { return r.p.ExternalTransactionID }
func (r Request) PlayerID() uuid.UUID                    { return r.p.PlayerID }
func (r Request) WalletID() uuid.UUID                    { return r.p.WalletID }
func (r Request) RoundID() string                        { return r.p.RoundID }
func (r Request) GameID() string                         { return r.p.GameID }
func (r Request) Kind() Kind                             { return r.p.Kind }
func (r Request) Amount() money.Money                    { return r.p.Amount }
func (r Request) ReferenceExternalTransactionID() string { return r.p.ReferenceExternalTransactionID }

// CanonicalJSON is the form the hash is taken over: the business fields as a JSON
// object with the keys in lexicographic order at every level, no whitespace, no
// absent or null fields, strings in UTF-8 written as they are (no HTML escaping), and
// the amount as the strict string it already is, never normalized. The same Request
// gives the same bytes on every path (HTTP, SQS), since this is the only place they
// are produced.
func (r Request) CanonicalJSON() []byte {
	fields := map[string]any{
		"providerId":            r.p.ProviderID,
		"externalTransactionId": r.p.ExternalTransactionID,
		"playerId":              r.p.PlayerID.String(),
		"walletId":              r.p.WalletID.String(),
		"roundId":               r.p.RoundID,
		"gameId":                r.p.GameID,
		"kind":                  string(r.p.Kind),
		"money":                 map[string]string{"amount": r.p.Amount.String(), "currency": r.p.Amount.Currency().String()},
	}
	if r.p.ReferenceExternalTransactionID != "" {
		fields["referenceExternalTransactionId"] = r.p.ReferenceExternalTransactionID
	}

	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	// encoding/json writes the keys of a map in sorted order.
	if err := encoder.Encode(fields); err != nil {
		panic(fmt.Sprintf("wager: canonical JSON of validated fields cannot fail: %v", err))
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}

// Hash is the SHA-256 of the canonical JSON, in lower-case hex.
func (r Request) Hash() PayloadHash {
	sum := sha256.Sum256(r.CanonicalJSON())
	return PayloadHash(hex.EncodeToString(sum[:]))
}
