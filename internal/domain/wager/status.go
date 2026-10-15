package wager

// Kind says what a Wager Transaction does. Only the internal opening of a wallet
// exists so far; the kinds an external Operation request can have arrive with the
// use case that processes them.
type Kind string

const KindOpening Kind = "OPENING"

// Origin says who started a Wager Transaction.
type Origin string

const OriginInternal Origin = "INTERNAL"

// Status is the state of a Wager Transaction (ARCHITECTURE.md, section 9).
type Status string

const (
	StatusPending          Status = "PENDING"
	StatusPendingReference Status = "PENDING_REFERENCE"
	StatusProcessed        Status = "PROCESSED"
	StatusRejected         Status = "REJECTED"
	StatusFailed           Status = "FAILED"
)

// transitions is the state machine. A state missing from it is terminal.
var transitions = map[Status][]Status{
	StatusPending:          {StatusProcessed, StatusRejected, StatusPendingReference, StatusFailed},
	StatusPendingReference: {StatusProcessed, StatusRejected, StatusFailed},
}

// IsValid reports whether the status is one of the five.
func (s Status) IsValid() bool {
	switch s {
	case StatusPending, StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed:
		return true
	}
	return false
}

// IsTerminal reports whether no transition leaves the status.
func (s Status) IsTerminal() bool { return s.IsValid() && len(transitions[s]) == 0 }

// CanTransitionTo reports whether the state machine allows going from s to next.
func (s Status) CanTransitionTo(next Status) bool {
	for _, allowed := range transitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// FailureCode explains a Rejection or a Failure.
type FailureCode string

// FailureInternalPermanent is the code of a Failure: a permanent defect of the
// infrastructure, recorded for audit.
const FailureInternalPermanent FailureCode = "INTERNAL_PERMANENT_FAILURE"
