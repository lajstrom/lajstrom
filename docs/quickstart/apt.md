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

In another terminal, once `make run` has logged `listening` (`serve` creates the tables of
`lajstrom.db`; before that, `admin` fails with `the index database is not migrated`), and with the same
`LAJSTROM_MASTER_KEY` exported there too (`admin key` of section 8 needs it), make the repo
`apt/ubuntu` on the Zot of the stack, and the first user, an admin, with a password of your own
(8 bytes at least):

```sh
bin/lajstrom admin repo create apt/ubuntu --registry local --config deploy/compose/lajstrom.example.yaml
pw='a password of your own'
printf '%s' "$pw" | bin/lajstrom admin user create --name admin --admin --password-stdin --config deploy/compose/lajstrom.example.yaml
u="admin:$pw"
```

```json
{"format":"apt","name":"ubuntu","registry":"local","kind":"hosted","anonymousRead":false,"policy":null,"createdAt":"2026-11-11T07:27:10.414239Z","updatedAt":"2026-11-11T07:27:10.414239Z"}
{"uid":"01a1c980-e66e-745c-8003-2f2d631dda41","name":"admin","kind":"local","disabled":false,"createdAt":"2026-11-11T07:27:10.446672Z"}
```

The repo gets its signing key on first use, and from then on `serve` refuses to start without
the master key that encrypts it ([operations.md](../operations.md#keys-and-apt-signing)); to start
over, remove `lajstrom.db` and run `make dev-down RESET=1`. Without `LAJSTROM_MASTER_KEY` the repo
is unsigned, `key.asc`, `InRelease` and `Release.gpg` are `404`, and the client line below is
`deb [trusted=yes] http://localhost:8080/apt/ubuntu jammy main` instead.

Nothing is open, on the dev stack either: a request without credentials is `401`, `key.asc`
included. The uploads below send the admin's (`-u "$u"`); apt-get reads with a deploy token of
its own (section 4). Who may do what is in [auth.md](../auth.md).

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

`dpkg-deb` records the times of the files, so your `.deb` differs from the one of this page by a
few bytes: its size, its hashes and the digests below are your own.

## 3. Upload it (A-01)

```sh
curl -sS -u "$u" -i -T hello_1.0.0-1_amd64.deb "http://localhost:8080/apt/ubuntu/upload?suite=jammy&component=main"
```

```text
HTTP/1.1 201 Created
Content-Type: application/json
Docker-Content-Digest: sha256:9ce05923875bfdaf6260fa85804a95e7488f08802d49110cd7f2537af2d84720
Location: /apt/ubuntu/pool/main/h/hello/hello_1.0.0-1_amd64.deb
Content-Length: 256

{"package":"hello","version":"1.0.0-1","architecture":"amd64","suite":"jammy","component":"main","filename":"hello_1.0.0-1_amd64.deb","digest":"sha256:9ce05923875bfdaf6260fa85804a95e7488f08802d49110cd7f2537af2d84720","tag":"1.0.0-1__jammy_2fmain_2famd64"}
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
curl -s -u "$u" http://localhost:8080/apt/ubuntu/dists/jammy/main/binary-amd64/Packages
```

```text
Package: hello
Version: 1.0.0-1
Architecture: amd64
Maintainer: You <you@example.com>
Description: says hello
Filename: pool/main/h/hello/hello_1.0.0-1_amd64.deb
Size: 544
MD5sum: ef150acf940c9996cecffe460a80eec5
SHA1: c460d07dd4156d183975894794c10493de4b08e3
SHA256: 3b4aaa5310bd8c7c3d8dbaf8a8bc5644bf9a3fed1d53ad84da9e2e8845d85ead
```

## 4. Install it with apt-get (A-04)

apt-get reads with a deploy token of the repo with `read`: it reaches this repo only and never
writes, so it is the credential to put on machines, rather than a user's password. Make one; its
secret is shown this once:

```sh
curl -s -u "$u" -H 'Content-Type: application/json' -d '{"name": "apt clients", "scopes": ["read"]}' \
  http://localhost:8080/api/v1/repos/apt/ubuntu/deploy-tokens | jq -r .secret > apt-token
```

A clean `ubuntu:24.04` on the host's network, where `localhost:8080` is Lajstrom (Linux and
OrbStack), with the secret in its environment:

```sh
docker run --rm -it --network host --platform linux/amd64 -e APT_TOKEN="$(cat apt-token)" ubuntu:24.04 bash
```

In the container, give apt the token, pin the repo's key and add the source, as on any machine
([operations.md](../operations.md#keys-and-apt-signing)). The token goes in
`/etc/apt/auth.conf.d` as the password of the login `deploy`, under a machine written with
`http://`: that is the only form for which apt sends credentials over plain http. The image has
no `curl`, so apt's own `apt-helper` fetches `key.asc`, with the same `auth.conf`. In a Debian
image (`debian:13` was tried) the file to remove is `debian.sources`; the rest is the same:

```sh
rm /etc/apt/sources.list.d/ubuntu.sources     # only the Lajstrom repo, for a short output
printf 'machine http://localhost:8080/apt/ubuntu\nlogin deploy\npassword %s\n' "$APT_TOKEN" > /etc/apt/auth.conf.d/lajstrom.conf
chmod 600 /etc/apt/auth.conf.d/lajstrom.conf
install -d -m 0755 /etc/apt/keyrings
/usr/lib/apt/apt-helper -o APT::Sandbox::User=root download-file http://localhost:8080/apt/ubuntu/key.asc /etc/apt/keyrings/ubuntu.asc
echo "deb [signed-by=/etc/apt/keyrings/ubuntu.asc] http://localhost:8080/apt/ubuntu jammy main" > /etc/apt/sources.list.d/lajstrom.list
apt-get update && apt-get install -y hello
cat /usr/share/hello/greeting
```

```text
Get:1 http://localhost:8080/apt/ubuntu/key.asc [758 B]
Fetched 758 B in 0s (14.6 kB/s)
Get:1 http://localhost:8080/apt/ubuntu jammy InRelease [1274 B]
Get:2 http://localhost:8080/apt/ubuntu jammy/main amd64 Packages [271 B]
Fetched 1545 B in 0s (3091 B/s)
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
Fetched 544 B in 0s (12.1 kB/s)
Selecting previously unselected package hello.
Preparing to unpack .../hello_1.0.0-1_amd64.deb ...
Unpacking hello (1.0.0-1) ...
Setting up hello (1.0.0-1) ...
hello from lajstrom
```

apt verified `InRelease` against the key it was given (no `[trusted=yes]`) and took `Packages.gz`
by its hash (`Acquire-By-Hash: yes`). On a machine with `curl`, the key is
`curl -fsSL -u "deploy:$APT_TOKEN" http://localhost:8080/apt/ubuntu/key.asc | sudo tee /etc/apt/keyrings/ubuntu.asc`.
Without the `auth.conf` file, `apt-get update` fails, and says why:

```text
E: Failed to fetch http://localhost:8080/apt/ubuntu/dists/jammy/InRelease  401  Unauthorized [IP: 127.0.0.1 8080]
```

## 5. Download it and check the hash (A-07)

Still in the container:

```sh
cd /tmp && apt-get -o APT::Sandbox::User=root download hello && sha256sum hello_1.0.0-1_amd64.deb
```

```text
Get:1 http://localhost:8080/apt/ubuntu jammy/main amd64 hello amd64 1.0.0-1 [544 B]
Fetched 544 B in 0s (10.3 kB/s)
3b4aaa5310bd8c7c3d8dbaf8a8bc5644bf9a3fed1d53ad84da9e2e8845d85ead  hello_1.0.0-1_amd64.deb
```

The same as `sha256sum hello_1.0.0-1_amd64.deb` (`shasum -a 256` on macOS) on the host: the bytes
uploaded. The pool file is the blob in the registry, read as the facade's blob `GET`:

```sh
curl -sI -u "$u" http://localhost:8080/apt/ubuntu/pool/main/h/hello/hello_1.0.0-1_amd64.deb
```

```text
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 544
Content-Type: application/vnd.debian.binary-package
Docker-Content-Digest: sha256:9ce05923875bfdaf6260fa85804a95e7488f08802d49110cd7f2537af2d84720
Etag: "sha256:3b4aaa5310bd8c7c3d8dbaf8a8bc5644bf9a3fed1d53ad84da9e2e8845d85ead"
Last-Modified: Mon, 09 Nov 2026 11:22:02 GMT
```

When the registry hands out download URLs (distribution on S3, the `s3` profile of the dev
stack), `GET` answers `307` to it and apt follows; `policy.blobRedirect: never` on the repo
streams instead ([api.md](../api.md)).

## 6. The repeat and the two 409s (A-06, E-07)

The same bytes again are the idempotent repeat: `201`, the same digest, nothing new stored.

```sh
curl -sS -u "$u" -T hello_1.0.0-1_amd64.deb "http://localhost:8080/apt/ubuntu/upload?suite=jammy&component=main" | jq -r .digest
```

```text
sha256:9ce05923875bfdaf6260fa85804a95e7488f08802d49110cd7f2537af2d84720
```

Other bytes under the same name, version and architecture are refused. Build them:

```sh
mkdir -p other && cp -R hello other/ && echo 'hello again' > other/hello/usr/share/hello/greeting
docker run --rm --platform linux/amd64 -u "$(id -u):$(id -g)" -v "$PWD/other:/w" -w /w ubuntu:24.04 dpkg-deb --root-owner-group --build hello hello_1.0.0-1_amd64.deb
```

In the same suite and component the tag holds another manifest, and tags are immutable:

```sh
curl -sS -u "$u" -i -T other/hello_1.0.0-1_amd64.deb "http://localhost:8080/apt/ubuntu/upload?suite=jammy&component=main"
```

```text
HTTP/1.1 409 Conflict
Content-Type: text/plain; charset=utf-8
X-Content-Type-Options: nosniff
Content-Length: 133

index: tag "1.0.0-1__jammy_2fmain_2famd64" already points at sha256:9ce05923875bfdaf6260fa85804a95e7488f08802d49110cd7f2537af2d84720
```

In another suite it would be another tag, but the same pool file: the repo has one pool, and a
Debian pool holds one content per file name, or apt fails the hash check in one of the suites:

```sh
curl -sS -u "$u" -i -T other/hello_1.0.0-1_amd64.deb "http://localhost:8080/apt/ubuntu/upload?suite=noble&component=main"
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
  "md5": "982ee694203649d75e40d8bef5b4c061",
  "md5sums": {},
  "sha1": "d5907133e1d774fc0269303ad2fa8ec62609eaa6",
  "sha256": "de3391aec059d1488b2957d44d92187d77f69b1383c97c39c4ca2b0f6544c67c",
  "size": 552
}
```

oras from its image, as in [oras.md](oras.md), logged in as the admin (the login is kept in
`.oras-auth.json`; `rm .oras-auth.json` ends it):

```sh
oras() { docker run --rm -i --network host -v "$PWD:/w" -w /w ghcr.io/oras-project/oras:v1.3.6 "$@" --registry-config /w/.oras-auth.json; }
printf '%s\n' "$pw" | oras login --plain-http -u admin --password-stdin localhost:8080   # Login Succeeded
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
Uploading 8056e80399e3 metadata.json
Uploading 44136fa355b3 application/vnd.oci.empty.v1+json
Uploaded  8056e80399e3 metadata.json
Uploading de3391aec059 world_2.0-1_amd64.deb
Uploaded  44136fa355b3 application/vnd.oci.empty.v1+json
Uploaded  de3391aec059 world_2.0-1_amd64.deb
Uploading 5f314216d6f5 application/vnd.oci.image.manifest.v1+json
Uploaded  5f314216d6f5 application/vnd.oci.image.manifest.v1+json
Pushed [registry] localhost:8080/apt/ubuntu/world:2.0-1__jammy_2fmain_2famd64
ArtifactType: application/vnd.lajstrom.deb.v1
Digest: sha256:5f314216d6f51590feadef916074b75399a15123d7f736c24774eea4d7a366b4
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
Get:2 http://localhost:8080/apt/ubuntu jammy/main amd64 Packages [404 B]
Fetched 1678 B in 1s (3149 B/s)
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
Fetched 1096 B in 0s (22.5 kB/s)
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
time=2026-11-09T12:22:52.761+01:00 level=INFO msg="signing key rotated" repo=apt/ubuntu fingerprint=90437F941E860E17CAC800E38E96B0284D430BA4 retired=F762A1BC316399727A66E759559497E61058255B algorithm=ed25519
90437F941E860E17CAC800E38E96B0284D430BA4
```

The new key signs from now on, and for `keys.retireGrace` (720 hours in the example) the retired
key signs `InRelease` beside it and stays in `key.asc`, so clients that pinned the old key keep
updating until they fetch `key.asc` again
([operations.md](../operations.md#keys-and-apt-signing) has the runbook):

```sh
curl -s -u "$u" http://localhost:8080/apt/ubuntu/key.asc | gpg --show-keys
```

```text
pub   ed25519 2026-11-09 [SC]
      90437F941E860E17CAC800E38E96B0284D430BA4
uid                      Lajstrom apt/ubuntu
sub   cv25519 2026-11-09 [E]
      B2189B0A31BEEC57D7DABF8804FA284DEB71E114

pub   ed25519 2026-11-09 [SC]
      F762A1BC316399727A66E759559497E61058255B
uid                      Lajstrom apt/ubuntu
sub   cv25519 2026-11-09 [E]
      1C1B89A5715194FBB62B03069764BAAA3D3901A6
```

## The end-to-end script

`test/e2e/apt.sh` runs sections 2 to 7 with checks against any Lajstrom with a writable apt repo:
two packages named after the run, one uploaded with `PUT`, one pushed with oras; the repo's
`anonymousRead` turned on through the API, `key.asc` and `apt-get update && apt-get install` of
both without credentials, signed by `key.asc` (or `[trusted=yes]` when the server does not sign),
and `apt-cache policy`; `anonymousRead` off, the anonymous `apt-get update` refused with a `401`,
then the same with credentials in `auth.conf`, `apt-get download` and the hashes; a second
`apt-get update` with `Acquire::By-Hash=no`; a copy of the suite with wrong hashes in its
`Release`, which apt refuses with `Hash Sum mismatch`; the repeat and the two `409`s; last, both
versions deleted through the API (#61), after which neither `Packages` nor apt has them. It runs
as the user of `LAJSTROM_E2E_USER` and `LAJSTROM_E2E_PASSWORD`, which needs `write`, `manage` and
`delete` on the repo, sets the repo's `anonymousRead` back as it was on exit, and needs the
server's `auth.anonymousRead: true`. It needs `curl`, `jq` and Docker, runs `admin deb metadata`
from `bin/lajstrom` (or `go run`), and builds and installs in `ubuntu:24.04`, or in the image
`APT_E2E_IMAGE` names (`ubuntu:22.04`, `debian:12` and `debian:13` pass too). Given the server's
log file in `LAJSTROM_SERVER_LOG`, it also checks that apt asked for the `Packages` by-hash, and
by name with `Acquire::By-Hash=no`.

`make e2e E2E=apt` runs it against a server of its own, which has `auth.anonymousRead: true`. The
example configuration has it off, so against the server of this page the script fails at the
anonymous step until the switch is on in a copy of the example, which `make run` prefers, and
`make run` is started again, with the same `LAJSTROM_MASTER_KEY`:

```sh
sed 's/^  anonymousRead: false/  anonymousRead: true/' deploy/compose/lajstrom.example.yaml > deploy/compose/lajstrom.yaml   # auth's, not the repos'
LAJSTROM_E2E_USER=admin LAJSTROM_E2E_PASSWORD="$pw" test/e2e/apt.sh http://localhost:8080 ubuntu
```

```text
ok   dpkg-deb --build: e2e-put-20261109161811-25900_1.0.0-1_amd64.deb, e2e-oras-20261109161811-25900_1.0.0-1_amd64.deb, and other bytes under the first name
ok   PUT e2e-put-20261109161811-25900_1.0.0-1_amd64.deb to jammy/main: 201 with sha256:0bfafbc6d84febf324cc9e55f1cc8580930864964cbc3e1c9bff32193fb8c248, Location in the pool
ok   oras push e2e-oras-20261109161811-25900_1.0.0-1_amd64.deb to jammy/main: Digest: sha256:b04c365e7520f077faf1cade1f953bf81e975468361d150c6cfbd570475658fa
ok   PATCH /api/v1/repos/apt/ubuntu anonymousRead true (it was false): anonymous GET of dists/jammy/Release 200
ok   anonymous GET key.asc: 200, the source is signed-by it
ok   client container ubuntu:24.04 (linux/amd64): apt 2.8.3 (amd64)
ok   anonymous apt-get update && apt-get install e2e-put-20261109161811-25900 e2e-oras-20261109161811-25900 [signed-by=/etc/apt/keyrings/ubuntu.asc]: both installed, their files there, apt-cache policy candidate 1.0.0-1 from jammy/main; how apt fetched the Packages is not checked without a server log
ok   PATCH anonymousRead false: anonymous GET 401, anonymous apt-get update fails: E: Failed to fetch http://localhost:8080/apt/ubuntu/dists/jammy/InRelease  401  Unauthorized [IP: 127.0.0.1 8080]
ok   apt-get update && apt-get install e2e-put-20261109161811-25900 e2e-oras-20261109161811-25900, auth.conf login admin: both installed again, apt-cache policy candidate 1.0.0-1; how apt fetched the Packages is not checked without a server log
ok   apt-get download e2e-put-20261109161811-25900 e2e-oras-20261109161811-25900: sha256 712eb898f671bfc9dcf63b0a67ced4bcae991112d1b4b922c072dff52eb66190 and ef9f1405d75f13134e1d13b1c45945c475a9b4796af290e5db0edf7e5750109b, those of the uploaded files
ok   apt-get -o Acquire::By-Hash=no update: apt-cache policy candidate 1.0.0-1; how apt fetched the Packages is not checked without a server log
ok   a copy of the suite whose Release has every SHA256 one digit off: apt-get update fails: E: Failed to fetch file:/w/mirror/dists/jammy/main/binary-amd64/Packages.gz Hash Sum mismatch
ok   PUT of the same bytes again: 201, the same digest sha256:0bfafbc6d84febf324cc9e55f1cc8580930864964cbc3e1c9bff32193fb8c248
ok   PUT of other bytes as e2e-put-20261109161811-25900 1.0.0-1 amd64 to jammy/main: 409, index: tag "1.0.0-1__jammy_2fmain_2famd64" already points at sha256:0bfafbc6d84febf324cc9e55f1cc8580930864964cbc3e1c9bff32193fb8c248
ok   PUT of other bytes as e2e-put-20261109161811-25900 1.0.0-1 amd64 to noble/main: 409, index: conflict: pool file name e2e-put-20261109161811-25900_1.0.0-1_amd64.deb is held by version 1.0.0-1 in jammy/main with another content; a Debian pool holds one content per file name
ok   DELETE /api/v1/repos/apt/ubuntu/packages/{e2e-put-20261109161811-25900,e2e-oras-20261109161811-25900}/versions/1.0.0-1: 204; the Packages lists neither; purged, apt-get update, apt-cache show finds neither
PASS apt e2e against http://localhost:8080/apt/ubuntu/
```

A run that fails before its last step leaves its two packages in the repo.

## Clean up

A version goes with `DELETE` on the API, every suite it is in at once
([api.md](../api.md#delete-a-version-delete-apiv1reposformatnamepackagespackageversionsversion)):
`204`, and the next `Packages` and `apt-get update` no longer have it:

```sh
curl -u "$u" -X DELETE http://localhost:8080/api/v1/repos/apt/ubuntu/packages/world/versions/2.0-1
curl -u "$u" -X DELETE http://localhost:8080/api/v1/repos/apt/ubuntu/packages/hello/versions/1.0.0-1
```

Everything, the repo's keys included:

```sh
make dev-down RESET=1   # the registries forget what they hold
rm lajstrom.db          # the index, and with it the repo's keys
rm -r hello other world hello_1.0.0-1_amd64.deb world_2.0-1_amd64.deb metadata.json apt-token .oras-auth.json
```

The last line removes what this page made in the checkout; `deploy/compose/lajstrom.yaml` too, if
you made it for the end-to-end script.
