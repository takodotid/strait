#!/usr/bin/env bash
# Exits 0 when the tag may be released, and 1 with the reason when it may not (RFC0007, requirement 17): the tag must
# be an annotated tag vX.Y.Z on the commit given, whose signature GitHub verifies; the commit must be on main; and the
# ci run of its push to main must have finished with every job of ci.yaml successful. Reads GitHub with gh.
#
#   gate.sh OWNER/REPO TAG COMMIT
set -euo pipefail

refuse() {
	echo "gate.sh: release refused: $*" >&2
	exit 1
}

[[ $# -eq 3 ]] || refuse "usage: gate.sh OWNER/REPO TAG COMMIT"
repo=$1 tag=$2 commit=$3
[[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || refuse "tag $tag is not vX.Y.Z"

# A lightweight tag points at the commit itself, and carries no signature.
ref=$(gh api "repos/$repo/git/ref/tags/$tag" --jq '.object.type + " " + .object.sha') || refuse "cannot read tag $tag"
read -r type object <<<"$ref"
[[ $type == tag ]] || refuse "tag $tag is not annotated, so it carries no signature"
obj=$(gh api "repos/$repo/git/tags/$object" --jq '[.tag, .object.sha, .verification.verified, .verification.reason] | @tsv') ||
	refuse "cannot read tag object $object"
IFS=$'\t' read -r name target verified reason <<<"$obj"
[[ $name == "$tag" ]] || refuse "tag object $object is named $name, not $tag"
[[ $target == "$commit" ]] || refuse "tag $tag points at $target, not at $commit"
[[ $verified == true ]] || refuse "GitHub does not verify the signature of tag $tag: $reason"
echo "tag $tag: annotated, signature verified by GitHub, on $commit"

# main is ahead of the commit, or is the commit: the commit is on main.
status=$(gh api "repos/$repo/compare/$commit...main" --jq .status) || refuse "cannot compare $commit with main"
[[ $status == ahead || $status == identical ]] || refuse "commit $commit is not on main: main is $status of it"
echo "commit $commit: on main"

# The jobs of ci.yaml, by their keys, as this commit defines them.
mapfile -t want < <(sed -n '/^jobs:/,$s/^  \([a-zA-Z0-9_-]*\):$/\1/p' "$(dirname "$0")/../../.github/workflows/ci.yaml")
[[ ${#want[@]} -gt 0 ]] || refuse "no job found in .github/workflows/ci.yaml"

runs=$(gh api --paginate "repos/$repo/actions/workflows/ci.yaml/runs?head_sha=$commit&event=push&branch=main&per_page=100" \
	--jq '.workflow_runs[] | "\(.id) \(.status) \(.conclusion)"') || refuse "cannot list the ci runs of $commit"
unfinished=0
while read -r id state conclusion; do
	[[ -n $id ]] || continue
	if [[ $state != completed ]]; then
		unfinished=1
		continue
	fi
	[[ $conclusion == success ]] || continue
	jobs=$(gh api --paginate "repos/$repo/actions/runs/$id/jobs?filter=latest&per_page=100" \
		--jq '.jobs[] | select(.conclusion == "success") | .name') || refuse "cannot list the jobs of ci run $id"
	mapfile -t ok <<<"$jobs"
	missing=()
	for job in "${want[@]}"; do
		printf '%s\n' "${ok[@]}" | grep -qxF -- "$job" || missing+=("$job")
	done
	if [[ ${#missing[@]} -eq 0 ]]; then
		echo "ci run $id on main: every job successful (${want[*]})"
		exit 0
	fi
	echo "ci run $id on main: not successful: ${missing[*]}" >&2
done <<<"$runs"

if [[ $unfinished -eq 1 ]]; then
	refuse "the ci run of commit $commit on main has not finished. Re-run the release once it has succeeded."
fi
refuse "no ci run of commit $commit on main succeeded in every job (${want[*]})"
