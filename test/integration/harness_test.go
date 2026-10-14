//go:build integration

package integration

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/test/infra"
)

// These tests check that the shared infrastructure is provisioned exactly as
// the application and later test layers will expect it. Tests of authentication
// and messaging build on top of this.

type claims struct {
	Issuer     string   `json:"iss"`
	Audience   any      `json:"aud"`
	ProviderID string   `json:"provider_id"`
	Resource   resource `json:"resource_access"`
}

type resource map[string]struct {
	Roles []string `json:"roles"`
}

// decodeClaims reads the payload of a JWT without verifying it: the test only
// inspects what the realm puts in the token.
func decodeClaims(t *testing.T, token string) claims {
	t.Helper()
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3, "an access token is a three-part JWT")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var c claims
	require.NoError(t, json.Unmarshal(payload, &c))
	return c
}

func TestKeycloakRealmIssuesTokensForTheConfiguredClients(t *testing.T) {
	ctx := t.Context()

	tests := []struct {
		client, secret, wantProvider, wantRole string
	}{
		{"provider-a", "provider-a-local-secret", "provider-a", "provider"},
		{"provider-b", "provider-b-local-secret", "provider-b", "provider"},
		{"internal-service", "internal-service-local-secret", "", "internal"},
	}
	for _, tt := range tests {
		t.Run(tt.client, func(t *testing.T) {
			token, err := kc.Token(ctx, tt.client, tt.secret)
			require.NoError(t, err)

			c := decodeClaims(t, token)
			require.Equal(t, kc.IssuerURL(), c.Issuer)
			require.Equal(t, "wallet-api", firstAudience(c.Audience))
			require.Equal(t, tt.wantProvider, c.ProviderID, "provider_id comes from the client, never from the request body")
			require.Contains(t, c.Resource["wallet-api"].Roles, tt.wantRole)
		})
	}

	t.Run("a wrong secret is refused", func(t *testing.T) {
		_, err := kc.Token(ctx, "provider-a", "wrong")
		var tokenErr *infra.TokenError
		require.ErrorAs(t, err, &tokenErr, "want a refusal from Keycloak, not a network failure")
		require.Equal(t, http.StatusUnauthorized, tokenErr.Status)
	})

	t.Run("providers get no internal role and internal gets no provider id", func(t *testing.T) {
		providerToken, err := kc.Token(ctx, "provider-a", "provider-a-local-secret")
		require.NoError(t, err)
		require.NotContains(t, decodeClaims(t, providerToken).Resource["wallet-api"].Roles, "internal")

		internalToken, err := kc.Token(ctx, "internal-service", "internal-service-local-secret")
		require.NoError(t, err)
		require.Empty(t, decodeClaims(t, internalToken).ProviderID)
		require.NotContains(t, decodeClaims(t, internalToken).Resource["wallet-api"].Roles, "provider")
	})
}

// firstAudience returns the audience whether the token carries a string or a list.
func firstAudience(aud any) any {
	if list, ok := aud.([]any); ok && len(list) > 0 {
		return list[0]
	}
	return aud
}

func newSQSClient(t *testing.T) *sqs.Client {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(t.Context(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	require.NoError(t, err)
	return sqs.NewFromConfig(cfg, func(o *sqs.Options) { o.BaseEndpoint = aws.String(ls.Endpoint) })
}

func TestLocalStackProvisionsTheQueues(t *testing.T) {
	ctx := t.Context()
	client := newSQSClient(t)

	queueURL := func(name string) string {
		out, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
		require.NoError(t, err, "queue %s must exist", name)
		return aws.ToString(out.QueueUrl)
	}

	input := queueURL("wager-transactions.fifo")
	dlq := queueURL("wager-transactions-dlq.fifo")
	queueURL("wager-events.fifo")

	attrs, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(input),
		AttributeNames: []sqstypes.QueueAttributeName{
			sqstypes.QueueAttributeNameRedrivePolicy,
			sqstypes.QueueAttributeNameFifoQueue,
			sqstypes.QueueAttributeNameVisibilityTimeout,
		},
	})
	require.NoError(t, err)
	require.Equal(t, "true", attrs.Attributes["FifoQueue"])
	require.Equal(t, "30", attrs.Attributes["VisibilityTimeout"])

	var redrive struct {
		DeadLetterTargetArn string `json:"deadLetterTargetArn"`
		MaxReceiveCount     any    `json:"maxReceiveCount"`
	}
	require.NoError(t, json.Unmarshal([]byte(attrs.Attributes["RedrivePolicy"]), &redrive))
	require.Equal(t, "5", fmt.Sprint(redrive.MaxReceiveCount))

	dlqAttrs, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(dlq),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
	})
	require.NoError(t, err)
	require.Equal(t, dlqAttrs.Attributes["QueueArn"], redrive.DeadLetterTargetArn, "the input queue redrives to the DLQ")
}
