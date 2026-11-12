# Install

Lajstrom is one static binary, `lajstrom`, for Linux and macOS on amd64 and arm64. A release
(#57) publishes it four ways: a container image, an archive per platform, a Homebrew cask, and
the source. All of them are the same build: `lajstrom version` and `GET /-/version` print the
tag, the commit and the build time.

**No release has been published yet.** The commands below are for `v0.1.0` and later; what
could be tried before the first release, with the snapshot that `make snapshot` builds from the
release configuration, is marked, and so is what could not.

| Artifact | Where | Verify with |
| --- | --- | --- |
| image `linux/amd64` and `linux/arm64`, distroless, user 65532 | `ghcr.io/lajstrom/lajstrom:vX.Y.Z`, `docker.io/lajstrom/lajstrom:vX.Y.Z` | `cosign verify` |
| `lajstrom_<version>_<os>_<arch>.tar.gz`, `<os>` `linux` or `darwin`, `<arch>` `amd64` or `arm64` | the [GitHub release](https://github.com/lajstrom/lajstrom/releases) | `checksums.txt`, signed: `cosign verify-blob`, then `sha256sum -c` |
| an SPDX SBOM per archive, `<archive>.sbom.json`; the image carries its own | the release; the image's attestation | the same `checksums.txt` |
| the cask `lajstrom` | the tap `lajstrom/homebrew-tap` | Homebrew checks the archive's sha256 |

Signatures are keyless: the release workflow signs with a short-lived certificate for its own
GitHub identity, logged in Rekor, so there is no public key to fetch. What `cosign` checks is
that identity: the workflow `release.yml` of `lajstrom/lajstrom` at the tag. Verify with
cosign 3 (`brew install cosign`): the signatures are in the bundle format of cosign 3, which
cosign 2 does not read by default.

## Container image

```sh
TAG=v0.1.0
docker pull ghcr.io/lajstrom/lajstrom:$TAG
cosign verify ghcr.io/lajstrom/lajstrom:$TAG \
  --certificate-identity "https://github.com/lajstrom/lajstrom/.github/workflows/release.yml@refs/tags/$TAG" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
docker run --rm ghcr.io/lajstrom/lajstrom:$TAG version
```

With the snapshot image (`cosign verify` cannot be tried on it: a snapshot is not signed):

```text
$ docker run --rm ghcr.io/lajstrom/lajstrom:v0.0.2-snapshot.aa808b3-arm64 version
lajstrom v0.0.1-138-gaa808b3 (aa808b3, 2026-11-09T17:15:23Z)
```

The image runs `/lajstrom serve` as the user 65532 (`nonroot`) in `/home/nonroot`, and has no
shell. Without a configuration it stops at once:

```text
$ docker run --rm ghcr.io/lajstrom/lajstrom:v0.0.2-snapshot.aa808b3-arm64
lajstrom: config: open lajstrom.yaml: no such file or directory (choose the file with --config or LAJSTROM_CONFIG)
```

Mount one and name it; the smallest that starts is `server.listen` and the URL clients use,
here the dev stack's `http://localhost:8080` with `server.insecureHTTP` (SQLite in the
container, no registry: a first look, not a deployment; a deployment's `externalURL` is
`https://<host>`):

```sh
printf 'server:\n  listen: ":8080"\n  externalURL: "http://localhost:8080"\n  insecureHTTP: true\n' > lajstrom.yaml
docker run -d --name lajstrom -p 127.0.0.1:8080:8080 \
  -v "$PWD/lajstrom.yaml:/etc/lajstrom/lajstrom.yaml:ro" \
  ghcr.io/lajstrom/lajstrom:$TAG serve --config /etc/lajstrom/lajstrom.yaml
curl -s localhost:8080/-/readyz     # ok
```

A deployment has Postgres, a registry and the master key besides: [Docker
Compose](deploy/compose.md) and [Kubernetes](deploy/kubernetes.md) show both shapes.

## Binary archive

Download the archive of your platform, `checksums.txt` and its bundle, verify the signature of
`checksums.txt`, then the archive against it:

```sh
TAG=v0.1.0 VERSION=${TAG#v} OS=linux ARCH=amd64     # OS: linux, darwin; ARCH: amd64, arm64
base=https://github.com/lajstrom/lajstrom/releases/download/$TAG
curl -fsSLO "$base/lajstrom_${VERSION}_${OS}_${ARCH}.tar.gz"
curl -fsSLO "$base/checksums.txt"
curl -fsSLO "$base/checksums.txt.sigstore.json"

cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/lajstrom/lajstrom/.github/workflows/release.yml@refs/tags/$TAG" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --ignore-missing -c checksums.txt          # or, on macOS: shasum -a 256 --ignore-missing -c checksums.txt
tar -xzf "lajstrom_${VERSION}_${OS}_${ARCH}.tar.gz"  # lajstrom, LICENSE, NOTICE, README.md, no directory
sudo install -m 0755 lajstrom /usr/local/bin/lajstrom
lajstrom version
```

The checksum, extraction and install steps were run on the snapshot's archives, in
`ubuntu:24.04` for `linux_arm64` and on macOS 27.0.1 for `darwin_arm64` (both `sha256sum` and
`shasum`):

```text
lajstrom_0.0.2-snapshot.aa808b3_linux_arm64.tar.gz: OK
lajstrom v0.0.1-138-gaa808b3 (aa808b3, 2026-11-09T17:15:23Z)
```

`cosign verify-blob` and the download were not: there is no release to download, and a
snapshot is not signed. The binaries are not notarized for macOS. `curl` does not mark what it
downloads, so the binary runs; an archive downloaded with a browser is quarantined, and
`xattr -d com.apple.quarantine lajstrom` lets it run.

## Homebrew

```sh
brew install lajstrom/tap/lajstrom
lajstrom version
```

A cask, for macOS (it lists the Linux archives too); it removes the quarantine attribute after
Homebrew has checked the archive's sha256. Since Homebrew 6, a cask from a tap other than
Homebrew's own needs trust. Installing it by its full name, as above, trusts this one cask.
To install it as `brew install lajstrom`, run `brew trust --cask lajstrom/tap/lajstrom` first
([Tap Trust](https://docs.brew.sh/Tap-Trust)).

The tap gets its cask at the first release. So the snapshot's cask was installed instead, on
2026-11-11, on macOS 27.0.1 arm64 with Homebrew 7.0.12. It came from a local tap, with its
URLs changed to `file://` URLs of the snapshot's archives, because the release URLs work only
once a release is published:

```text
$ brew install lajstromreleasetest/tap/lajstrom
==> Fetching downloads for: lajstromreleasetest/tap/lajstrom
✔︎ Cask lajstrom (0.0.2-snapshot.533acf3)
==> Installing Cask lajstrom
==> Linking Binary 'lajstrom' to '/opt/homebrew/bin/lajstrom'
$ lajstrom version
lajstrom v0.0.1-357-g533acf3 (533acf3, 2026-11-11T07:23:38Z)
$ brew uninstall --cask lajstrom
```

After the install, the binary had no extended attributes. That run cannot show whether the
step that removes the quarantine attribute worked, because a `file://` download may never have
had the attribute. [release.md](release.md#verifying-a-release) installs the published cask.

## From source

Go 1.27 or later (from [go.dev/dl](https://go.dev/dl/): a distribution's package is often
older), `git` and `make`:

```sh
git clone https://github.com/lajstrom/lajstrom.git
cd lajstrom
git checkout v0.1.0       # or stay on main
make build                # bin/lajstrom
bin/lajstrom version
```

On main, on 2026-11-09:

```text
go build ./...
go build -trimpath -ldflags '-X github.com/lajstrom/lajstrom/internal/version.Version=v0.0.1-135-g9260649 -X github.com/lajstrom/lajstrom/internal/version.Commit=9260649 -X github.com/lajstrom/lajstrom/internal/version.Date=2026-11-09T17:32:27Z' -o bin/lajstrom ./cmd/lajstrom
lajstrom v0.0.1-135-g9260649 (9260649, 2026-11-09T17:32:27Z)
```

No C compiler is needed: SQLite is `modernc.org/sqlite`, in Go. The release builds with
`CGO_ENABLED=0`, a static binary; set it for `make build` too when the binary goes to another
machine.

## Next

1. The configuration: [configuration.md](configuration.md); `lajstrom serve --config …`.
2. The first admin: a one-time bootstrap token that you make (`openssl rand -hex 32`), named in
   `auth.bootstrap.tokenFile` and exchanged with `lajstrom admin bootstrap`, or
   `lajstrom admin user create --admin` against the database; `serve` never makes a token:
   [operations.md](operations.md#the-first-admin).
3. Running it for good: [Docker Compose](deploy/compose.md), [Kubernetes](deploy/kubernetes.md),
   behind a [reverse proxy](reverse-proxy.md), and [backups](operations.md#backup-and-restore).
4. A quick start per format: [raw](quickstart/raw.md), [apt](quickstart/apt.md),
   [pypi](quickstart/pypi.md), [maven](quickstart/maven.md), [npm](quickstart/npm.md),
   [OCI with oras](quickstart/oras.md).
