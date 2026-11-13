# Rebuild a release

Anyone can rebuild a release from its tag and compare it with what the release published. What is reproducible, and why: [RFC0006](../../rfcs/RFC0006.md) requirement 17 for the CLI archives, [RFC0007](../../rfcs/RFC0007.md) requirement 30 for the image.

| Artifact | Same bytes as published |
|---|---|
| CLI archive | Yes. Compare it with the release's `SHA256SUMS`. |
| Image index, from 1.5.0 | Yes, with the annotations that the release wrote on it. Its digest is the one that the chart names. |
| Image of each platform, `linux/amd64` and `linux/arm64` | Yes, with the labels and annotations that the release wrote on it. |
| Image index, before 1.5.0 | No. It holds the build's provenance and SBOM, with the build's times and random identifiers. Compare each platform's image instead. |

v1.4.0 was rebuilt this way, and matched: the archive `strait_v1.4.0_linux_amd64.tar.gz`, and the images `linux/amd64` and `linux/arm64`.

## What it needs

| Need | Why |
|---|---|
| A clone with the release's tag on `HEAD`, and no other change | Go records the commit, and whether the tree changed, in each binary. `make dist` names the version from the tag. A clone without tags builds other bytes. |
| The Go version of the tag's `go.mod` | The release builds with it. |
| docker with buildx, and `jq`, for the image | The release builds the image with the BuildKit image that its `release.yaml` pins. |

## CLI archive

```sh
v=v1.4.0
git clone https://github.com/takodotid/strait && cd strait && git checkout "$v"
make dist GOOS=linux GOARCH=amd64
curl -fsSLO "https://github.com/takodotid/strait/releases/download/$v/SHA256SUMS"
grep "strait_${v}_linux_amd64.tar.gz" SHA256SUMS
sha256sum "dist/strait_${v}_linux_amd64.tar.gz"
```

The two hashes are equal. `make dist` takes `SOURCE_DATE_EPOCH` from the commit's time.

## Image

In the same clone:

```sh
image=ghcr.io/takodotid/strait
buildkit=$(grep -o 'image=moby/buildkit[^ ]*' .github/workflows/release.yaml | cut -d= -f2)
docker buildx create --name strait-rebuild --driver docker-container --driver-opt "image=$buildkit"

# The published index, and the labels and annotations that the release wrote on it and on its platform images.
docker buildx imagetools inspect "$image:${v#v}" --raw >published.json
amd64=$(jq -r '.manifests[] | select(.platform.architecture == "amd64") | .digest' published.json)
args=()
while IFS= read -r kv; do args+=(--annotation "index:$kv"); done < <(
  jq -r '.annotations | to_entries[] | "\(.key)=\(.value)"' published.json)
while IFS= read -r kv; do args+=(--label "$kv" --annotation "manifest:$kv"); done < <(
  docker buildx imagetools inspect "$image@$amd64" --raw | jq -r '.annotations | to_entries[] | "\(.key)=\(.value)"')

docker buildx build --builder strait-rebuild --platform linux/amd64,linux/arm64 \
  --build-arg VERSION="$v" --build-arg COMMIT="$(git rev-parse HEAD)" \
  --build-arg SOURCE_DATE_EPOCH="$(git log -1 --format=%ct)" "${args[@]}" \
  --provenance=false --sbom=false --no-cache \
  --output "type=oci,dest=rebuild.tar,rewrite-timestamp=true" .

mkdir rebuild && tar -xf rebuild.tar -C rebuild
echo "published sha256:$(sha256sum <published.json | cut -d' ' -f1)"
echo "rebuilt   $(jq -r '.manifests[0].digest' rebuild/index.json)"
docker buildx rm strait-rebuild
```

From 1.5.0, the two digests are equal. The labels and annotations hold the release's time, `org.opencontainers.image.created`, so the build takes them from the published image. `--provenance=false --sbom=false` leaves out the attestations, as the release's index does. The release attests each platform image separately from the index ([Verify a release](../../README.md#verify-a-release)).

Before 1.5.0, compare each platform's digest:

```sh
jq -r '.manifests[] | select(.platform.os == "linux") | "\(.platform.architecture) \(.digest)"' published.json
jq -r '.manifests[0].digest' rebuild/index.json | cut -d: -f2 |
  xargs -I{} jq -r '.manifests[] | "\(.platform.architecture) \(.digest)"' rebuild/blobs/sha256/{}
```

A platform image's binaries can also be checked with `hack/distcheck`: [RFC0007, Releases](../../rfcs/RFC0007.md#releases), job `image-check`.
