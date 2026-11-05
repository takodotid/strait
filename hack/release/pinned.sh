#!/usr/bin/env bash
# Exits 1, and lists them, when a workflow or a local action of the directory given, the repository's root by default,
# uses an action that is not pinned by commit SHA (RFC0007, requirement 24). A local action, ./PATH, is the commit's
# own.
#
#   pinned.sh [ROOT]
set -euo pipefail

root=${1:-$(cd "$(dirname "$0")/../.." && pwd)}
shopt -s nullglob
files=("$root"/.github/workflows/*.yaml "$root"/.github/workflows/*.yml "$root"/.github/actions/*/action.yaml "$root"/.github/actions/*/action.yml)
[[ ${#files[@]} -gt 0 ]] || {
	echo "pinned.sh: no workflow in $root/.github" >&2
	exit 1
}
bad=$(grep -nE '^[[:space:]-]*uses:' "${files[@]}" | grep -vE 'uses:[[:space:]]+(\./[^[:space:]]+|[^[:space:]@]+@[0-9a-f]{40})([[:space:]]+#.*)?$' || true)
if [[ -n $bad ]]; then
	echo "pinned.sh: actions not pinned by commit SHA:" >&2
	echo "$bad" >&2
	exit 1
fi
echo "pinned.sh: every action is pinned by commit SHA"
