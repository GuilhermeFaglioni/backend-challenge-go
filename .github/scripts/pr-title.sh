#!/usr/bin/env bash
# Validates that the pull request title follows Conventional Commits.
#
# With squash merges the PR title becomes the commit message on main, so this
# is what keeps the history readable.
#
# The title is user-controlled text. It is read only from the PR_TITLE
# environment variable, never interpolated into a command line, and is only
# ever passed to printf as an argument, not as a format string.
set -euo pipefail

title="${PR_TITLE-}"

# The scope is limited to lowercase letters, digits and . _ / - so history
# stays uniform (feat(wallet), chore(deps)).
types='feat|fix|docs|chore|test|refactor|perf|ci|build'
pattern="^(${types})(\\([a-z0-9._/-]+\\))?!?: [^[:space:]].*\$"

# A title is a single line; `.` in a bash regex would otherwise match newlines.
if [[ "$title" != *$'\n'* && "$title" =~ $pattern ]]; then
  exit 0
fi

printf 'PR title is not a Conventional Commit: %s\n' "$title" >&2
cat >&2 <<EOF

Expected: <type>(<optional scope>)<optional !>: <description>
Types:    ${types//|/, }
Examples: feat(wallet): open wallet
          fix!: reject negative amounts
          chore(deps): bump the go-modules group
EOF
exit 1
