# Install

## What a release publishes

| Artifact | Where |
|---|---|
| Helm chart | `oci://ghcr.io/takodotid/charts/strait` |
| Image, linux/amd64 and linux/arm64 | `ghcr.io/takodotid/strait` |
| CLI archives for Linux, macOS and Windows, amd64 and arm64, with `SHA256SUMS` | [GitHub releases](https://github.com/takodotid/strait/releases) |
| The CRDs, in one file: `strait-crds.yaml` | [GitHub releases](https://github.com/takodotid/strait/releases) |

The commands on this page use v1.5.0, the latest release when this page was written. Use the latest version on the [releases page](https://github.com/takodotid/strait/releases).

## Requirements

Each site needs Kubernetes 1.31 or later. The CRDs' validation rules use the CEL functions `isIP` and `isCIDR`, and the API server of 1.30 or earlier refuses these CRDs. `make test` runs the CRDs' rules on the oldest and the latest version, which the `Makefile`'s `ENVTEST_K8S_VERSIONS` names.

## The chart

Install the chart at each site, in a namespace with the Pod Security level `privileged`, with the values of [Getting started](../getting-started.md#2-install-the-chart):

```sh
helm --kube-context site-a install strait oci://ghcr.io/takodotid/charts/strait --version 1.5.0 \
  --namespace strait-system --values values-a.yaml --wait
```

| Before | Read |
|---|---|
| Setting up two clusters for the first time | [Getting started](../getting-started.md): from the WireGuard key to a routed name. |
| A site serves production traffic | [Before production](../runbooks/before-production.md). |
| Choosing the controller's memory | [Sizing the controller](../sizing.md): it grows with the number of Services in the cluster. |
| Changing a value | [`values.yaml`](../../deploy/helm/strait/values.yaml), which holds every value of the chart. |

### Which image the chart runs

| Value | Effect on the image |
|---|---|
| `image.tag` and `image.digest` empty | The image that the chart's release pushed: `ghcr.io/takodotid/strait:X.Y.Z@sha256:...`. |
| `image.tag` set | That tag, without a digest, as before 1.3.0. |
| `image.digest` set | That digest. |
| `image.repository` set, for a mirror | The mirror, with the release's digest while `image.tag` and `image.digest` are empty. Copy the image with its digest, for example with `crane copy` or `skopeo copy --all`. |

## The CLI

On Linux, here on amd64:

```sh
v=v1.5.0
curl -fsSLO "https://github.com/takodotid/strait/releases/download/$v/strait_${v}_linux_amd64.tar.gz"
curl -fsSLO "https://github.com/takodotid/strait/releases/download/$v/SHA256SUMS"
sha256sum --ignore-missing -c SHA256SUMS
tar -xzf "strait_${v}_linux_amd64.tar.gz"
sudo install "strait_${v}_linux_amd64/strait" "strait_${v}_linux_amd64/kubectl-strait" /usr/local/bin/
```

The archive also holds the man pages in `man/man1/` and the skill document `SKILL.md`. Shell completion is in [The strait command](../reference/cli.md#shell-completion).

### On Windows

The Windows archive is a `.zip` with `strait.exe` and `kubectl-strait.exe`. Install it in PowerShell, here on amd64:

```powershell
$v = "v1.5.0"
$a = "strait_${v}_windows_amd64"
$url = "https://github.com/takodotid/strait/releases/download/$v"
Invoke-WebRequest "$url/$a.zip" -OutFile "$a.zip" -UseBasicParsing
Invoke-WebRequest "$url/SHA256SUMS" -OutFile SHA256SUMS -UseBasicParsing
$sum = (Get-Content SHA256SUMS | Where-Object { $_.EndsWith("  $a.zip") }).Split(" ")[0]
if ((Get-FileHash "$a.zip" -Algorithm SHA256).Hash -ne $sum) { throw "$a.zip does not match SHA256SUMS" }
Expand-Archive "$a.zip" -DestinationPath .
$bin = "$env:LOCALAPPDATA\Programs\strait"
New-Item -ItemType Directory -Force $bin | Out-Null
Copy-Item "$a\strait.exe", "$a\kubectl-strait.exe" $bin
```

Add that folder to `Path` once, then open a new terminal. kubectl finds `kubectl-strait.exe` there, so `kubectl strait` runs the same commands. Flags go after `strait`:

```powershell
strait version
kubectl plugin list
kubectl strait --context site-a status
```

How the CLI differs on Windows is in [The strait command](../reference/cli.md#on-windows).

## Verify a release

From 1.3.0, each release signs the image and the chart with Sigstore. The signing is keyless and uses the identity of the release's workflow. It attests the CLI archives, the chart archive, `strait-crds.yaml` and `SHA256SUMS` with GitHub artifact attestations. The chart records the digest of the image's index in the annotation `strait.tako.id/image-digest` ([RFC0007, Releases](../../rfcs/RFC0007.md#releases)). From 1.5.0, the index lists only the platform images, `linux/amd64` and `linux/arm64`. Each platform image carries its build's provenance (SLSA v1) and its SBOM (SPDX) as cosign attestations under the same identity.

Verify the chart and its image with [cosign](https://docs.sigstore.dev/cosign/verifying/verify/) 3, and an archive with the GitHub CLI's `gh attestation verify`:

```sh
v=v1.5.0
id="https://github.com/takodotid/strait/.github/workflows/release.yaml@refs/tags/$v"
issuer=https://token.actions.githubusercontent.com
cosign verify "ghcr.io/takodotid/charts/strait:${v#v}" --certificate-identity "$id" --certificate-oidc-issuer "$issuer"
digest=$(helm show chart oci://ghcr.io/takodotid/charts/strait --version "${v#v}" | sed -n 's|^  strait.tako.id/image-digest: ||p')
cosign verify "ghcr.io/takodotid/strait@$digest" --certificate-identity "$id" --certificate-oidc-issuer "$issuer"
gh attestation verify "strait_${v}_linux_amd64.tar.gz" --repo takodotid/strait --cert-identity "$id"
gh attestation verify SHA256SUMS --repo takodotid/strait --cert-identity "$id"
```

From 1.5.0, verify each platform image's provenance and SBOM. This needs `docker` and `jq` too. The platform images come from the index that the chart names:

```sh
for image in $(docker buildx imagetools inspect "ghcr.io/takodotid/strait@$digest" --raw | jq -r '.manifests[].digest'); do
  cosign verify-attestation "ghcr.io/takodotid/strait@$image" --type slsaprovenance1 --certificate-identity "$id" --certificate-oidc-issuer "$issuer" >"provenance-${image#sha256:}.json"
  cosign verify-attestation "ghcr.io/takodotid/strait@$image" --type spdxjson --certificate-identity "$id" --certificate-oidc-issuer "$issuer" >"sbom-${image#sha256:}.json"
done
```

Each file holds the attestation as a DSSE envelope. `jq '.payload | @base64d | fromjson | .predicate' FILE` prints the provenance or the SBOM.

To rebuild a CLI archive or the image from the release's tag and compare it with what the release published, see [Rebuild a release](../runbooks/rebuild.md).

## Upgrade

Upgrade one site at a time: see [Upgrade](../runbooks/upgrade.md).
