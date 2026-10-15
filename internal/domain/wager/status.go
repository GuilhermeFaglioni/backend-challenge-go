package wager

import "slices"

// Kind says what a Wager Transaction does (CONTEXT.md; ARCHITECTURE.md, section 10).
// OPENING is the internal opening of a wallet and can never come from a provider.
type Kind string

const (
	KindOpening  Kind = "OPENING"
	KindBet      Kind = "BET"
	KindWin      Kind = "WIN"
	KindLoss     Kind = "LOSS"
	KindRefund   Kind = "REFUND"
	KindRollback Kind = "ROLLBACK"
)

// externalKinds are the kinds a provider may send.
var externalKinds = []Kind{KindBet, KindWin, KindLoss, KindRefund, KindRollback}

// IsExternal reports whether a provider may send this kind.
func (k Kind) IsExternal() bool { return slices.Contains(externalKinds, k) }

// Origin says who started a Wager Transaction.
type Origin string

const (
	OriginInternal Origin = "INTERNAL"
	OriginExternal Origin = "EXTERNAL"
)

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
