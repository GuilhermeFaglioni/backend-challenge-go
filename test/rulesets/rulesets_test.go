// Package rulesets checks the repository rulesets stored in .github/rulesets
// against the workflows they protect. The rules are applied by
// .github/scripts/apply-rulesets.sh; nothing here talks to GitHub.
package rulesets

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// repoRoot is two levels up from this package (test/rulesets).
const repoRoot = "../.."

const (
	// githubActionsAppID is the id of the GitHub Actions app (the `app.id` of a
	// check run produced by a workflow).
	githubActionsAppID = 15368
	// adminRoleID is the repository role "admin" in a ruleset's bypass list.
	adminRoleID = 5
)

type ruleset struct {
	Name         string `json:"name"`
	Target       string `json:"target"`
	Enforcement  string `json:"enforcement"`
	BypassActors []struct {
		ActorID    int    `json:"actor_id"`
		ActorType  string `json:"actor_type"`
		BypassMode string `json:"bypass_mode"`
	} `json:"bypass_actors"`
	Conditions struct {
		RefName struct {
			Include []string `json:"include"`
		} `json:"ref_name"`
	} `json:"conditions"`
	Rules []struct {
		Type       string          `json:"type"`
		Parameters json.RawMessage `json:"parameters"`
	} `json:"rules"`
}

func load(t *testing.T, file string) ruleset {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, ".github", "rulesets", file)) //nolint:gosec // a fixed file of this repository
	require.NoError(t, err)
	var r ruleset
	require.NoError(t, json.Unmarshal(data, &r), "%s is not valid JSON for a ruleset", file)
	return r
}

func (r ruleset) rule(t *testing.T, kind string) json.RawMessage {
	t.Helper()
	for _, rule := range r.Rules {
		if rule.Type == kind {
			if rule.Parameters == nil {
				return json.RawMessage("{}")
			}
			return rule.Parameters
		}
	}
	return nil
}

func (r ruleset) requiredChecks(t *testing.T) []string {
	t.Helper()
	raw := r.rule(t, "required_status_checks")
	require.NotNil(t, raw, "the ruleset has no required_status_checks rule")
	var params struct {
		Strict bool `json:"strict_required_status_checks_policy"`
		Checks []struct {
			Context       string `json:"context"`
			IntegrationID int    `json:"integration_id"`
		} `json:"required_status_checks"`
	}
	require.NoError(t, json.Unmarshal(raw, &params))
	require.True(t, params.Strict, "the branch must be up to date with main before merging")
	var contexts []string
	for _, c := range params.Checks {
		// Only a GitHub Actions job may satisfy a check: without this any app,
		// or the commit status API, could report success under the same name.
		require.Equal(t, githubActionsAppID, c.IntegrationID, "check %q must come from GitHub Actions", c.Context)
		contexts = append(contexts, c.Context)
	}
	return contexts
}

// workflowFile is the part of a workflow the rules depend on.
type workflowFile struct {
	path string
	On   any `yaml:"on"`
	Jobs map[string]struct {
		Name string `yaml:"name"`
		If   string `yaml:"if"`
	} `yaml:"jobs"`
}

func loadWorkflows(t *testing.T) []workflowFile {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repoRoot, ".github", "workflows", "*.yml"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "no workflows found")

	var workflows []workflowFile
	for _, file := range files {
		data, err := os.ReadFile(file) //nolint:gosec // a workflow file found by globbing this repository
		require.NoError(t, err)
		w := workflowFile{path: filepath.Base(file)}
		require.NoError(t, yaml.Unmarshal(data, &w), file)
		workflows = append(workflows, w)
	}
	return workflows
}

// pullRequestWorkflows are the workflows whose jobs a pull request shows, and
// so the ones a ruleset can require.
func pullRequestWorkflows(t *testing.T) []workflowFile {
	t.Helper()
	var out []workflowFile
	for _, w := range loadWorkflows(t) {
		if _, ok := pullRequestTrigger(w.On); ok {
			out = append(out, w)
		}
	}
	return out
}

// pullRequestJobNames returns the display name of every job of those workflows.
func pullRequestJobNames(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, w := range pullRequestWorkflows(t) {
		for id, job := range w.Jobs {
			if job.Name != "" {
				names = append(names, job.Name)
			} else {
				names = append(names, id)
			}
		}
	}
	return names
}

// pullRequestTrigger reports whether the workflow runs on pull requests and
// returns its filters (nil when it has none).
func pullRequestTrigger(on any) (filters map[string]any, ok bool) {
	switch v := on.(type) {
	case string:
		return nil, v == "pull_request"
	case []any:
		return nil, slices.Contains(v, any("pull_request"))
	case map[string]any:
		trigger, ok := v["pull_request"]
		if !ok {
			return nil, false
		}
		filters, _ = trigger.(map[string]any)
		return filters, true
	}
	return nil, false
}

func TestRequiredChecksAreExactlyThePullRequestJobs(t *testing.T) {
	required := load(t, "main.json").requiredChecks(t)
	produced := pullRequestJobNames(t)

	require.Len(t, required, len(slices.Compact(slices.Sorted(slices.Values(required)))), "a check is listed twice")

	// A required check that no job produces would block every merge forever.
	for _, check := range required {
		require.Contains(t, produced, check, "the ruleset requires %q but no pull request workflow has a job with that name", check)
	}
	// A job that is not required could fail without blocking the merge.
	for _, job := range produced {
		require.Contains(t, required, job, "the job %q runs on pull requests but is not a required check", job)
	}
}

// A required check that is never produced leaves the pull request pending for
// ever, and nothing short of editing the ruleset by hand unblocks it. So every
// pull request must get every required job: no path or branch filter on the
// trigger, only activity types that include the ones that matter, and no job
// that can be skipped by an `if`.
func TestRequiredChecksAreAlwaysProduced(t *testing.T) {
	for _, w := range pullRequestWorkflows(t) {
		filters, _ := pullRequestTrigger(w.On)
		for _, filter := range []string{"paths", "paths-ignore", "branches", "branches-ignore"} {
			require.NotContains(t, filters, filter, "%s: a %q filter on pull_request would skip required checks on some pull requests", w.path, filter)
		}
		if types, ok := filters["types"].([]any); ok {
			for _, needed := range []string{"opened", "synchronize", "reopened"} {
				require.Contains(t, types, any(needed), "%s: pull_request types must include %q", w.path, needed)
			}
		}
		for id, job := range w.Jobs {
			require.Empty(t, job.If, "%s: job %q has an `if` and could be skipped, leaving its required check missing", w.path, id)
		}
	}
}

func TestMainBranchRules(t *testing.T) {
	r := load(t, "main.json")

	require.Equal(t, "branch", r.Target)
	require.Equal(t, "active", r.Enforcement)
	require.Equal(t, []string{"~DEFAULT_BRANCH"}, r.Conditions.RefName.Include)
	require.Empty(t, r.BypassActors, "nobody may bypass the rules, administrators included")

	for _, kind := range []string{"deletion", "non_fast_forward", "required_linear_history"} {
		require.NotNil(t, r.rule(t, kind), "missing rule %s", kind)
	}

	raw := r.rule(t, "pull_request")
	require.NotNil(t, raw, "changes must go through a pull request")
	var pr struct {
		Approvals int      `json:"required_approving_review_count"`
		Methods   []string `json:"allowed_merge_methods"`
	}
	require.NoError(t, json.Unmarshal(raw, &pr))
	require.Equal(t, 0, pr.Approvals, "a single maintainer cannot approve their own pull request")
	require.Equal(t, []string{"squash"}, pr.Methods, "only squash merge is allowed")
}

func TestReleaseTagRules(t *testing.T) {
	r := load(t, "tags.json")

	require.Equal(t, "tag", r.Target)
	require.Equal(t, "active", r.Enforcement)
	require.Equal(t, []string{"refs/tags/v*"}, r.Conditions.RefName.Include)
	for _, kind := range []string{"creation", "update", "deletion", "non_fast_forward"} {
		require.NotNil(t, r.rule(t, kind), "missing rule %s", kind)
	}

	// Only repository administrators may create release tags: a release tag
	// triggers the publish workflow with package-write permission.
	require.Len(t, r.BypassActors, 1)
	require.Equal(t, "RepositoryRole", r.BypassActors[0].ActorType)
	require.Equal(t, adminRoleID, r.BypassActors[0].ActorID, "the bypass role must be admin")
	require.Equal(t, "always", r.BypassActors[0].BypassMode)
}
