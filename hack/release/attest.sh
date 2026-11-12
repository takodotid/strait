#!/usr/bin/env bash
# Attests each platform image of the index IMAGE@DIGEST with cosign, keyless, apart from the index: BuildKit's SLSA v1
# provenance and its SPDX SBOM of that image (RFC0007, requirements 23 and 30). INDEX.json is the index as the
# registry serves it for DIGEST. LAYOUT is an extracted OCI layout of a second build of the same commit, with
# --provenance=mode=max,version=v1 and --sbom=true. It must hold each platform image of the index, by the same digest,
# and an attestation manifest for it. The script checks every platform first, and attests nothing unless all pass.
#
#   attest.sh IMAGE@DIGEST INDEX.json LAYOUT
set -euo pipefail

refuse() {
	echo "attest.sh: $*" >&2
	exit 1
}

[[ $# -eq 3 ]] || refuse "usage: attest.sh IMAGE@DIGEST INDEX.json LAYOUT"
source=$1 index=$2 layout=$3
image=${source%@*} digest=${source##*@}
[[ $source == *@* && $digest =~ ^sha256:[0-9a-f]{64}$ ]] || refuse "$source is not IMAGE@sha256:..."
[[ $(sha256sum <"$index" | cut -d' ' -f1) == "${digest#sha256:}" ]] || refuse "$index does not hash to $digest"

# blob prints the path of a blob of the layout, once it hashes to its digest.
blob() {
	[[ $1 =~ ^sha256:[0-9a-f]{64}$ ]] || refuse "the layout names '$1', not sha256:..."
	local f=$layout/blobs/sha256/${1#sha256:}
	[[ -f $f && $(sha256sum <"$f" | cut -d' ' -f1) == "${1#sha256:}" ]] || refuse "the layout lacks the blob $1"
	printf '%s' "$f"
}

# The predicate type of each attestation, as BuildKit writes it in the layer's annotation and the statement, and as
# cosign attest names it.
declare -A types=([https://slsa.dev/provenance/v1]=slsaprovenance1 [https://spdx.dev/Document]=spdxjson)

jq -e '[.manifests[] | select(.annotations["vnd.docker.reference.type"] != null)] | length == 0' "$index" >/dev/null ||
	refuse "$index holds attestations: it must list the platform images only"
mapfile -t images < <(jq -r '.manifests[] | "\(.digest) \(.platform.os)/\(.platform.architecture)"' "$index")
[[ ${#images[@]} -gt 0 ]] || refuse "$index lists no image"
[[ $(jq '.manifests | length' "$layout/index.json") -eq 1 ]] || refuse "the layout's index.json must name one image index"
built=$(blob "$(jq -r '.manifests[0].digest' "$layout/index.json")")

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
for entry in "${images[@]}"; do
	d=${entry% *} platform=${entry#* }
	jq -e --arg d "$d" '[.manifests[] | select(.digest == $d)] | length == 1' "$built" >/dev/null ||
		refuse "the second build has no image $d for $platform: the two builds differ"
	att=$(jq -r --arg d "$d" '[.manifests[] | select(.annotations["vnd.docker.reference.type"] == "attestation-manifest"
		and .annotations["vnd.docker.reference.digest"] == $d) | .digest] | if length == 1 then .[0] else "" end' "$built")
	[[ -n $att ]] || refuse "the second build has no attestation manifest for $platform, $d"
	manifest=$(blob "$att")
	for pt in "${!types[@]}"; do
		layer=$(jq -r --arg pt "$pt" '[.layers[] | select(.annotations["in-toto.io/predicate-type"] == $pt) | .digest]
			| if length == 1 then .[0] else "" end' "$manifest")
		[[ -n $layer ]] || refuse "the attestations of $platform hold no $pt"
		statement=$(blob "$layer")
		jq -e --arg pt "$pt" '.predicateType == $pt and (.predicate | type) == "object"' "$statement" >/dev/null ||
			refuse "the $pt statement of $platform has another predicate type, or no predicate"
		jq .predicate "$statement" >"$work/${d#sha256:}-${types[$pt]}.json"
	done
done

for entry in "${images[@]}"; do
	d=${entry% *} platform=${entry#* }
	for type in slsaprovenance1 spdxjson; do
		cosign attest --yes --type "$type" --predicate "$work/${d#sha256:}-$type.json" "$image@$d"
	done
	echo "attest.sh: $image@$d, $platform: provenance and SBOM attested"
done
