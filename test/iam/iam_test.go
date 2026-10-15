// Package iam checks the least-privilege policies of deploy/aws/iam against the code: each
// component's policy must allow exactly the SQS calls its code makes, on exactly the queues
// it makes them on, and nothing else. A call added to the code without its permission, or a
// permission left behind after its call is gone, fails here. Nothing here talks to AWS.
package iam

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/sqsconsumer"
	"github.com/GuilhermeFaglioni/backend-challenge-go/internal/adapters/sqspublisher"
)

const repoRoot = "../.."

type statement struct {
	Sid      string
	Effect   string
	Action   []string
	Resource string
}

type policy struct {
	Version   string
	Statement []statement
}

func load(t *testing.T, name string) policy {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, "deploy", "aws", "iam", name)) //nolint:gosec // a file of the repository
	require.NoError(t, err)
	var p policy
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&p))
	require.Equal(t, "2012-10-17", p.Version)
	return p
}

// actionsOf turns the methods of a client interface into the IAM actions they need.
// SendMessageBatch is authorised by sqs:SendMessage.
func actionsOf(api any) []string {
	typ := reflect.TypeOf(api).Elem()
	var out []string
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		if name == "SendMessageBatch" {
			name = "SendMessage"
		}
		out = append(out, "sqs:"+name)
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// granted is every action of the policy, by queue.
func granted(t *testing.T, p policy) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, s := range p.Statement {
		require.Equal(t, "Allow", s.Effect, s.Sid)
		require.True(t, strings.HasPrefix(s.Resource, "arn:aws:sqs:*:*:"), "%s: a resource is a queue ARN", s.Sid)
		queue := strings.TrimPrefix(s.Resource, "arn:aws:sqs:*:*:")
		require.NotContains(t, queue, "*", "%s: no wildcard queue", s.Sid)
		for _, a := range s.Action {
			require.NotContains(t, a, "*", "%s: no wildcard action", s.Sid)
			out[queue] = append(out[queue], a)
		}
		sort.Strings(out[queue])
	}
	return out
}

func union(m map[string][]string) []string {
	var all []string
	for _, actions := range m {
		all = append(all, actions...)
	}
	sort.Strings(all)
	return slices.Compact(all)
}

func TestTheConsumerMayDoExactlyWhatItsCodeDoes(t *testing.T) {
	g := granted(t, load(t, "consumer-policy.json"))

	require.Equal(t, actionsOf((*sqsconsumer.API)(nil)), union(g),
		"the policy allows exactly the calls of sqsconsumer.API (the readiness check's GetQueueUrl is among them)")
	require.Equal(t, map[string][]string{
		"wager-transactions.fifo":     {"sqs:ChangeMessageVisibility", "sqs:DeleteMessage", "sqs:GetQueueUrl", "sqs:ReceiveMessage"},
		"wager-transactions-dlq.fifo": {"sqs:GetQueueUrl", "sqs:SendMessage"},
	}, g, "it reads the input queue and only writes the dead-letter queue; it never touches the events queue")
}

func TestThePublisherMayDoExactlyWhatItsCodeDoes(t *testing.T) {
	g := granted(t, load(t, "publisher-policy.json"))

	require.Equal(t, actionsOf((*sqspublisher.API)(nil)), union(g), "the policy allows exactly the calls of sqspublisher.API")
	require.Equal(t, map[string][]string{
		"wager-events.fifo": {"sqs:GetQueueUrl", "sqs:SendMessage"},
	}, g, "it only writes the events queue: it cannot read it, nor touch the input queues")
}

// The queue names in the policies are the ones the environment provisions.
func TestThePoliciesNameTheProvisionedQueues(t *testing.T) {
	script, err := os.ReadFile(filepath.Join(repoRoot, "deploy", "localstack", "init", "01-sqs.sh"))
	require.NoError(t, err)
	for _, file := range []string{"consumer-policy.json", "publisher-policy.json"} {
		for queue := range granted(t, load(t, file)) {
			require.Contains(t, string(script), queue, "%s names %s, which the init script does not create", file, queue)
		}
	}
	iamScript, err := os.ReadFile(filepath.Join(repoRoot, "deploy", "localstack", "init", "02-iam.sh"))
	require.NoError(t, err)
	require.Contains(t, string(iamScript), "${component}-policy.json")
}
