#!/usr/bin/env bash
# Packages the chart into DIST, dist by default, as strait-X.Y.Z.tgz with version and appVersion X.Y.Z. Writes DIGEST
# into it, so that it names the Strait image by that digest (RFC0007, requirement 21). Then renders the package, and
# fails unless every Strait image in it is REPOSITORY:X.Y.Z@DIGEST, or REPOSITORY:X.Y.Z without a DIGEST.
#
#   chart.sh vX.Y.Z [DIGEST]
set -euo pipefail

refuse() {
	echo "chart.sh: $*" >&2
	exit 1
}

[[ $# -eq 1 || $# -eq 2 ]] || refuse "usage: chart.sh vX.Y.Z [DIGEST]"
root=$(cd "$(dirname "$0")/../.." && pwd)
dist=${DIST:-$root/dist}
version=${1#v} digest=${2:-}
[[ $# -eq 1 || $digest =~ ^sha256:[0-9a-f]{64}$ ]] || refuse "digest '$digest' is not sha256: and 64 hex digits"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cp -R "$root/deploy/helm/strait" "$work/"
chart=$work/strait/Chart.yaml
line='  strait.tako.id/image-digest: ""'
[[ $(grep -cxF "$line" "$chart") -eq 1 ]] || refuse "Chart.yaml must hold this line once: $line"
sed "s|^$line\$|  strait.tako.id/image-digest: \"$digest\"|" "$chart" >"$chart.new"
mv "$chart.new" "$chart"

mkdir -p "$dist"
helm package "$work/strait" --version "$version" --app-version "$version" -d "$dist"
package=$dist/strait-$version.tgz
repository=$(helm show values "$package" | sed -n '/^image:/,/^[^ ]/s/^  repository: //p')
images=$(helm template strait "$package" --set site.name=a | sed -n "s|^ *image: \\($repository[:@].*\\)|\\1|p" | sort -u)
want=$repository:$version${digest:+@$digest}
[[ $images == "$want" ]] || refuse "the chart names the image as $images, want $want"
echo "chart.sh: $package names $want"
