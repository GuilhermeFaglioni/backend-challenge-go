package wager

import "slices"

// FailureCode explains a Rejection or a Failure. The strings are part of the API
// contract: clients branch on them, so they never change.
type FailureCode string

const (
	// FailureInsufficientFunds: a BET beyond the balance.
	FailureInsufficientFunds FailureCode = "INSUFFICIENT_FUNDS"
	// FailureBalanceLimitExceeded: a credit that would take the balance beyond what a
	// wallet can hold.
	FailureBalanceLimitExceeded FailureCode = "BALANCE_LIMIT_EXCEEDED"
	// FailureReversalInsufficientFunds: a reversal that would take the balance below
	// zero, told apart from the BET's code as the challenge requires.
	FailureReversalInsufficientFunds FailureCode = "REVERSAL_INSUFFICIENT_FUNDS"
	// FailureWalletNotFound: the wallet does not exist.
	FailureWalletNotFound FailureCode = "WALLET_NOT_FOUND"
	// FailureCurrencyMismatch: the request's currency is not the wallet's.
	FailureCurrencyMismatch FailureCode = "CURRENCY_MISMATCH"
	// FailurePlayerMismatch: the player does not own the wallet.
	FailurePlayerMismatch FailureCode = "PLAYER_MISMATCH"
	// FailureReferenceNotFound: the Reference never appeared in time.
	FailureReferenceNotFound FailureCode = "REFERENCE_NOT_FOUND"
	// FailureReferenceNotProcessed: the Reference ended without success, or stayed
	// pending until the limit.
	FailureReferenceNotProcessed FailureCode = "REFERENCE_NOT_PROCESSED"
	// FailureReferenceKindInvalid: the Reference is of a kind this operation cannot act on.
	FailureReferenceKindInvalid FailureCode = "REFERENCE_KIND_INVALID"
	// FailureReferenceMismatch: provider, player, wallet, currency or round differ.
	FailureReferenceMismatch FailureCode = "REFERENCE_MISMATCH"
	// FailureReferenceAmountMismatch: the amount is not the Reference's.
	FailureReferenceAmountMismatch FailureCode = "REFERENCE_AMOUNT_MISMATCH"
	// FailureReferenceAlreadyReversed: the Reference already has a Reversal.
	FailureReferenceAlreadyReversed FailureCode = "REFERENCE_ALREADY_REVERSED"
	// FailureInternalPermanent is the code of a Failure: a permanent defect of the
	// infrastructure, recorded for audit. It is not a Rejection.
	FailureInternalPermanent FailureCode = "INTERNAL_PERMANENT_FAILURE"
)

// FailureClass tells a provider what to do about a failure code.
type FailureClass string

const (
	// ClassCorrectable: the data the provider sent is at fault; a new operation with
	// corrected data (and a new key and external id) can succeed.
	ClassCorrectable FailureClass = "CORRECTABLE"
	// ClassDefinitive: a business result that sending again does not change.
	ClassDefinitive FailureClass = "DEFINITIVE"
)

// catalog lists every code with its class, in the order of ARCHITECTURE.md.
var catalog = []struct {
	code  FailureCode
	class FailureClass
}{
	{FailureInsufficientFunds, ClassDefinitive},
	{FailureBalanceLimitExceeded, ClassDefinitive},
	{FailureReversalInsufficientFunds, ClassDefinitive},
	{FailureWalletNotFound, ClassDefinitive},
	{FailureCurrencyMismatch, ClassCorrectable},
	{FailurePlayerMismatch, ClassCorrectable},
	{FailureReferenceNotFound, ClassDefinitive},
	{FailureReferenceNotProcessed, ClassDefinitive},
	{FailureReferenceKindInvalid, ClassCorrectable},
	{FailureReferenceMismatch, ClassCorrectable},
	{FailureReferenceAmountMismatch, ClassCorrectable},
	{FailureReferenceAlreadyReversed, ClassDefinitive},
	{FailureInternalPermanent, ClassDefinitive},
}

// FailureCodes returns every code.
func FailureCodes() []FailureCode {
	codes := make([]FailureCode, len(catalog))
	for i, entry := range catalog {
		codes[i] = entry.code
	}
	return codes
}

// IsValid reports whether the code is in the catalog.
func (c FailureCode) IsValid() bool {
	return slices.ContainsFunc(catalog, func(e struct {
		code  FailureCode
		class FailureClass
	}) bool {
		return e.code == c
	})
}

// IsRejection reports whether the code can end a transaction in REJECTED: every code
// but the internal permanent failure, which ends it in FAILED.
func (c FailureCode) IsRejection() bool { return c.IsValid() && c != FailureInternalPermanent }

// Class says whether the provider can correct the operation. An unknown code has no class.
func (c FailureCode) Class() FailureClass {
	for _, entry := range catalog {
		if entry.code == c {
			return entry.class
		}
	}
	return ""
}
