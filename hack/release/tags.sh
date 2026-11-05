#!/usr/bin/env bash
# Points each tag given at the image index IMAGE@DIGEST, then fails unless each tag names that digest (RFC0007,
# requirement 22). With one source and no annotation, docker buildx imagetools create pushes the index as it is.
#
#   tags.sh IMAGE@DIGEST TAG...
set -euo pipefail

refuse() {
	echo "tags.sh: $*" >&2
	exit 1
}

[[ $# -ge 2 ]] || refuse "usage: tags.sh IMAGE@DIGEST TAG..."
source=$1
shift
digest=${source##*@}
[[ $source == *@* && $digest =~ ^sha256:[0-9a-f]{64}$ ]] || refuse "$source is not IMAGE@sha256:..."
args=()
for tag; do
	[[ -n $tag ]] && args+=(--tag "$tag")
done
[[ ${#args[@]} -gt 0 ]] || refuse "no tag given"
docker buildx imagetools create "${args[@]}" "$source"
for tag; do
	[[ -n $tag ]] || continue
	got=$(docker buildx imagetools inspect "$tag" --format '{{json .Manifest}}' | jq -r .digest)
	[[ $got == "$digest" ]] || refuse "$tag names $got, not $digest"
	echo "tags.sh: $tag names $digest"
done
