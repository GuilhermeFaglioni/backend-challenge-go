package wallet

import "errors"

// Errors returned by this package. Callers classify them with errors.Is.
var (
	ErrUninitialized     = errors.New("wallet: uninitialized value")
	ErrInvalidID         = errors.New("wallet: identifier must not be empty")
	ErrInvalidTime       = errors.New("wallet: invalid timestamp")
	ErrInvalidAmount     = errors.New("wallet: amount must be a positive, initialized value")
	ErrInvalidBalance    = errors.New("wallet: balance must be an initialized, non-negative value")
	ErrInvalidVersion    = errors.New("wallet: invalid version")
	ErrInvalidDirection  = errors.New("wallet: direction must be DEBIT or CREDIT")
	ErrInconsistentEntry = errors.New("wallet: balance after does not follow from balance before, amount and direction")
	// ErrInsufficientFunds: a debit beyond the balance. A business outcome, not a defect.
	ErrInsufficientFunds = errors.New("wallet: insufficient funds")
)
