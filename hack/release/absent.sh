#!/usr/bin/env bash
# Exits 0 when none of the references given is published, and 1 when one is, or when the registry or GitHub does not
# answer that it is absent (RFC0007, requirement 20).
#
#   absent.sh oci HOST/NAME:TAG ...   an image or a chart in an OCI registry that serves anonymous pull tokens, as ghcr.io
#   absent.sh release OWNER/REPO TAG   a GitHub release, read with gh
set -euo pipefail

refuse() {
	echo "absent.sh: $*" >&2
	exit 1
}

oci() {
	local ref=$1
	local host=${ref%%/*} rest=${ref#*/}
	local name=${rest%:*} tag=${rest##*:}
	[[ $host != "$ref" && $name != "$rest" && -n $tag ]] || refuse "$ref is not HOST/NAME:TAG"
	local token code
	token=$(curl -fsS --retry 3 "https://$host/token?scope=repository:$name:pull&service=$host" | jq -er .token) ||
		refuse "$ref: no pull token from $host, so its absence cannot be told"
	code=$(curl -sS --retry 3 -o /dev/null -w '%{http_code}' -I \
		-H "Authorization: Bearer $token" \
		-H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
		"https://$host/v2/$name/manifests/$tag") || code=error
	case $code in
	404) echo "$ref: absent" ;;
	200) refuse "$ref: published already" ;;
	*) refuse "$ref: HTTP $code, so its absence cannot be told" ;;
	esac
}

release() {
	local repo=$1 tag=$2 out
	if out=$(gh api "repos/$repo/releases/tags/$tag" --jq .html_url 2>&1); then
		refuse "release $tag of $repo: published already, $out"
	fi
	[[ $out == *"(HTTP 404)"* ]] || refuse "release $tag of $repo: $out, so its absence cannot be told"
	echo "release $tag of $repo: absent"
}

case ${1:-} in
oci)
	shift
	[[ $# -gt 0 ]] || refuse "usage: absent.sh oci HOST/NAME:TAG ..."
	for ref; do oci "$ref"; done
	;;
release)
	[[ $# -eq 3 ]] || refuse "usage: absent.sh release OWNER/REPO TAG"
	release "$2" "$3"
	;;
*) refuse "usage: absent.sh oci HOST/NAME:TAG ... | absent.sh release OWNER/REPO TAG" ;;
esac
