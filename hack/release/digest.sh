#!/usr/bin/env bash
# Prints the digest of the manifest that a tag names, as the registry answers it, and fails when the registry answers
# anything but the manifest with a sha256 digest.
#
#   digest.sh HOST/NAME:TAG   in an OCI registry that serves anonymous pull tokens, as ghcr.io
set -euo pipefail

refuse() {
	echo "digest.sh: $*" >&2
	exit 1
}

[[ $# -eq 1 ]] || refuse "usage: digest.sh HOST/NAME:TAG"
ref=$1
host=${ref%%/*} rest=${ref#*/}
name=${rest%:*} tag=${rest##*:}
[[ $host != "$ref" && $name != "$rest" && -n $tag ]] || refuse "$ref is not HOST/NAME:TAG"
token=$(curl -fsS --retry 3 "https://$host/token?scope=repository:$name:pull&service=$host" | jq -er .token) ||
	refuse "$ref: no pull token from $host"
headers=$(curl -sS --retry 3 -o /dev/null -D - -I \
	-H "Authorization: Bearer $token" \
	-H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
	"https://$host/v2/$name/manifests/$tag" | tr -d '\r') || refuse "$ref: no answer from $host"
status=$(sed -n '1s/^HTTP\/[0-9.]* \([0-9]*\).*/\1/p' <<<"$headers")
[[ $status == 200 ]] || refuse "$ref: HTTP $status"
digest=$(sed -n 's/^[Dd]ocker-[Cc]ontent-[Dd]igest: *//p' <<<"$headers")
[[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || refuse "$ref: no sha256 digest in the answer: '$digest'"
echo "$digest"
