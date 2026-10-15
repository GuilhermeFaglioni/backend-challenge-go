package application_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/application"
)

// "" stands for an internal transaction, which has no provider.
func TestAScopeReachesOnlyWhatItIsMeantTo(t *testing.T) {
	cases := map[string]struct {
		scope   application.Scope
		reaches map[string]bool
	}{
		"everything":              {application.AllTransactions(), map[string]bool{"provider-a": true, "provider-b": true, "": true}},
		"one provider":            {application.ProviderTransactions("provider-a"), map[string]bool{"provider-a": true, "provider-b": false, "": false}},
		"a provider with no name": {application.ProviderTransactions(""), map[string]bool{"provider-a": false, "": false}},
		"the zero scope":          {application.Scope{}, map[string]bool{"provider-a": false, "": false}},
	}
	for name, c := range cases {
		for provider, want := range c.reaches {
			require.Equal(t, want, c.scope.Reaches(provider), "%s reaching %q", name, provider)
		}
	}
}

func TestOnlyTheAllScopeIsAll(t *testing.T) {
	require.True(t, application.AllTransactions().All())
	require.False(t, application.ProviderTransactions("provider-a").All())
	require.False(t, application.Scope{}.All())
	require.Equal(t, "provider-a", application.ProviderTransactions("provider-a").ProviderID())
}

func TestAWalletAlreadyExistsErrorMatchesItsSentinelAndNamesTheWallet(t *testing.T) {
	err := &application.WalletAlreadyExistsError{}
	require.ErrorIs(t, err, application.ErrWalletAlreadyExists)
	require.Contains(t, err.Error(), "already exists")
}
