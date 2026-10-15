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
	ErrExternalFieldsOnOpening = errors.New("wager: an internal opening has no provider, external id, key, hash, round, game or reference")
)
