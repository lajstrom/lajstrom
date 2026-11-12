# Quick start: an rpm repo

An rpm repo serves `.rpm` packages to `dnf`, `yum` and `zypper`. You put a package in with
`curl -T` or `oras push`, and take it out with `dnf install`. The repodata (`repomd.xml` and the
`primary`, `filelists` and `other` documents) is signed with a key of the repo's own. Every package
is an OCI artifact in the registry behind Lajstrom. Lajstrom keeps the index and renders the
repodata from it, so `lajstrom reindex` can rebuild all of it from the registry.

The endpoints are specified in [http-routing.md](../spec/http-routing.md) §3 (rpm). What lands in
the registry is the [manifest profile](../spec/manifest-profile.md).

One repo is one yum repository with every architecture in it: dnf picks its own. A distribution
release (el8, el9, fc41) is a repo of its own. Modularity, comps groups and `updateinfo` are not
served.

**About the outputs on this page.** The commands are those that
[test/e2e/rpm.sh](../../test/e2e/rpm.sh) runs. The outputs show the shape of each answer, not a
captured run: your digests, sizes and times are your own.

## 1. Start the dev stack with a master key

You need Go, Docker, `curl` and `jq`. The packages are built and installed in Rocky Linux 9
containers, so nothing of rpm or dnf is needed on the host.

```sh
make dev-up                                       # Zot, distribution and Postgres: docs/dev-stack.md
export LAJSTROM_MASTER_KEY=$(openssl rand -hex 32) # keep it: the repo's signing key is encrypted with it
make run                                          # serves deploy/compose/lajstrom.example.yaml on 127.0.0.1:8080
```

In another terminal, once `make run` has logged `listening`, export the same
`LAJSTROM_MASTER_KEY` there too. Then make the repo `rpm/el9` on the Zot of the stack, and the
first user, an admin:

```sh
bin/lajstrom admin repo create rpm/el9 --registry local --config deploy/compose/lajstrom.example.yaml
pw='a password of your own'
printf '%s' "$pw" | bin/lajstrom admin user create --name admin --admin --password-stdin --config deploy/compose/lajstrom.example.yaml
u="admin:$pw"
```

The repo gets its signing key on first use, as an apt repo does
([operations.md](../operations.md#keys-and-apt-signing)).

Without `LAJSTROM_MASTER_KEY` the repo is unsigned, and `key.asc`, `repodata/repomd.xml.key` and
`repodata/repomd.xml.asc` are `404`. Clients then set `repo_gpgcheck=0`.

A request without credentials is `401` unless the repo has `anonymousRead`.

## 2. Build a package

Build a package of one command with `rpmbuild` in a Rocky container. The gzip payload keeps it
readable by the rpm of older distributions too:

```sh
mkdir -p rpmbuild/SPECS
cat > rpmbuild/SPECS/hello.spec <<'EOF'
Name: hello
Version: 1.0
Release: 1.el9
Summary: says hello
License: Apache-2.0
BuildArch: noarch
%description
Says hello from Lajstrom.
%install
mkdir -p %{buildroot}/usr/bin
printf '#!/bin/sh\necho hello from lajstrom\n' > %{buildroot}/usr/bin/hello && chmod 0755 %{buildroot}/usr/bin/hello
%files
/usr/bin/hello
%changelog
* Thu Nov 12 2026 You <you@example.com> - 1.0-1.el9
- First build
EOF
docker run --rm --platform linux/amd64 -v "$PWD:/w" -w /w quay.io/rockylinux/rockylinux:9.7 bash -ec "
  dnf -q -y install rpm-build >/dev/null
  rpmbuild -bb --define '_topdir /w/rpmbuild' --define '_binary_payload w9.gzdio' rpmbuild/SPECS/hello.spec
  chown -R $(id -u):$(id -g) rpmbuild"
cp rpmbuild/RPMS/noarch/hello-1.0-1.el9.noarch.rpm .
```

## 3. Upload it

```sh
curl -sS -u "$u" -i -T hello-1.0-1.el9.noarch.rpm http://localhost:8080/rpm/el9/upload
```

```text
HTTP/1.1 201 Created
Content-Type: application/json
Docker-Content-Digest: sha256:…
Location: /rpm/el9/Packages/hello-1.0-1.el9.noarch.rpm

{"name":"hello","epoch":0,"version":"1.0","release":"1.el9","arch":"noarch","filename":"hello-1.0-1.el9.noarch.rpm","digest":"sha256:…","tag":"1.0-1.el9__noarch"}
```

The name, epoch, version, release and arch come from the rpm header, and the upload needs no
parameters. The manifest is tagged with the version, `[epoch:]version-release` (without an epoch
when it is 0), and the arch as its variant ([manifest-profile.md](../spec/manifest-profile.md)
§4.2). A `_` in an arch is escaped: `x86_64` makes `…__x86_5f64`. `digest` is the manifest's
digest.

The body streams into the registry. Its head, up to the end of the rpm header, is read first, and
a header over 32 MiB is a `400`. A source package (`.src.rpm`) goes in the same way, with the arch
`src`.

The repodata lists the package now:

```sh
curl -s -u "$u" http://localhost:8080/rpm/el9/repodata/repomd.xml
```

```xml
<?xml version="1.0" encoding="UTF-8"?>
<repomd xmlns="http://linux.duke.edu/metadata/repo" xmlns:rpm="http://linux.duke.edu/metadata/rpm">
  <revision>…</revision>
  <data type="primary">
    <checksum type="sha256">…</checksum>
    <open-checksum type="sha256">…</open-checksum>
    <location href="repodata/…-primary.xml.gz"/>
    …
  </data>
  <data type="filelists">…</data>
  <data type="other">…</data>
</repomd>
```

Each document is named by its checksum and served while the current `repomd.xml` lists it. A
publish or a delete renders the repodata again on the next `GET`. A repo without packages has
the three documents empty, so dnf can read a repo you just created.

## 4. Install it with dnf

dnf reads with a deploy token of the repo with `read`: it reaches this repo only and never
writes. Make one:

```sh
curl -s -u "$u" -H 'Content-Type: application/json' -d '{"name": "dnf clients", "scopes": ["read"]}' \
  http://localhost:8080/api/v1/repos/rpm/el9/deploy-tokens | jq -r .secret > rpm-token
```

Start a clean Rocky container on the host's network, where `localhost:8080` is Lajstrom (Linux
and OrbStack):

```sh
docker run --rm -it --network host --platform linux/amd64 -e RPM_TOKEN="$(cat rpm-token)" quay.io/rockylinux/rockylinux:9.7 bash
```

In the container, add the repo with `repo_gpgcheck=1` and the repo's key as `gpgkey`. dnf checks
the signature of `repomd.xml`, `repodata/repomd.xml.asc`, with it. The package itself is not
signed, hence `gpgcheck=0`. A package you sign with `rpmsign` keeps its own signature: set
`gpgcheck=1` and add your key to `gpgkey`.

```sh
curl -s -u "deploy:$RPM_TOKEN" -o /etc/pki/rpm-gpg/lajstrom-el9.asc http://localhost:8080/rpm/el9/key.asc
cat > /etc/yum.repos.d/lajstrom-el9.repo <<EOF
[lajstrom-el9]
name=Lajstrom rpm/el9
baseurl=http://localhost:8080/rpm/el9/
username=deploy
password=$RPM_TOKEN
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/lajstrom-el9.asc
gpgcheck=0
EOF
dnf -y --disablerepo='*' --enablerepo=lajstrom-el9 install hello
hello
```

dnf imports the repo's key the first time (`Importing GPG key 0x…`), then installs, and `hello`
says `hello from lajstrom`.

`gpgkey=http://localhost:8080/rpm/el9/repodata/repomd.xml.key` works too, when the repo has
`anonymousRead`.

## 5. Install it with zypper

zypper fetches `repodata/repomd.xml.key` beside `repomd.xml.asc` by itself. In an openSUSE Leap
container:

```sh
docker run --rm -it --network host --platform linux/amd64 -e RPM_TOKEN="$(cat rpm-token)" registry.opensuse.org/opensuse/leap:15.6 bash
```

```sh
printf 'username=deploy\npassword=%s\n' "$RPM_TOKEN" > /etc/zypp/credentials.d/lajstrom
zypper -n addrepo --gpgcheck-allow-unsigned-rpm 'http://localhost:8080/rpm/el9/?credentials=lajstrom' lajstrom-el9
zypper -n --gpg-auto-import-keys refresh lajstrom-el9
zypper -n install hello
```

## 6. The repeat and the 409s

The same bytes again are the idempotent repeat: `201` with the same digest, and nothing is
written. Other bytes under the same name, version and arch are a `409` naming the manifest the tag
holds. Tags never move.

The file name `<name>-<version>-<release>.<arch>.rpm` has no epoch. So a package that differs from
one in the repo only by its epoch (`1:1.0-1.el9` beside `1.0-1.el9`) is a `409` too, naming the
version that holds the file name: a repo serves one content per file name.

## 7. A second package with oras push

A package can also come through the OCI facade, as an `oras push` with the rpm profile. Build
`world` as in section 2, with `Epoch: 1` and `Requires: hello`.

The manifest needs a metadata layer, which `lajstrom admin rpm metadata` prints: the same bytes the
upload route writes.

```sh
bin/lajstrom admin rpm metadata world-1.0-1.el9.noarch.rpm > metadata.json
oras() { docker run --rm -i --network host -v "$PWD:/w" -w /w ghcr.io/oras-project/oras:v1.3.6 "$@" --registry-config /w/.oras-auth.json; }
printf '%s\n' "$pw" | oras login --plain-http -u admin --password-stdin localhost:8080
oras push --plain-http --artifact-type application/vnd.lajstrom.rpm.v1 \
  --annotation lajstrom.schema=1 --annotation lajstrom.format=rpm --annotation lajstrom.repo=el9 \
  --annotation lajstrom.name=world --annotation lajstrom.version=1:1.0-1.el9 --annotation lajstrom.variant=noarch \
  --annotation org.opencontainers.image.created=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
  localhost:8080/rpm/el9/world:1_3a1.0-1.el9__noarch \
  world-1.0-1.el9.noarch.rpm:application/vnd.lajstrom.rpm.package.v1 \
  metadata.json:application/vnd.lajstrom.rpm.metadata.v1+json
```

Ingest reads the header of the pushed file and refuses a metadata layer that is not what the
header says, naming the first key that differs. It also refuses a title that is not
`<name>-<version>-<release>.<arch>.rpm` of the annotations.

The file name rule of section 6 is the upload route's: a push cannot be checked against the index.
For a file name held with two contents, the `Packages/` route answers `500` and logs both digests.
Upload through `PUT …/upload` where you can.

## 8. Delete, and rebuild from the registry

A version goes with `DELETE` on the API. Name the arch with `variant` to take one architecture
only:

```sh
curl -u "$u" -X DELETE 'http://localhost:8080/api/v1/repos/rpm/el9/packages/world/versions/1%3A1.0-1.el9?variant=noarch'
```

The next `repomd.xml` no longer lists it, and `dnf clean all && dnf list --available world` finds
nothing.

The repodata is derived. Everything it needs per package is in the manifest's metadata layer, so a
reindex rebuilds it from the registry. The rebuilt `primary`, `filelists` and `other` are the same
bytes, under the same names:

```sh
curl -u "$u" -X POST http://localhost:8080/api/v1/repos/rpm/el9/reindex
curl -s -u "$u" http://localhost:8080/api/v1/repos/rpm/el9/reindex | jq '{state, report: .report.added}'
```

## 9. Rotate the key

```sh
bin/lajstrom admin key rotate --repo rpm/el9 --config deploy/compose/lajstrom.example.yaml
```

The new key signs from now on. For `keys.retireGrace` the retired key signs `repomd.xml.asc`
beside it and stays in `key.asc`. So clients that imported the old key keep refreshing until they
import the new one ([operations.md](../operations.md#keys-and-apt-signing) has the runbook, written
for apt; the same commands serve rpm repos).

## The end-to-end script

[test/e2e/rpm.sh](../../test/e2e/rpm.sh) runs this page with checks against any Lajstrom, as an
admin. It:

- creates a repo of its own and builds two packages with `rpmbuild`
- uploads one with `curl` and pushes the other with oras
- checks the repodata against its checksums
- runs `dnf install` with `repo_gpgcheck=1`, and the same with zypper
- checks the `409`
- deletes a version and checks that primary and dnf no longer have it
- rebuilds the repo with a reindex after deleting it with `?force=true`, compares the documents,
  and reinstalls from it with dnf

```sh
make e2e E2E=rpm
```

## Clean up

```sh
make dev-down RESET=1   # the registries forget what they hold
rm lajstrom.db          # the index, and with it the repo's keys
rm -r rpmbuild hello-1.0-1.el9.noarch.rpm world-1.0-1.el9.noarch.rpm metadata.json rpm-token .oras-auth.json
```
