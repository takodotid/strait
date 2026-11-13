# Release

The operator's steps to publish a release. What the workflow checks and in which order: [RFC0007, Releases](../../rfcs/RFC0007.md#releases).

| Step | What to do |
|---|---|
| 1. Merge | Merge the release's pull request into `main`. It names the new version on the pages that name the latest release, [Install](../guide/install.md) and [Upgrade](./upgrade.md): their `v=` lines, and the version in their sentences. The site is built from the tag, so it shows what the tag holds. |
| 2. Wait for CI | Wait until the `ci` run of the merge commit on `main` has finished with every job successful: `gh run list --workflow ci.yaml --branch main`. |
| 3. Tag | Sign an annotated tag on that commit, and push it: `git tag -s vX.Y.Z COMMIT -m "Strait vX.Y.Z"`, then `git push origin vX.Y.Z`. GitHub must show the tag as Verified: the key must belong to the tagger's GitHub account. |
| 4. Approve | When the environment `release` has reviewers, approve the run. Nothing runs until then. |
| 5. Check | Once the run has finished, verify the release as [Verify a release](../guide/install.md#verify-a-release) shows. |
| 6. Publish the site | The run's job `pages` publishes the documentation site from the tag, once the release is out. If only `pages` failed, re-run that job: the release itself is out. To publish the site from `main` without a release, run the workflow `pages` by hand: `gh workflow run pages.yml`. |

## When the release stops

| The job's log says | What to do |
|---|---|
| `gate.sh: release refused: tag ... is not vX.Y.Z` | Tag the commit again with a name `vX.Y.Z`. |
| `is not annotated`, or `does not verify the signature` | Tag again with `git tag -s`, with a key that GitHub has for the tagger's account. |
| `is not on main`, or `points at ..., not at ...` | Tag the merge commit on `main`. |
| `has not finished` | Re-run the whole run once the `ci` run has succeeded: `gh run rerun RUN`. Nothing was pushed. |
| `no ci run of commit ... succeeded` | Fix `main`, and release a later commit. |
| `published already` | That version exists, and the release never replaces it. Release the next patch version. |
| `cannot be told` | GitHub or the registry did not answer. Re-run the failed jobs: `gh run rerun RUN --failed`. |

A tag that the release refused can be deleted and pushed again with the same name, where the repository's tag rules allow it. Otherwise, use the next patch version.

## A run that fails after a push

Only the job `publish` pushes what users see, in this order: the chart, the GitHub release, the image tags. The job `image` pushes the image by its digest only, with no tag.

| `publish` failed | What to do |
|---|---|
| Before its step `Push and sign the chart` | Re-run the failed jobs. No part of the version is visible. |
| In that step or later | Release the next patch version. A re-run stops with `published already`. The chart that was pushed names the image by its digest, so it installs without the image tags. |

A re-run of the whole run stops in `gate` once any part of the version exists.

## Known gaps

What the release does not check, and what to do about it ([RFC0007, Releases](../../rfcs/RFC0007.md#releases)).

| Gap | What to do |
|---|---|
| `gate` runs from the workflow of the tag's commit. A tag on a commit whose `release.yaml` lacks it publishes anyway. | Only people trusted to release may push `v*` tags: a tag ruleset in the repository's settings. |
| GitHub verifies a tag's signature against the keys of the tagger's GitHub account. Any account that may push tags passes the check with its own key. | Keep the right to push tags to the people trusted to release. |
| The `ci` run that `gate` requires builds and tests the image from the same commit, but not the image that the release pushes. | Verify the release as [Verify a release](../guide/install.md#verify-a-release) shows. A rebuild that differs shows there: [Rebuild](rebuild.md). |
| A run that fails after its chart's push cannot go on: a re-run stops with the version published. | Release the next patch version, as [A run that fails after a push](#a-run-that-fails-after-a-push) says. |
| Renovate proposes helm's new version, but not its SHA-256. helm publishes its archives and their SHA-256 on `get.helm.sh`, not as assets of its GitHub releases. The update fails CI at the check of `setup-helm`. | Set the SHA-256 by hand in the same pull request, from `helm-<version>-linux-amd64.tar.gz.sha256sum` on `get.helm.sh`. It comes in a pull request of its own, so it holds back no other update. |
| Renovate proposes bun's new version, but not its SHA-256. The update fails the `pages` build at the check of `setup-bun`. | Set the SHA-256 by hand in the same pull request, from `SHASUMS256.txt` of bun's release, the line of `bun-linux-x64.zip`. |
| Some tools come pinned by others, or with the runner. The `e2e` job's kind and kubectl come from `helm/kind-action`, checked against checksum files of the same release. `actions/setup-go` downloads Go, and `make test` downloads envtest's binaries with `setup-envtest`. docker, buildx, gh, jq and curl come with the runner image. | Review those actions' updates as carefully as the tools' own. |
