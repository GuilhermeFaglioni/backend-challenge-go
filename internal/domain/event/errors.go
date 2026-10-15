package event

import "errors"

// Errors returned by this package. Callers classify them with errors.Is.
var (
	ErrUninitialized      = errors.New("event: uninitialized value")
	ErrInvalidID          = errors.New("event: identifier must not be empty")
	ErrInvalidCorrelation = errors.New("event: correlation id must not be empty")
	ErrInvalidTime        = errors.New("event: invalid timestamp")
	ErrInvalidKind        = errors.New("event: kind and origin must not be empty")
	ErrInvalidDirection   = errors.New("event: direction must be DEBIT or CREDIT")
	ErrInvalidMoney       = errors.New("event: amount missing, negative or not positive where it must be")
	ErrInvalidFailureCode = errors.New("event: failure code must be a rejection code of the catalog")
	ErrInvalidVersion     = errors.New("event: wallet version must be at least 1")
	ErrInconsistentData   = errors.New("event: balance after does not follow from balance before, amount and direction")
)
