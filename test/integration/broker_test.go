//go:build integration

package integration

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	tcexec "github.com/testcontainers/testcontainers-go/exec"

	"github.com/GuilhermeFaglioni/backend-challenge-go/test/infra"
)

func awslocal(t *testing.T, args ...string) string {
	t.Helper()
	code, out, err := ls.Container.Exec(t.Context(), append([]string{"awslocal", "--region", "us-east-1"}, args...), tcexec.Multiplexed())
	require.NoError(t, err)
	data, err := io.ReadAll(out)
	require.NoError(t, err)
	require.Zero(t, code, string(data))
	return string(data)
}

func canonical(t *testing.T, raw string) string {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal([]byte(raw), &v))
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return string(data)
}

// The environment provisions one IAM user per component with the policy of
// deploy/aws/iam, the same documents the code is checked against (test/iam). LocalStack's
// free edition does not enforce them; this proves they are provisioned as written.
func TestEachComponentHasItsOwnUserWithItsLeastPrivilegePolicy(t *testing.T) {
	for _, component := range []string{"consumer", "publisher"} {
		user := "wagering-" + component
		var attached struct {
			AttachedPolicies []struct{ PolicyName, PolicyArn string }
		}
		require.NoError(t, json.Unmarshal([]byte(awslocal(t, "iam", "list-attached-user-policies", "--user-name", user)), &attached))
		require.Len(t, attached.AttachedPolicies, 1, user)
		require.Equal(t, user+"-sqs", attached.AttachedPolicies[0].PolicyName)

		var version struct {
			Policy struct{ DefaultVersionId string }
		}
		require.NoError(t, json.Unmarshal([]byte(awslocal(t, "iam", "get-policy", "--policy-arn", attached.AttachedPolicies[0].PolicyArn)), &version))
		var doc struct {
			PolicyVersion struct{ Document json.RawMessage }
		}
		require.NoError(t, json.Unmarshal([]byte(awslocal(t, "iam", "get-policy-version", "--policy-arn", attached.AttachedPolicies[0].PolicyArn,
			"--version-id", version.Policy.DefaultVersionId)), &doc))
		document := string(doc.PolicyVersion.Document)
		if strings.HasPrefix(document, `"`) { // some versions return it as an encoded string
			require.NoError(t, json.Unmarshal(doc.PolicyVersion.Document, &document))
		}
		file, err := os.ReadFile(filepath.Join(infra.RepoRoot(), "deploy", "aws", "iam", component+"-policy.json")) //nolint:gosec // a file of the repository
		require.NoError(t, err)
		require.Equal(t, canonical(t, string(file)), canonical(t, document), user)
	}
}
