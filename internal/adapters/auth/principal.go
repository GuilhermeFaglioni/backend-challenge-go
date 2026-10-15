// Package auth validates the OIDC access tokens issued by the identity provider
// and turns them into a Principal: who is calling, with which roles, and, for a
// game provider, which provider id. The service never stores passwords and never
// issues tokens (ARCHITECTURE.md, section 13).
package auth

import (
	"context"
	"slices"
	"sort"
)

// Role is a permission the identity provider grants to a client.
type Role string

const (
	// RoleProvider is a game provider: it may only touch its own transactions.
	RoleProvider Role = "provider"
	// RoleInternal is the internal service: wallet operations.
	RoleInternal Role = "internal"
)

var knownRoles = []Role{RoleProvider, RoleInternal}

// Principal is the authenticated caller. It is immutable.
type Principal struct {
	subject    string
	clientID   string
	providerID string
	roles      []Role // sorted, no duplicates, known roles only
}

// NewPrincipal builds a Principal. Unknown roles are dropped, and so is the
// provider id unless the caller has the provider role: it identifies a provider
// and means nothing for anyone else, so it must not travel with them. The verifier
// is the only production caller; tests use it to stand in for a verified token.
func NewPrincipal(subject, clientID, providerID string, roles ...Role) Principal {
	kept := make([]Role, 0, len(roles))
	for _, r := range roles {
		if slices.Contains(knownRoles, r) && !slices.Contains(kept, r) {
			kept = append(kept, r)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i] < kept[j] })
	if !slices.Contains(kept, RoleProvider) {
		providerID = ""
	}
	return Principal{subject: subject, clientID: clientID, providerID: providerID, roles: kept}
}

// Subject is the token's `sub`.
func (p Principal) Subject() string { return p.subject }

// ClientID is the client that obtained the token (`azp`).
func (p Principal) ClientID() string { return p.clientID }

// ProviderID is the provider the caller acts for. It comes from the token, never
// from a request body; it is empty for callers without the provider role.
func (p Principal) ProviderID() string { return p.providerID }

// Roles returns a copy of the caller's roles.
func (p Principal) Roles() []Role { return slices.Clone(p.roles) }

// HasRole reports whether the caller has the role.
func (p Principal) HasRole(r Role) bool { return slices.Contains(p.roles, r) }

// HasAnyRole reports whether the caller has at least one of the roles.
func (p Principal) HasAnyRole(roles ...Role) bool {
	return slices.ContainsFunc(roles, p.HasRole)
}

type principalKey struct{}

// WithPrincipal returns a context that carries the principal.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal of a request that went through authentication.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
