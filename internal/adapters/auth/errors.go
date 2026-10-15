package auth

import "errors"

// ErrMisconfigured marks a failure that retrying cannot fix: the identity provider
// answered, but what it says does not match the configuration (a different issuer,
// an unusable keys URL). Start-up gives up on it at once instead of waiting for the
// deadline.
var ErrMisconfigured = errors.New("auth: the identity provider does not match the configuration")

// Reason says why authentication failed. The set is closed and small on purpose:
// it is a metric label, and none of them carries token content.
type Reason string

const (
	ReasonMissing              Reason = "missing"               // no Authorization header
	ReasonMalformed            Reason = "malformed"             // not a bearer token, not a JWT, no key id
	ReasonUnsupportedAlgorithm Reason = "unsupported_algorithm" // `alg: none`, HMAC, anything but RS256
	ReasonUnknownKey           Reason = "unknown_key"           // signed by a key the identity provider does not publish
	ReasonBadSignature         Reason = "bad_signature"         // the signature does not match
	ReasonExpired              Reason = "expired"
	ReasonNotYetValid          Reason = "not_yet_valid"
	ReasonWrongIssuer          Reason = "wrong_issuer"
	ReasonWrongAudience        Reason = "wrong_audience"
	ReasonInvalidClaims        Reason = "invalid_claims" // missing exp, bad provider id, provider without provider id
	// ReasonKeysUnavailable is not the caller's fault: the signing key was unknown
	// and the identity provider could not be reached to look for it. Retryable.
	ReasonKeysUnavailable Reason = "keys_unavailable"
	// ReasonForbidden is recorded by the HTTP layer: authenticated, wrong role.
	ReasonForbidden Reason = "forbidden_role"
)

// Error is an authentication failure. Its text names only the reason; the
// underlying cause (available through Unwrap) is for tests and debugging and is
// never sent to the caller.
type Error struct {
	Reason Reason
	cause  error
}

func (e *Error) Error() string { return "auth: " + string(e.Reason) }
func (e *Error) Unwrap() error { return e.cause }

func fail(reason Reason, cause error) *Error { return &Error{Reason: reason, cause: cause} }
