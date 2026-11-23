# Rebuild a release

Anyone can rebuild a release from its tag and compare it with what the release published. What is reproducible: [RFC0006](../../rfcs/RFC0006.md) requirement 17 for the CLI archives, [RFC0007](../../rfcs/RFC0007.md) requirement 30 for the image.

## Steps

Check [What it needs](#what-it-needs) first. Run every block in the same shell and clone: the later blocks use the variable `v` and the files of the earlier ones. The examples rebuild v1.4.0. To rebuild another release, change `v` in the first line of step 1.

### 1. Rebuild the CLI archive

Clone the repository at the tag, build the archive, and print both hashes.

```sh
v=v1.4.0
git clone https://github.com/takodotid/strait && cd strait && git checkout "$v"
make dist GOOS=linux GOARCH=amd64
curl -fsSLO "https://github.com/takodotid/strait/releases/download/$v/SHA256SUMS"
grep "strait_${v}_linux_amd64.tar.gz" SHA256SUMS
sha256sum "dist/strait_${v}_linux_amd64.tar.gz"
```

Check: the two hashes are equal. `make dist` takes `SOURCE_DATE_EPOCH` from the commit's time.

### 2. Rebuild the image

Run this in the same shell and clone as step 1, so that `v` is set. It reads the published index, then builds the image from the tag with the published labels and annotations.

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
```

Then build, in the same shell:

```sh
docker buildx build --builder strait-rebuild --platform linux/amd64,linux/arm64 \
  --build-arg VERSION="$v" --build-arg COMMIT="$(git rev-parse HEAD)" \
  --build-arg SOURCE_DATE_EPOCH="$(git log -1 --format=%ct)" "${args[@]}" \
  --provenance=false --sbom=false --no-cache \
  --output "type=oci,dest=rebuild.tar,rewrite-timestamp=true" .

mkdir rebuild && tar -xf rebuild.tar -C rebuild
```

The labels and annotations hold the release's time, so the build takes them from the published image. `--provenance=false --sbom=false` leaves out the attestations, as the release's index does.

### 3. Compare the digests

For 1.5.0 and later, the two digests are equal:

```sh
echo "published sha256:$(sha256sum <published.json | cut -d' ' -f1)"
echo "rebuilt   $(jq -r '.manifests[0].digest' rebuild/index.json)"
```

For versions before 1.5.0, the published index holds the build's provenance and SBOM, with the build's times and random identifiers, so its digest differs. Compare each platform's digest instead; the two lists are equal:

```sh
jq -r '.manifests[] | select(.platform.os == "linux") | "\(.platform.architecture) \(.digest)"' published.json
jq -r '.manifests[0].digest' rebuild/index.json | cut -d: -f2 |
  xargs -I{} jq -r '.manifests[] | "\(.platform.architecture) \(.digest)"' rebuild/blobs/sha256/{}
```

### 4. Remove the builder

```sh
docker buildx rm strait-rebuild
```

## Checking the binaries

A platform image's binaries can also be checked with `hack/distcheck`: [RFC0007, Releases](../../rfcs/RFC0007.md#releases), job `image-check`.

## What it needs

- A clone with the release's tag on `HEAD`, and no other change.
- The Go version of the tag's `go.mod`.
- docker with buildx, and `jq`, for the image.
