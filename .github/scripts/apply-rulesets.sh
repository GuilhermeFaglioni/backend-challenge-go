#!/usr/bin/env bash
# Applies the repository's merge settings and the rulesets in .github/rulesets.
#
# Dry run by default: it only reads, and prints what --apply would do. Run it
# as a repository administrator, with the gh CLI authenticated:
#
#   bash .github/scripts/apply-rulesets.sh           # show the plan
#   bash .github/scripts/apply-rulesets.sh --apply   # change the repository
#
# Re-running with --apply is safe: rulesets are matched by name and updated in
# place, so nothing is duplicated.
set -euo pipefail

mode=dry
case "${1:-}" in
  "") ;;
  --apply) mode=apply ;;
  *)
    echo "usage: apply-rulesets.sh [--apply]" >&2
    exit 2
    ;;
esac

# Before anything is changed: a missing tool must not leave a half-applied state.
for tool in gh jq; do
  command -v "$tool" >/dev/null || { echo "apply-rulesets: $tool is required" >&2; exit 1; }
done

here="$(cd "$(dirname "$0")" && pwd)"
rulesets_dir="$here/../rulesets"
repo="$(gh repo view --json nameWithOwner --jq .nameWithOwner)"

if [ "$mode" = dry ]; then
  echo "Dry run for ${repo}: nothing is changed. Add --apply to apply."
fi

# do_api <description> <gh api arguments...>: runs it with --apply, else only describes it.
do_api() {
  local description="$1"
  shift
  if [ "$mode" = apply ]; then
    echo "Applying: ${description}"
    gh api "$@" >/dev/null
  else
    echo "Would: ${description}"
  fi
}

# Merge settings: squash only, so every commit on main is one reviewed pull
# request, titled by the (validated) pull request title.
do_api "set squash-only merging, squash commit title from the PR title, delete branches after merge" \
  -X PATCH "repos/${repo}" \
  -F allow_squash_merge=true \
  -F allow_merge_commit=false \
  -F allow_rebase_merge=false \
  -F delete_branch_on_merge=true \
  -f squash_merge_commit_title=PR_TITLE \
  -f squash_merge_commit_message=BLANK

existing="$(gh api "repos/${repo}/rulesets")"

for file in "$rulesets_dir"/*.json; do
  name="$(jq -r .name "$file")"
  id="$(jq -r --arg name "$name" '.[] | select(.name == $name) | .id' <<<"$existing")"
  if [ -n "$id" ]; then
    do_api "update ruleset \"${name}\" (id ${id}) from $(basename "$file")" \
      -X PUT "repos/${repo}/rulesets/${id}" --input "$file"
  else
    do_api "create ruleset \"${name}\" from $(basename "$file")" \
      -X POST "repos/${repo}/rulesets" --input "$file"
  fi
done

if [ "$mode" = apply ]; then
  # Ask GitHub what is now enforced on the default branch, rather than trusting
  # that the calls above did what was meant.
  default_branch="$(gh repo view --json defaultBranchRef --jq .defaultBranchRef.name)"
  enforced="$(gh api "repos/${repo}/rules/branches/${default_branch}" --jq '[.[].type] | unique | join(" ")')"
  for rule in pull_request required_status_checks required_linear_history non_fast_forward deletion; do
    case " $enforced " in
      *" $rule "*) ;;
      *)
        echo "apply-rulesets: ${default_branch} does not enforce '${rule}' (enforced: ${enforced})" >&2
        exit 1
        ;;
    esac
  done
  echo "Verified: ${default_branch} enforces: ${enforced}"
  echo "Review at https://github.com/${repo}/rules"
fi
