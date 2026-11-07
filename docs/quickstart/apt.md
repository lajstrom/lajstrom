# Quick start: an apt repo

An apt repo serves `.deb` packages to `apt-get`: `curl -T` or `oras push` puts a package in,
`apt-get update && apt-get install` takes it out, and the indexes are signed with a key of the
repo's own. Every package is an OCI artifact in the registry behind Lajstrom; Lajstrom keeps the
index and renders `Release`, `InRelease` and `Packages` from it. This page runs the M2 demo on
your machine. The endpoints are specified in [http-routing.md](../spec/http-routing.md) §3 (apt);
what lands in the registry is the [manifest profile](../spec/manifest-profile.md).

## 1. Start the dev stack with a master key

You need Go, Docker, `curl` and `jq`. The packages are built and installed in `ubuntu:24.04`
containers for `linux/amd64`: native on an amd64 machine, emulated on Apple silicon (OrbStack
and Docker Desktop do it), so nothing of dpkg or apt is needed on the host.

```sh
make dev-up                                       # Zot, distribution and Postgres: docs/dev-stack.md
export LAJSTROM_MASTER_KEY=$(openssl rand -hex 32) # keep it: the repo's signing key is encrypted with it
make run                                          # serves deploy/compose/lajstrom.example.yaml on :8080
```

In another terminal (export the same `LAJSTROM_MASTER_KEY` there too: `admin key` of section 8
needs it), make the repo `apt/ubuntu` on the Zot of the stack:

```sh
bin/lajstrom admin repo create apt/ubuntu --registry local --config deploy/compose/lajstrom.example.yaml
```

```json
{"format":"apt","name":"ubuntu","registry":"local","kind":"hosted","anonymousRead":false,"policy":null,"createdAt":"2026-11-07T17:48:03.284166Z","updatedAt":"2026-11-07T17:48:03.284166Z"}
```

The repo gets its signing key on first use, and from then on `serve` refuses to start without
the master key that encrypts it ([operations.md](../operations.md#keys-and-apt-signing)); to start
over, remove `lajstrom.db` and run `make dev-down RESET=1`. Without `LAJSTROM_MASTER_KEY` the repo
is unsigned, `key.asc`, `InRelease` and `Release.gpg` are `404`, and the client line below is
`deb [trusted=yes] http://localhost:8080/apt/ubuntu jammy main` instead. Everything is open on the
dev stack: authentication comes with #45.

## 2. Build a package

A package of one file, built with `dpkg-deb` in a container:

```sh
mkdir -p hello/DEBIAN hello/usr/share/hello && echo 'hello from lajstrom' > hello/usr/share/hello/greeting
printf 'Package: hello\nVersion: 1.0.0-1\nArchitecture: amd64\nMaintainer: You <you@example.com>\nDescription: says hello\n' > hello/DEBIAN/control
docker run --rm --platform linux/amd64 -u "$(id -u):$(id -g)" -v "$PWD:/w" -w /w ubuntu:24.04 dpkg-deb --root-owner-group --build hello hello_1.0.0-1_amd64.deb
```

```text
dpkg-deb: building package 'hello' in 'hello_1.0.0-1_amd64.deb'.
```

## 3. Upload it (A-01)

```sh
curl -sS -i -T hello_1.0.0-1_amd64.deb "http://localhost:8080/apt/ubuntu/upload?suite=jammy&component=main"
```

```text
HTTP/1.1 201 Created
Content-Type: application/json
Docker-Content-Digest: sha256:d35b504ae333547709098c6f55b4263063e901ff04c258eae97beecd043f7cc0
Location: /apt/ubuntu/pool/main/h/hello/hello_1.0.0-1_amd64.deb
Content-Length: 256

{"package":"hello","version":"1.0.0-1","architecture":"amd64","suite":"jammy","component":"main","filename":"hello_1.0.0-1_amd64.deb","digest":"sha256:d35b504ae333547709098c6f55b4263063e901ff04c258eae97beecd043f7cc0","tag":"1.0.0-1__jammy_2fmain_2famd64"}
```

`suite` and `component` are required: lowercase letters, digits, `.`, `+` and `-`. Suites and
components need no configuration; a repo has those of its packages. `&arch=` may be added, and
then it must be the control file's `Architecture`. The package name, version and architecture
come from the control file; the manifest is tagged with the version and the variant
`jammy/main/amd64` ([manifest-profile.md](../spec/manifest-profile.md) §4.2), and `digest` is its
digest. `Location` is the file in the pool, under the source package's directory as in a Debian
archive (`pool/<component>/<first letter>/<source>/`, `lib` and one more letter for a `lib*`
source). The repo's index lists it now:

```sh
curl -s http://localhost:8080/apt/ubuntu/dists/jammy/main/binary-amd64/Packages
```

```text
Package: hello
Version: 1.0.0-1
Architecture: amd64
Maintainer: You <you@example.com>
Description: says hello
Filename: pool/main/h/hello/hello_1.0.0-1_amd64.deb
Size: 544
MD5sum: 8b587af8c3443a778935363b567d52a0
SHA1: f00c8b9c2e8f7ec0c6f0bf0ca2f243d90abb93f7
SHA256: 13c06eb8263b01df968a4d443cca0ce86cb41e089377c8a2ef79186d201a8590
```

## 4. Install it with apt-get (A-04)

A clean `ubuntu:24.04` on the host's network, where `localhost:8080` is Lajstrom (Linux and
OrbStack):

```sh
docker run --rm -it --network host --platform linux/amd64 ubuntu:24.04 bash
```

In the container, pin the repo's key and add the source, as on any machine
([operations.md](../operations.md#keys-and-apt-signing)); the image has no `curl`, so apt's own
`apt-helper` fetches `key.asc`:

```sh
rm /etc/apt/sources.list.d/ubuntu.sources     # only the Lajstrom repo, for a short output
install -d -m 0755 /etc/apt/keyrings
/usr/lib/apt/apt-helper -o APT::Sandbox::User=root download-file http://localhost:8080/apt/ubuntu/key.asc /etc/apt/keyrings/ubuntu.asc
echo "deb [signed-by=/etc/apt/keyrings/ubuntu.asc] http://localhost:8080/apt/ubuntu jammy main" > /etc/apt/sources.list.d/lajstrom.list
apt-get update && apt-get install -y hello
cat /usr/share/hello/greeting
```

```text
Get:1 http://localhost:8080/apt/ubuntu/key.asc [758 B]
Fetched 758 B in 0s (15.2 kB/s)
Get:1 http://localhost:8080/apt/ubuntu jammy InRelease [1274 B]
Get:2 http://localhost:8080/apt/ubuntu jammy/main amd64 Packages [272 B]
Fetched 1546 B in 1s (2888 B/s)
Reading package lists...
Reading package lists...
Building dependency tree...
Reading state information...
The following NEW packages will be installed:
  hello
0 upgraded, 1 newly installed, 0 to remove and 0 not upgraded.
Need to get 544 B of archives.
After this operation, 0 B of additional disk space will be used.
Get:1 http://localhost:8080/apt/ubuntu jammy/main amd64 hello amd64 1.0.0-1 [544 B]
debconf: delaying package configuration, since apt-utils is not installed
Fetched 544 B in 0s (11.3 kB/s)
Selecting previously unselected package hello.
Preparing to unpack .../hello_1.0.0-1_amd64.deb ...
Unpacking hello (1.0.0-1) ...
Setting up hello (1.0.0-1) ...
hello from lajstrom
```

apt verified `InRelease` against the key it was given (no `[trusted=yes]`) and took `Packages.gz`
by its hash (`Acquire-By-Hash: yes`). On a machine with `curl`, the key is
`curl -fsSL http://localhost:8080/apt/ubuntu/key.asc | sudo tee /etc/apt/keyrings/ubuntu.asc`.

## 5. Download it and check the hash (A-07)

Still in the container:

```sh
cd /tmp && apt-get -o APT::Sandbox::User=root download hello && sha256sum hello_1.0.0-1_amd64.deb
```

```text
Get:1 http://localhost:8080/apt/ubuntu jammy/main amd64 hello amd64 1.0.0-1 [544 B]
Fetched 544 B in 0s (11.0 kB/s)
13c06eb8263b01df968a4d443cca0ce86cb41e089377c8a2ef79186d201a8590  hello_1.0.0-1_amd64.deb
```

The same as `sha256sum hello_1.0.0-1_amd64.deb` (`shasum -a 256` on macOS) on the host: the bytes
uploaded. The pool file is the blob in the registry, read as the facade's blob `GET`:

```sh
curl -sI http://localhost:8080/apt/ubuntu/pool/main/h/hello/hello_1.0.0-1_amd64.deb
```

```text
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 544
Content-Type: application/vnd.debian.binary-package
Docker-Content-Digest: sha256:d35b504ae333547709098c6f55b4263063e901ff04c258eae97beecd043f7cc0
Etag: "sha256:13c06eb8263b01df968a4d443cca0ce86cb41e089377c8a2ef79186d201a8590"
Last-Modified: Sat, 07 Nov 2026 17:48:33 GMT
```

When the registry hands out download URLs (distribution on S3, the `s3` profile of the dev
stack), `GET` answers `307` to it and apt follows; `policy.blobRedirect: never` on the repo
streams instead ([api.md](../api.md)).

## 6. The repeat and the two 409s (A-06, E-07)

The same bytes again are the idempotent repeat: `201`, the same digest, nothing new stored.

```sh
curl -sS -T hello_1.0.0-1_amd64.deb "http://localhost:8080/apt/ubuntu/upload?suite=jammy&component=main" | jq -r .digest
```

```text
sha256:d35b504ae333547709098c6f55b4263063e901ff04c258eae97beecd043f7cc0
```

Other bytes under the same name, version and architecture are refused. Build them:

```sh
mkdir -p other && cp -R hello other/ && echo 'hello again' > other/hello/usr/share/hello/greeting
docker run --rm --platform linux/amd64 -u "$(id -u):$(id -g)" -v "$PWD/other:/w" -w /w ubuntu:24.04 dpkg-deb --root-owner-group --build hello hello_1.0.0-1_amd64.deb
```

In the same suite and component the tag holds another manifest, and tags are immutable:

```sh
curl -sS -i -T other/hello_1.0.0-1_amd64.deb "http://localhost:8080/apt/ubuntu/upload?suite=jammy&component=main"
```

```text
HTTP/1.1 409 Conflict
Content-Type: text/plain; charset=utf-8
X-Content-Type-Options: nosniff
Content-Length: 133

index: tag "1.0.0-1__jammy_2fmain_2famd64" already points at sha256:d35b504ae333547709098c6f55b4263063e901ff04c258eae97beecd043f7cc0
```

In another suite it would be another tag, but the same pool file: the repo has one pool, and a
Debian pool holds one content per file name, or apt fails the hash check in one of the suites:

```sh
curl -sS -i -T other/hello_1.0.0-1_amd64.deb "http://localhost:8080/apt/ubuntu/upload?suite=noble&component=main"
```

```text
HTTP/1.1 409 Conflict
Content-Type: text/plain; charset=utf-8
X-Content-Type-Options: nosniff
Content-Length: 165

index: conflict: pool file name hello_1.0.0-1_amd64.deb is held by version 1.0.0-1 in jammy/main with another content; a Debian pool holds one content per file name
```

The same bytes in `noble` are `201`: a second manifest over the same blob, and one pool file. A
version `1:1.0.0-1` of other bytes is the same `409`: the epoch is not in the file name.

## 7. A second package with oras push (A-02)

A package can also come through the OCI facade, as an `oras push` with the deb profile; the
manifest hook hands it to the same ingest pipeline. Build a second package that depends on the
first:

```sh
mkdir -p world/DEBIAN world/usr/share/world && echo 'hello from oras' > world/usr/share/world/greeting
printf 'Package: world\nVersion: 2.0-1\nArchitecture: amd64\nMaintainer: You <you@example.com>\nDepends: hello\nDescription: says hello too\n' > world/DEBIAN/control
docker run --rm --platform linux/amd64 -u "$(id -u):$(id -g)" -v "$PWD:/w" -w /w ubuntu:24.04 dpkg-deb --root-owner-group --build world world_2.0-1_amd64.deb
```

The manifest needs a metadata layer, which the upload route computes and an `oras push` has to
carry: `lajstrom admin deb metadata` prints it, the same bytes the upload route writes, with no
configuration needed:

```sh
bin/lajstrom admin deb metadata world_2.0-1_amd64.deb > metadata.json
jq . metadata.json
```

```json
{
  "control": {
    "Architecture": "amd64",
    "Depends": "hello",
    "Description": "says hello too",
    "Maintainer": "You <you@example.com>",
    "Package": "world",
    "Version": "2.0-1"
  },
  "filename": "world_2.0-1_amd64.deb",
  "md5": "742c0d1d047e28256efd506c049e3975",
  "md5sums": {},
  "sha1": "42ede0ca06b1bb58b394ecdb4e07f294d0abc7cd",
  "sha256": "9fd4bafef4983c560f177072c2d5b91da8a1de896d6ded1cd7428f62eeaf43f3",
  "size": 552
}
```

oras from its image, as in [oras.md](oras.md):

```sh
oras() { docker run --rm --network host -v "$PWD:/w" -w /w ghcr.io/oras-project/oras:v1.3.6 "$@"; }
oras push --plain-http --artifact-type application/vnd.lajstrom.deb.v1 \
  --annotation lajstrom.schema=1 --annotation lajstrom.format=apt --annotation lajstrom.repo=ubuntu \
  --annotation lajstrom.name=world --annotation lajstrom.version=2.0-1 \
  --annotation lajstrom.variant=jammy/main/amd64 --annotation lajstrom.deb.suite=jammy \
  --annotation lajstrom.deb.component=main --annotation lajstrom.deb.arch=amd64 \
  --annotation org.opencontainers.image.created=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
  localhost:8080/apt/ubuntu/world:2.0-1__jammy_2fmain_2famd64 \
  world_2.0-1_amd64.deb:application/vnd.lajstrom.deb.package.v1 \
  metadata.json:application/vnd.lajstrom.deb.metadata.v1+json
```

```text
Preparing world_2.0-1_amd64.deb
Preparing metadata.json
Uploading b3bf08120400 metadata.json
Uploading 44136fa355b3 application/vnd.oci.empty.v1+json
Uploading 9fd4bafef498 world_2.0-1_amd64.deb
Uploaded  b3bf08120400 metadata.json
Uploaded  44136fa355b3 application/vnd.oci.empty.v1+json
Uploaded  9fd4bafef498 world_2.0-1_amd64.deb
Uploading 8be60c696361 application/vnd.oci.image.manifest.v1+json
Uploaded  8be60c696361 application/vnd.oci.image.manifest.v1+json
Pushed [registry] localhost:8080/apt/ubuntu/world:2.0-1__jammy_2fmain_2famd64
ArtifactType: application/vnd.lajstrom.deb.v1
Digest: sha256:8be60c6963617350c777beb896c5c84841b6eb15c75a653edd36a27617abde14
```

What the frontend sets itself, the client sets here
([manifest-profile.md](../spec/manifest-profile.md) §1 to §4):

- `--artifact-type application/vnd.lajstrom.deb.v1`: the artifact type is `deb`, the repo format
  `apt`;
- `lajstrom.name`, `lajstrom.version` and `lajstrom.deb.arch`: the control file's `Package`,
  `Version` (with its epoch, if any) and `Architecture`; the package path below `apt/ubuntu/` is
  the name;
- `lajstrom.deb.suite` and `lajstrom.deb.component`, and `lajstrom.variant`, the three joined
  with `/`;
- the tag `<version>__<variant>` with every byte but letters, digits, `.` and `-` escaped as `_`
  and two hex digits (`/` is `_2f`, `:` of an epoch `_3a`);
- the `.deb` layer, `application/vnd.lajstrom.deb.package.v1`, titled with the pool file name
  `<name>_<version without the epoch>_<arch>.deb`, which is why oras pushes it from the
  directory it is in; then the metadata layer, last.

Ingest checks all of it against the package (its control file and md5sums) and the metadata,
and a mismatch is a `400` that names the rule. The pool file name rule of section 6 is the upload
route's: a push through the facade cannot be checked against the index, so another `.deb` under a
file name the repo holds is accepted there; the pool route then answers `500` for that name and
logs both digests for the operator, and `lajstrom reindex` (#29) reports it. Upload through
`PUT …/upload` where you can.

Both packages are in the suite now; installing `world` brings `hello` along:

```sh
apt-get update && apt-cache policy hello world   # in the container of section 4
apt-get install -y world && cat /usr/share/world/greeting
```

```text
Get:1 http://localhost:8080/apt/ubuntu jammy InRelease [1274 B]
Get:2 http://localhost:8080/apt/ubuntu jammy/main amd64 Packages [403 B]
Fetched 1677 B in 1s (3130 B/s)
Reading package lists...
hello:
  Installed: (none)
  Candidate: 1.0.0-1
  Version table:
     1.0.0-1 500
        500 http://localhost:8080/apt/ubuntu jammy/main amd64 Packages
world:
  Installed: (none)
  Candidate: 2.0-1
  Version table:
     2.0-1 500
        500 http://localhost:8080/apt/ubuntu jammy/main amd64 Packages
Reading package lists...
Building dependency tree...
Reading state information...
The following additional packages will be installed:
  hello
The following NEW packages will be installed:
  hello world
0 upgraded, 2 newly installed, 0 to remove and 0 not upgraded.
Need to get 1096 B of archives.
After this operation, 0 B of additional disk space will be used.
Get:1 http://localhost:8080/apt/ubuntu jammy/main amd64 hello amd64 1.0.0-1 [544 B]
Get:2 http://localhost:8080/apt/ubuntu jammy/main amd64 world amd64 2.0-1 [552 B]
debconf: delaying package configuration, since apt-utils is not installed
Fetched 1096 B in 0s (22.4 kB/s)
Selecting previously unselected package hello.
Preparing to unpack .../hello_1.0.0-1_amd64.deb ...
Unpacking hello (1.0.0-1) ...
Selecting previously unselected package world.
Preparing to unpack .../archives/world_2.0-1_amd64.deb ...
Unpacking world (2.0-1) ...
Setting up hello (1.0.0-1) ...
Setting up world (2.0-1) ...
hello from oras
```

(This output is from a fresh container; in the one of section 4 `hello` is installed already.)

## 8. Rotate the key

```sh
bin/lajstrom admin key rotate --repo apt/ubuntu --config deploy/compose/lajstrom.example.yaml
```

```text
time=2026-11-07T18:50:17.366+01:00 level=INFO msg="signing key rotated" repo=apt/ubuntu fingerprint=35C7506AC5AFACF61337EDAC87726522A83C3733 retired=8DFC877B80909766BF2BF13049628AC6D674219B algorithm=ed25519
35C7506AC5AFACF61337EDAC87726522A83C3733
```

The new key signs from now on, and for `keys.retireGrace` (720 hours in the example) the retired
key signs `InRelease` beside it and stays in `key.asc`, so clients that pinned the old key keep
updating until they fetch `key.asc` again
([operations.md](../operations.md#keys-and-apt-signing) has the runbook):

```sh
curl -s http://localhost:8080/apt/ubuntu/key.asc | gpg --show-keys
```

```text
pub   ed25519 2026-11-07 [SC]
      35C7506AC5AFACF61337EDAC87726522A83C3733
uid                      Lajstrom apt/ubuntu
sub   cv25519 2026-11-07 [E]
      B56AAA01837F8F18A4E9A1B5446FD7B70DE04272

pub   ed25519 2026-11-07 [SC]
      8DFC877B80909766BF2BF13049628AC6D674219B
uid                      Lajstrom apt/ubuntu
sub   cv25519 2026-11-07 [E]
      B5B5B7B015D9C8BBAD1EC0C7E521CEFE59E74627
```

## The end-to-end script

`test/e2e/apt.sh` runs sections 2 to 7 with checks against any Lajstrom with a writable apt repo:
two packages named after the run, one uploaded with `PUT`, one pushed with oras, `apt-get update
&& apt-get install` of both signed by `key.asc` (or `[trusted=yes]` when the server does not sign),
`apt-get download` and the hashes, the repeat and the two `409`s. It needs `curl`, `jq` and
Docker, runs `admin deb metadata` from `bin/lajstrom` (or `go run`), and builds and installs in
`ubuntu:24.04`, or in the image `APT_E2E_IMAGE` names (`ubuntu:22.04` and `debian:12` pass too):

```sh
test/e2e/apt.sh http://localhost:8080 ubuntu
```

```text
ok   dpkg-deb --build: e2e-put-20261107175156-7721_1.0.0-1_amd64.deb, e2e-oras-20261107175156-7721_1.0.0-1_amd64.deb, and other bytes under the first name
ok   PUT e2e-put-20261107175156-7721_1.0.0-1_amd64.deb to jammy/main: 201 with sha256:9bd826ef9b87f9c70b75ab1e2e7e91cd83c50134b10c5766f14bbc865a130d63, Location in the pool
ok   oras push e2e-oras-20261107175156-7721_1.0.0-1_amd64.deb to jammy/main: Digest: sha256:30a2e3b3b227950113806e981fe3f341705d594f370ed2579c57ebff413934aa
ok   GET key.asc: 200, the source is signed-by it
ok   apt-get update && apt-get install e2e-put-20261107175156-7721 e2e-oras-20261107175156-7721 in ubuntu:24.04 [signed-by=/etc/apt/keyrings/ubuntu.asc]: both installed, their files there
ok   apt-get download e2e-put-20261107175156-7721 e2e-oras-20261107175156-7721: sha256 644c85b74c90915d3e6a61af61dd8cd6d427dee6655e25732b21b73ef8504ef0 and 0bb3c58942f28c311c9b99b8f8d05f85727a50b16d659fa17d134408dfc54852, those of the uploaded files
ok   PUT of the same bytes again: 201, the same digest sha256:9bd826ef9b87f9c70b75ab1e2e7e91cd83c50134b10c5766f14bbc865a130d63
ok   PUT of other bytes as e2e-put-20261107175156-7721 1.0.0-1 amd64 to jammy/main: 409, index: tag "1.0.0-1__jammy_2fmain_2famd64" already points at sha256:9bd826ef9b87f9c70b75ab1e2e7e91cd83c50134b10c5766f14bbc865a130d63
ok   PUT of other bytes as e2e-put-20261107175156-7721 1.0.0-1 amd64 to noble/main: 409, index: conflict: pool file name e2e-put-20261107175156-7721_1.0.0-1_amd64.deb is held by version 1.0.0-1 in jammy/main with another content; a Debian pool holds one content per file name
PASS apt e2e against http://localhost:8080/apt/ubuntu/
```

apt versions cannot be deleted yet, so the packages of each run stay in the repo.

## Clean up

```sh
make dev-down RESET=1   # the registries forget what they hold
rm lajstrom.db          # the index, and with it the repo's keys
```
