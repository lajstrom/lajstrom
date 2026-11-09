# Releasing Lajstrom

A pushed tag `vX.Y.Z` builds and publishes a release without anyone's laptop:
[`.github/workflows/release.yml`](../.github/workflows/release.yml) runs goreleaser with
[`.goreleaser.yaml`](../.goreleaser.yaml). Installing a release is described in
`docs/install.md` (#58).

## What a release contains

| Artifact | Where |
| --- | --- |
| `lajstrom_<version>_<os>_<arch>.tar.gz` for linux and darwin, amd64 and arm64: the static binary (`CGO_ENABLED=0`), `LICENSE`, `NOTICE`, `README.md` | the GitHub release |
| `<archive>.sbom.json`, an SPDX SBOM of each archive (syft) | the GitHub release |
| `checksums.txt` (SHA-256 of the archives and SBOMs) and `checksums.txt.sigstore.json`, its cosign bundle | the GitHub release |
| the image, linux/amd64 and linux/arm64 in one index, distroless static, user 65532, entrypoint `/lajstrom`, with an SBOM attestation and a cosign signature | `ghcr.io/lajstrom/lajstrom:vX.Y.Z` and `docker.io/lajstrom/lajstrom:vX.Y.Z`; `:latest` too on a stable version |
| the Homebrew cask `lajstrom` | `lajstrom/homebrew-tap`: `brew install lajstrom/tap/lajstrom` |

The binary reports the tag: `lajstrom version` and `GET /-/version` print `vX.Y.Z`, the short
commit and the build time, the same build information `make build` sets. The signatures are
keyless: cosign gets a short-lived certificate for the identity of the release workflow at that
tag from Fulcio, through the job's GitHub OIDC token, and logs the signature in Rekor. There is
no signing key to keep.

## Cutting a release

1. `main` is green, and the release candidate passed the e2e suite and the quickstarts.
2. Tag the commit and push the tag; a version with a suffix is a pre-release:

   ```sh
   git switch main && git pull --ff-only
   git tag -a v0.1.0-rc.1 -m v0.1.0-rc.1
   git push origin v0.1.0-rc.1
   ```

3. The `release` job of the Release workflow builds and pushes everything. It creates the GitHub
   release as a draft, with notes grouped from the Conventional Commits since the previous tag.
4. Edit the notes, then publish the draft. The archive URLs of the cask, and so
   `brew install`, work once the release is published. The images are public as soon as they are
   pushed.
5. Verify the release as a user would ([below](#verifying-a-release)).

A run that failed can be re-run from the Actions tab: it replaces the draft. A tag that was
published stays; fix forward with the next version.

### Secrets and one-time setup

| Secret | Used for | Without it |
| --- | --- | --- |
| `GITHUB_TOKEN` (automatic) | the release and its assets, `ghcr.io/lajstrom/lajstrom` | |
| `DOCKERHUB_USERNAME`, `DOCKERHUB_TOKEN` | a Docker Hub access token with read and write on `lajstrom/lajstrom` | the images go to ghcr.io only; the job warns |
| `HOMEBREW_TAP_TOKEN` | a fine-grained token with contents read and write on `lajstrom/homebrew-tap` | the cask is not pushed; the job warns |

After the first release, check that the `lajstrom` package on ghcr.io is public (a new package
can start private) and linked to this repository.

## Verifying a release

```sh
TAG=v0.1.0
ID="https://github.com/lajstrom/lajstrom/.github/workflows/release.yml@refs/tags/${TAG}"
ISSUER=https://token.actions.githubusercontent.com

# the archives: the signature of checksums.txt, then the checksums
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity "$ID" --certificate-oidc-issuer "$ISSUER"
sha256sum --ignore-missing -c checksums.txt

# the image, and its SBOM
cosign verify "ghcr.io/lajstrom/lajstrom:${TAG}" \
  --certificate-identity "$ID" --certificate-oidc-issuer "$ISSUER"
docker buildx imagetools inspect "ghcr.io/lajstrom/lajstrom:${TAG}" --format '{{ json .SBOM }}'
docker run --rm "ghcr.io/lajstrom/lajstrom:${TAG}" version
```

## Trying the release locally

`make release-check` validates `.goreleaser.yaml`; `make snapshot` runs the whole release
without publishing or signing. Both run goreleaser from its image, at the version the workflow
uses, and need only Docker. The snapshot writes the archives, SBOMs, `checksums.txt` and the
cask to `dist/` and loads one image per platform into the local Docker:

```sh
make snapshot
version=$(jq -r .version dist/metadata.json)   # 0.0.2-snapshot.6b52bd4
docker run --rm "ghcr.io/lajstrom/lajstrom:v${version}-arm64" version
```

The Release workflow runs the same snapshot on every push to `main` and on a pull request that
changes the release configuration, and keeps `dist/` as the `lajstrom-snapshot` artifact.

The `Dockerfile` copies a prebuilt binary from `linux/<arch>/lajstrom` in the build context, the
layout goreleaser gives it. To build the image by hand from a snapshot:

```sh
ctx=$(mktemp -d) && mkdir -p "$ctx/linux/arm64"
cp dist/lajstrom_linux_arm64_v8.0/lajstrom "$ctx/linux/arm64/"
docker build -t lajstrom:local -f Dockerfile "$ctx"
docker run --rm lajstrom:local version
```
