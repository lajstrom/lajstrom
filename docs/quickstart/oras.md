# Quick start: oras through the OCI facade

Lajstrom answers the OCI distribution API at `https://<host>/v2/`, so `oras`, `crane` and the
other OCI clients talk to it as to a registry, with names `<format>/<repo>/<path>`. The bytes go
through to the backend registry and are never kept by Lajstrom. This page pushes and pulls an
artifact on the dev stack in a few minutes. The endpoints are specified in
[http-routing.md](../spec/http-routing.md) §2.

## 1. Start the dev stack and make two repos

You need Go, Docker and `curl`; `crane` is optional. oras runs from its image, so it need not
be installed.

```sh
make dev-up                                           # Zot, distribution and Postgres: docs/dev-stack.md
make run                                              # serves deploy/compose/lajstrom.example.yaml on 127.0.0.1:8080
```

In another terminal, once `make run` has logged `listening` (`serve` creates the tables of
`lajstrom.db`; before that, `admin` fails with `the index database is not migrated`), make a repo `oci/images` and a
repo `raw/assets` on the Zot of the stack,
and the first user, an admin, with a password of your own (8 bytes at least):

```sh
bin/lajstrom admin repo create oci/images --registry local --config deploy/compose/lajstrom.example.yaml
bin/lajstrom admin repo create raw/assets --registry local --config deploy/compose/lajstrom.example.yaml
pw='a password of your own'
printf '%s' "$pw" | bin/lajstrom admin user create --name admin --admin --password-stdin --config deploy/compose/lajstrom.example.yaml
```

`oci` is not a plugin: it is the format of the pass-through repos, which hold any OCI artifact
as it is pushed and index nothing beyond the repository. `raw` is a format Lajstrom manages: its
manifests follow the [manifest profile](../spec/manifest-profile.md) and are indexed.

oras from its image, on OrbStack or Linux, where `--network host` reaches `localhost:8080`;
the dev stack is plain HTTP, hence `--plain-http`:

```sh
oras() { docker run --rm -i --network host -v "$PWD:/w" -w /w ghcr.io/oras-project/oras:v1.3.6 "$@" --registry-config /w/.oras-auth.json; }
printf '%s\n' "$pw" | oras login --plain-http -u admin --password-stdin localhost:8080   # Login Succeeded
printf 'hello facade\n' > file.txt
```

Nothing is open: `GET /v2/` without credentials is `401` with a `Bearer` challenge, and oras
answers it with the login, which `/v2/token` exchanges for a token of 20 minutes with the
actions the admin may do ([auth.md](../auth.md#bearer-tokens-on-v2)). The login lives in
`.oras-auth.json` here; `rm .oras-auth.json` ends it. crane, if you use it, logs in once with
`crane auth login localhost:8080 -u admin -p "$pw"`.

## 2. Push, pull, list on an `oci` repo

```sh
oras push --plain-http --artifact-type application/vnd.example.notes localhost:8080/oci/images/hello:1 file.txt
```

```text
Preparing file.txt
Uploading 7f4b67b5c68d file.txt
Uploading 44136fa355b3 application/vnd.oci.empty.v1+json
Uploaded  7f4b67b5c68d file.txt
Uploaded  44136fa355b3 application/vnd.oci.empty.v1+json
Uploading 147b5c0b3273 application/vnd.oci.image.manifest.v1+json
Uploaded  147b5c0b3273 application/vnd.oci.image.manifest.v1+json
Pushed [registry] localhost:8080/oci/images/hello:1
ArtifactType: application/vnd.example.notes
Digest: sha256:147b5c0b3273d42c5a4b50e99febafa877268f3bd0063bc5b5e025ddbe857feb
```

The repository is `oci/images/hello` in Zot too: the OCI name below the registry's instance
prefix is the name the client used. A second push of the same file finds the blobs (`Exists`)
and uploads only the manifest, a new one by its `created` time, and the tag `1` moves to it: the
tags of an `oci` repo are as mutable as the backend makes them
([guarantees.md](../spec/guarantees.md)), unlike those of a managed repo (section 3).

```sh
oras pull --plain-http -o out localhost:8080/oci/images/hello:1   # writes out/file.txt
oras manifest fetch --plain-http localhost:8080/oci/images/hello:1
oras repo tags --plain-http localhost:8080/oci/images/hello        # 1
crane ls localhost:8080/oci/images/hello                           # 1
crane catalog localhost:8080                                       # oci/images/hello, …
```

Tags of an `oci` repo come from the backend; `_catalog` lists the repositories of every repo
you may read, from the index (`oci_repos`), so a manifest pushed to an `oci` repo records its
repository there.

## 3. A file in a `raw` repo, through ingest

A manifest pushed to a repo Lajstrom manages goes through the manifest hook: the facade checks
that it is an OCI image manifest whose layers the backend has, and hands it to ingest, the
pipeline behind `curl -T` on the raw frontend too ([raw.md](raw.md)): the manifest profile, the
raw plugin, the claim on the tag, the push, the index. The client sets what the frontend would
set itself ([manifest-profile.md](../spec/manifest-profile.md) §1, §3, §4):

- `--artifact-type application/vnd.lajstrom.raw.v1`, and the media type of the file layer,
  `application/vnd.lajstrom.raw.file.v1`, after the file name;
- `lajstrom.schema=1`, `lajstrom.format=raw`, `lajstrom.repo=<repo>`;
- `lajstrom.name` and `lajstrom.raw.path`: the path of the file in the repo, which is also the
  repository below `raw/<repo>/` (lower case, escaped) and the title of the layer;
- `lajstrom.version`: the first 16 hex digits of the file's sha256, which is also the tag;
- `org.opencontainers.image.created`: oras sets it to the time of the push when the flag is
  left out.

oras titles a layer with the path as the command line gives it, so push from a directory
layout: `docs/hello.txt` is titled `docs/hello.txt`, the path, where a bare `hello.txt` would be
titled `hello.txt` and refused.

```sh
mkdir -p docs && printf 'hello lajstrom\n' > docs/hello.txt
v=$(sha256sum docs/hello.txt | cut -c1-16)    # ec5ba820a095a4d6; shasum -a 256 where sha256sum is missing
oras push --plain-http --artifact-type application/vnd.lajstrom.raw.v1 \
  --annotation lajstrom.schema=1 --annotation lajstrom.format=raw --annotation lajstrom.repo=assets \
  --annotation lajstrom.name=docs/hello.txt --annotation lajstrom.version=$v \
  --annotation lajstrom.raw.path=docs/hello.txt \
  --annotation org.opencontainers.image.created=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
  localhost:8080/raw/assets/docs/hello.txt:$v docs/hello.txt:application/vnd.lajstrom.raw.file.v1
```

```text
Preparing docs/hello.txt
Uploading ec5ba820a095 docs/hello.txt
Uploading 44136fa355b3 application/vnd.oci.empty.v1+json
Uploaded  ec5ba820a095 docs/hello.txt
Uploaded  44136fa355b3 application/vnd.oci.empty.v1+json
Uploading 6ce3072bb497 application/vnd.oci.image.manifest.v1+json
Uploaded  6ce3072bb497 application/vnd.oci.image.manifest.v1+json
Pushed [registry] localhost:8080/raw/assets/docs/hello.txt:ec5ba820a095a4d6
ArtifactType: application/vnd.lajstrom.raw.v1
Digest: sha256:6ce3072bb497dcc8dd9094dfd89e6b6dcb184f1c215914aded55e63cd0adc872
```

The manifest digest changes with the `created` time. The file is indexed now, the raw frontend
serves it, and its tags come from the index:

```sh
curl -u "admin:$pw" http://localhost:8080/raw/assets/docs/hello.txt   # hello lajstrom
oras repo tags --plain-http localhost:8080/raw/assets/docs/hello.txt       # ec5ba820a095a4d6
oras pull --plain-http -o out localhost:8080/raw/assets/docs/hello.txt:$v  # writes out/docs/hello.txt
```

Tags are immutable. Pushing the same manifest again is the idempotent `201`, but the same
command run again builds another manifest, if only by its `created` time, and through the facade
another manifest under a tag is a `409` naming the one the tag holds (manifest-profile.md §4.2;
the raw frontend's `PUT` takes the same file as the repeat instead):

```text
Preparing docs/hello.txt
Exists    ec5ba820a095 docs/hello.txt
Exists    44136fa355b3 application/vnd.oci.empty.v1+json
Uploading 5c493b2b3cc6 application/vnd.oci.image.manifest.v1+json
Error response from registry: tag invalid: tag ec5ba820a095a4d6 of raw/assets/docs/hello.txt is taken by sha256:6ce3072bb497dcc8dd9094dfd89e6b6dcb184f1c215914aded55e63cd0adc872: tags are immutable: map[existing:sha256:6ce3072bb497dcc8dd9094dfd89e6b6dcb184f1c215914aded55e63cd0adc872]
```

Other bytes are another version under a tag of their own: compute `v` again and push. Under the
old tag they are refused, since the version is not theirs:

```text
Error response from registry: manifest invalid: the manifest is not a valid raw artifact for raw/assets/docs/hello.txt: ingest: raw plugin: lajstrom.version "ec5ba820a095a4d6" is not the first 16 hex digits of the file's sha256 d9a4c6676a62cb3b8ca0b8459ab341837cdba8543316c8574b454ccc24d4c690
```

A missing annotation is a `400` that names it, here the push without
`--annotation lajstrom.repo=assets`:

```text
Error response from registry: manifest invalid: the manifest is not a valid raw artifact for raw/assets/docs/hello.txt: profile: invalid annotation: lajstrom.repo is missing
```

Without `--artifact-type` oras sends its default, `application/vnd.unknown.artifact.v1`, and the
answer says what to pass:

```text
Error response from registry: manifest invalid: the manifest is not a valid raw artifact for raw/assets/docs/hello.txt; pass --artifact-type application/vnd.lajstrom.raw.v1: profile: invalid artifactType: "application/vnd.unknown.artifact.v1" is not application/vnd.lajstrom.<type>.v1; profile: invalid layer title: layer 0: "docs/hello.txt" is not a file name
```

A push without a tag (`localhost:8080/raw/assets/docs/hello.txt`, no `:$v`) goes by digest: the
hook takes the tag from `lajstrom.version` (and `lajstrom.variant` in the formats that have one),
and the version is indexed all the same; without `lajstrom.version` it is a `400` that asks for a
tag. A layer the backend does not have is `400 MANIFEST_BLOB_UNKNOWN` with its digest.

Tags of a managed repo come from the index, not from the backend: the versions of the package
whose escaped path is the name below the repo.

## 4. What happens to an upload

A blob upload is the backend's own upload session, driven by Lajstrom: `POST …/blobs/uploads/`
opens one in the backend and answers a `Location` with an id of Lajstrom's; that id is the key
of a row in the index that holds the backend's session, so the next `PATCH` or `PUT` may land on
any replica behind the load balancer. Every body streams from the client to the backend. A
single `POST ?digest=` with the whole blob works too, and so does a mount
(`?mount=<digest>&from=<name>`) from a repo you may read.

A blob `GET` passes a `Range` to the backend and answers `307` to the backend's object storage
when it hands out a presigned URL (the `s3` profile of the dev stack; `policy.blobRedirect:
never` on a repo always streams).

## 5. Referrers and deletes

An artifact about another one, a signature, an SBOM or a note, names it as its `subject`, and
`GET …/referrers/<digest>` lists them, on both kinds of repo: on a managed one any artifact type
but `application/vnd.lajstrom.*`, pushed by digest, as `oras attach` does
([http-routing.md](../spec/http-routing.md) §2):

```sh
printf 'a note\n' > note.txt
oras attach --plain-http --artifact-type application/vnd.example.note localhost:8080/raw/assets/docs/hello.txt:$v note.txt
oras discover --plain-http localhost:8080/raw/assets/docs/hello.txt:$v      # the note, below the manifest
```

A delete needs `delete` on the repo. On a managed repo it deletes the version, its referrers
first, as `DELETE /api/v1/…/versions/<version>` does ([api.md](../api.md)); on an `oci` repo the
manifest, and by digest every tag that names it:

```sh
oras manifest delete --plain-http --force localhost:8080/raw/assets/docs/hello.txt:$v   # Deleted
oras manifest delete --plain-http --force localhost:8080/oci/images/hello:1             # Deleted
curl -s -o /dev/null -w '%{http_code}\n' -u "admin:$pw" http://localhost:8080/raw/assets/docs/hello.txt   # 404
```

A registry that cannot delete manifests answers `405` and nothing changes.

## 6. Clean up

```sh
make dev-down          # RESET=1 also drops what the registries hold
rm lajstrom.db         # the index: its repos and its admin
rm -r file.txt out docs/hello.txt note.txt .oras-auth.json
```

The last line removes what this page made in the checkout (`docs/hello.txt` beside the repo's
own docs); `crane auth logout localhost:8080` removes crane's login from
`~/.docker/config.json`.
