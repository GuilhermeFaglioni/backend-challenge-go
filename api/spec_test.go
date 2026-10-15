package api_test

import (
	"context"
	"regexp"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/api"
)

func load(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData(api.OpenAPI)
	require.NoError(t, err)
	return doc
}

func TestTheDocumentIsAValidOpenAPIDocument(t *testing.T) {
	require.NoError(t, load(t).Validate(context.Background()))
}

// Every operation says who may call it: either a security requirement, or an
// explicit empty one (`security: []`) for the public routes. An operation that
// inherits nothing by accident is a documentation hole.
func TestEveryOperationStatesItsSecurity(t *testing.T) {
	doc := load(t)
	require.NotNil(t, doc.Security, "a document-wide default is required")
	public := map[string]bool{"/health/live": true, "/health/ready": true, "/metrics": true, "/openapi.yaml": true}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if public[path] {
				require.NotNil(t, op.Security, "%s %s must opt out explicitly", method, path)
				require.Empty(t, *op.Security, "%s %s is public", method, path)
				continue
			}
			require.Nil(t, op.Security, "%s %s must inherit the client-credentials requirement", method, path)
		}
	}
}

func TestTheSecuritySchemeIsClientCredentialsThroughKeycloak(t *testing.T) {
	scheme := load(t).Components.SecuritySchemes["clientCredentials"].Value
	require.Equal(t, "oauth2", scheme.Type)
	require.NotNil(t, scheme.Flows.ClientCredentials)
	require.Contains(t, scheme.Flows.ClientCredentials.TokenURL, "/realms/wagering/protocol/openid-connect/token")
}

// Money's amount pattern is the strict form of ADR 0003; the schema must accept
// exactly what the domain accepts.
func TestTheMoneySchemaMatchesTheStrictAmountFormat(t *testing.T) {
	amount := load(t).Components.Schemas["Money"].Value.Properties["amount"].Value
	pattern := regexp.MustCompile(amount.Pattern)
	for _, good := range []string{"0.00", "0.01", "25.00", "100.50", "123456789.99"} {
		require.True(t, pattern.MatchString(good), good)
	}
	for _, bad := range []string{"25", "25.5", "25.500", "-1.00", "01.00", "1e3", "NaN", "", ".50", "1,00", " 1.00"} {
		require.False(t, pattern.MatchString(bad), bad)
	}
}

func TestEveryErrorCodeOfTheContractIsListed(t *testing.T) {
	codes := load(t).Components.Schemas["ErrorCode"].Value.Enum
	for _, want := range []string{"INVALID_REQUEST", "UNAUTHENTICATED", "FORBIDDEN", "NOT_FOUND", "WALLET_ALREADY_EXISTS", "UNAVAILABLE", "INTERNAL_ERROR"} {
		require.Contains(t, codes, want)
	}
}
