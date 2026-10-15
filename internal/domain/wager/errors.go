package wager

import "errors"

// Errors returned by this package. Callers classify them with errors.Is.
var (
	ErrUninitialized           = errors.New("wager: uninitialized value")
	ErrInvalidID               = errors.New("wager: identifier must not be empty")
	ErrInvalidAmount           = errors.New("wager: amount must be a positive, initialized value")
	ErrInvalidTime             = errors.New("wager: invalid timestamp")
	ErrInvalidCorrelation      = errors.New("wager: correlation id must not be empty")
	ErrInvalidKind             = errors.New("wager: unsupported kind")
	ErrInvalidStatus           = errors.New("wager: status not valid for this kind")
	ErrInvalidTransition       = errors.New("wager: transition not allowed from the current status")
	ErrInvalidResult           = errors.New("wager: result does not match the status or the kind")
	ErrInvalidFailureCode      = errors.New("wager: not a failure code of the catalog")
	ErrInvalidProvider         = errors.New("wager: invalid provider id")
	ErrInvalidIdentifier       = errors.New("wager: an identifier must be 1 to 128 characters of valid UTF-8, with no control characters or padding")
	ErrReferenceRequired       = errors.New("wager: this kind of operation needs a reference")
	ErrReferenceNotAllowed     = errors.New("wager: this operation cannot carry that reference")
	ErrInvalidIdempotencyKey   = errors.New("wager: the idempotency key must be 1 to 255 printable ASCII characters")
	ErrPayloadHashMismatch     = errors.New("wager: the stored payload hash is not the hash of the stored request")
	ErrNoReference             = errors.New("wager: this transaction has no reference to wait for")
	ErrWalletMismatch          = errors.New("wager: the wallet is not the transaction's wallet")
	ErrKindNotSupported        = errors.New("wager: this kind of transaction cannot be settled yet")
	ErrExternalFieldsOnOpening = errors.New("wager: an internal opening has no provider, external id, key, hash, round, game or reference")
)
