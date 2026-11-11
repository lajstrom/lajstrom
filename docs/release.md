# Releasing Lajstrom

A pushed tag `vX.Y.Z` builds and publishes a release without anyone's laptop:
[`.github/workflows/release.yml`](../.github/workflows/release.yml) runs goreleaser with
[`.goreleaser.yaml`](../.goreleaser.yaml). Installing a release is described in
[install.md](install.md).

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

A tag with a suffix is a pre-release. It differs from a stable tag in three places:

| | `v0.1.0-rc.1` | `v0.1.0` |
| --- | --- | --- |
| GitHub release | a draft, marked as a pre-release | a draft; "Latest" once published |
| image tags | `v0.1.0-rc.1` | `v0.1.0` and `latest` |
| notes | the commits since the nearest tag before it | the commits since the previous stable tag, `v0.0.1` |

The cask is pushed for both: once the draft of an rc is published, `brew install` gets the rc.

## The workflow

On a `v*` tag, the Release workflow runs two jobs:

1. `preflight` checks the secrets that are set: the Docker Hub user and token can push
   `lajstrom/lajstrom`, and the tap token can write to `lajstrom/homebrew-tap`. A secret that is
   not set is a warning: that target is skipped. A secret that does not work fails the job, and
   nothing is published.
2. `release` checks the Go that builds the binaries, setup-go's `stable`: it must be the latest
   patch release of its minor version on [go.dev/dl](https://go.dev/dl/), and `govulncheck ./...`
   must find nothing. A new Go patch release that setup-go does not have yet fails the job:
   rerun it later. Then it runs `goreleaser release`. That builds, signs `checksums.txt`, pushes
   and signs the image, creates the draft release with its assets, and pushes the cask to the
   tap last.

**Run workflow** on the Actions tab (`workflow_dispatch`) runs `preflight` alone, to check the
secrets before a tag is pushed.

| Secret of `lajstrom/lajstrom` | What it is | Without it |
| --- | --- | --- |
| `GITHUB_TOKEN` (automatic) | the release and its assets, `ghcr.io/lajstrom/lajstrom`, the OIDC token for cosign | |
| `DOCKERHUB_USERNAME` | `lajstrom`, the Docker Hub account that owns the namespace | no image on Docker Hub; a warning |
| `DOCKERHUB_TOKEN` | a personal access token of that account, **Read & Write** | |
| `HOMEBREW_TAP_TOKEN` | a fine-grained token, **Contents: Read and write** on `lajstrom/homebrew-tap` only | the cask is not pushed; a warning |

## Before the first release

Once. What exists on 2026-11-11 is marked; the preflight checks the rest.

1. **Docker Hub.** `lajstrom` is a Docker Hub *user* account (it exists), not an organization,
   so its own token pushes `docker.io/lajstrom/lajstrom`.
   - Signed in as `lajstrom` on [hub.docker.com](https://hub.docker.com): **Create a
     repository**, name `lajstrom`, visibility **Public**. A first push would create it too, with
     the account's default visibility; created by hand it is public for sure.
   - **Account settings → Personal access tokens → Generate new token**: description
     `github lajstrom/lajstrom release`, access permissions **Read & Write**. Note its expiry
     date.
   - Repository secrets (both are set since 2026-11-09; set them again if the token is new):

     ```sh
     gh secret set DOCKERHUB_USERNAME --repo lajstrom/lajstrom --body lajstrom
     gh secret set DOCKERHUB_TOKEN --repo lajstrom/lajstrom    # prompts for the token
     ```

2. **The Homebrew tap.** `lajstrom/homebrew-tap` exists: public, a README on `main`, no branch
   protection. goreleaser commits `Casks/lajstrom.rb` to `main` directly, so `main` must not
   require pull requests.
   - [New fine-grained token](https://github.com/settings/personal-access-tokens/new): resource
     owner `lajstrom`; repository access **Only select repositories**, `lajstrom/homebrew-tap`;
     repository permissions **Contents: Read and write** (Metadata: Read comes with it). If the
     organization approves fine-grained tokens, approve it under **Organization settings →
     Personal access tokens → Pending requests**.
   - `gh secret set HOMEBREW_TAP_TOKEN --repo lajstrom/lajstrom` (set since 2026-11-09).

3. **ghcr.io.** Nothing to create: the first release job creates the package
   `ghcr.io/lajstrom/lajstrom` with its `GITHUB_TOKEN`, linked to this repository. A new
   package can start private; check it after the first run ([below](#cutting-a-release),
   step 4). The organization must allow public packages: **Organization settings → Packages →
   Package creation → Public** (the default).

4. **Check.** Run the preflight on `main`:

   ```sh
   gh workflow run release.yml --repo lajstrom/lajstrom --ref main
   gh run watch --repo lajstrom/lajstrom \
     "$(gh run list --repo lajstrom/lajstrom --workflow release.yml --event workflow_dispatch --limit 1 --json databaseId --jq '.[0].databaseId')"
   ```

   It is green, without warnings, and its log has
   `Docker Hub grants lajstrom on lajstrom/lajstrom: pull,push` (in either order) and
   `HOMEBREW_TAP_TOKEN can write to lajstrom/homebrew-tap`.

## Cutting a release

1. `main` is green. For a stable version, the release candidate passed the e2e suite and the
   quickstarts, `CHANGELOG.md` has the version and its date instead of `[Unreleased]`, and
   `docs/releases/vX.Y.Z.md` is final.
2. Tag the commit and push the tag:

   ```sh
   TAG=v0.1.0-rc.1
   git switch main && git pull --ff-only
   git tag -a "$TAG" -m "$TAG"
   git push origin "$TAG"
   gh run watch --repo lajstrom/lajstrom \
     "$(gh run list --repo lajstrom/lajstrom --workflow release.yml --branch "$TAG" --limit 1 --json databaseId --jq '.[0].databaseId')"
   ```

   The run takes about ten minutes.
3. Read the draft: `gh release view "$TAG" --repo lajstrom/lajstrom --web`. It has ten assets:
   four archives, four SBOMs, `checksums.txt` and `checksums.txt.sigstore.json`. For a stable
   version, put `docs/releases/vX.Y.Z.md` above the generated list of commits.
4. The first release only: check that the image is public.
   `gh api orgs/lajstrom/packages/container/lajstrom --jq .visibility` prints `public`. If it
   prints `private`, open
   [the package's settings](https://github.com/orgs/lajstrom/packages/container/lajstrom/settings)
   and choose **Danger Zone → Change visibility → Public**. While the package is private,
   `docker pull` and `cosign verify` without a login get `denied`. A Docker that is logged in to
   ghcr.io does not show this.
5. Publish the draft: `gh release edit "$TAG" --repo lajstrom/lajstrom --draft=false`. The
   archive URLs, and so the cask and `brew install`, work from then on; the images are public
   as soon as they are pushed (after step 4 on the first release).
6. Verify the release as a user would ([below](#verifying-a-release)).

**If a run fails.** The preflight failed: nothing is published; fix the secret and
`gh run rerun <run id> --repo lajstrom/lajstrom`. The release job failed: rerun it; goreleaser
replaces the draft and pushes the images again under the same tags, with a new digest that it
signs. Once a release is published, its tag stays: fix forward with the next version
(`v0.1.0-rc.2`).

### Migrations in the notes

Operators upgrade with a rolling update, so the notes say what the release does to the index
database ([index-schema.md](spec/index-schema.md#upgrades)). The new migrations are
`git diff --name-only <previous tag> -- internal/index/migrations/postgres`, and the contract
ones carry the line `-- lajstrom:requires NNNN`. Under a heading **Upgrade**, the notes of a
release with migrations list each one, `NNNN_<name>`, with what it changes and whether it is
expand or contract. Without a contract migration, one line: a rolling update from the release
before, and a rollback to it, work. For each contract migration, the notes also say:

- what it removes, and which release it requires: the first release that has migration NNNN.
  An upgrade from an older release is refused by `serve` and `lajstrom migrate`; upgrade
  through the required release, or stop every replica and run `lajstrom migrate
  --allow-breaking`.
- that a rollback goes no further back than the required release; before it, it is a restore of
  the backup ([operations.md](operations.md#upgrades)).
- how long it ran on the largest index you have measured, when it rewrites rows, and which
  tables it locks while the old replicas serve.

## Verifying a release

With cosign 3 (`brew install cosign`), Docker and Homebrew, on macOS arm64:

```sh
TAG=v0.1.0-rc.1 VERSION=${TAG#v}
ID="https://github.com/lajstrom/lajstrom/.github/workflows/release.yml@refs/tags/${TAG}"
ISSUER=https://token.actions.githubusercontent.com
cd "$(mktemp -d)"

# the archives: the signature of checksums.txt, then the checksums
for f in checksums.txt checksums.txt.sigstore.json "lajstrom_${VERSION}_darwin_arm64.tar.gz"; do
  curl -fsSLO "https://github.com/lajstrom/lajstrom/releases/download/${TAG}/${f}"
done
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity "$ID" --certificate-oidc-issuer "$ISSUER"   # Verified OK
shasum -a 256 --ignore-missing -c checksums.txt                      # sha256sum on Linux
tar -xzf "lajstrom_${VERSION}_darwin_arm64.tar.gz" lajstrom && ./lajstrom version

# the images, on both registries, and the SBOM attestation
for image in ghcr.io/lajstrom/lajstrom docker.io/lajstrom/lajstrom; do
  cosign verify "${image}:${TAG}" \
    --certificate-identity "$ID" --certificate-oidc-issuer "$ISSUER" > /dev/null
  docker run --rm "${image}:${TAG}" version
done
docker buildx imagetools inspect "ghcr.io/lajstrom/lajstrom:${TAG}"   # linux/amd64, linux/arm64
docker buildx imagetools inspect "ghcr.io/lajstrom/lajstrom:${TAG}" --format '{{ json .SBOM }}' | head -c 300
docker buildx imagetools inspect ghcr.io/lajstrom/lajstrom:latest     # an rc: not found before v0.1.0

# the cask
brew install lajstrom/tap/lajstrom
lajstrom version
brew uninstall --cask lajstrom
```

Each `version` prints `lajstrom v0.1.0-rc.1 (<commit>, <build time>)`. The two `cosign verify`
lines print `The following checks were performed …` and fail on a wrong identity. On a stable
version, `latest` names the same digest as the tag.

## Trying the release locally

`make release-check` validates `.goreleaser.yaml`; `make snapshot` runs the whole release
without publishing or signing. Both run goreleaser from its image, at the version the workflow
uses, and need only Docker. The Go is the image's own (go1.27.3 in goreleaser v2.20.0), not the
release's: `go version dist/lajstrom_linux_amd64_v1/lajstrom` says which. The snapshot writes
the archives, SBOMs, `checksums.txt` and the cask to `dist/` and loads one image per platform
into the local Docker:

```sh
make snapshot
version=$(jq -r .version dist/metadata.json)   # 0.0.2-snapshot.6b52bd4
docker run --rm "ghcr.io/lajstrom/lajstrom:v${version}-arm64" version
```

The image names of a snapshot are the same for every snapshot of one commit: another snapshot
of that commit, in another worktree, replaces them.

The Release workflow runs the same snapshot on every push to `main` and on a pull request that
changes the release configuration. It runs the linux/amd64 binary and image. It checks that
the binary was built by the Go of setup-go, and that govulncheck finds no vulnerable function
of the standard library in the binary. It keeps `dist/` as the `lajstrom-snapshot` artifact
for 14 days:

```sh
gh run download --repo lajstrom/lajstrom --name lajstrom-snapshot \
  "$(gh run list --repo lajstrom/lajstrom --workflow release.yml --branch main --status success --limit 1 --json databaseId --jq '.[0].databaseId')"
```

The `Dockerfile` copies a prebuilt binary from `linux/<arch>/lajstrom` in the build context, the
layout goreleaser gives it. To build the image by hand from a snapshot:

```sh
ctx=$(mktemp -d) && mkdir -p "$ctx/linux/arm64"
cp dist/lajstrom_linux_arm64_v8.0/lajstrom "$ctx/linux/arm64/"
docker build -t lajstrom:local -f Dockerfile "$ctx"
docker run --rm lajstrom:local version
```
