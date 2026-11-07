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
make run                                              # serves deploy/compose/lajstrom.example.yaml on :8080
```

In another terminal, make a repo `oci/images` and a repo `raw/assets` on the Zot of the stack:

```sh
bin/lajstrom admin repo create oci/images --registry local --config deploy/compose/lajstrom.example.yaml
bin/lajstrom admin repo create raw/assets --registry local --config deploy/compose/lajstrom.example.yaml
```

`oci` is not a plugin: it is the format of the pass-through repos, which hold any OCI artifact
as it is pushed and index nothing beyond the repository. `raw` is a format Lajstrom manages: its
manifests follow the [manifest profile](../spec/manifest-profile.md) and are indexed.

oras from its image, on OrbStack or Linux, where `--network host` reaches `localhost:8080`;
the dev stack is plain HTTP, hence `--plain-http`:

```sh
oras() { docker run --rm --network host -v "$PWD:/w" -w /w ghcr.io/oras-project/oras:v1.3.6 "$@"; }
printf 'hello facade\n' > file.txt
```

Everything is open on the dev stack: the token endpoint and RBAC come with #26 and #45, and
`GET /v2/` answers `200` until then.

## 2. Push, pull, list on an `oci` repo

```sh
oras push --plain-http --artifact-type application/vnd.example.notes localhost:8080/oci/images/hello:1 file.txt
```

```text
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
and uploads only the manifest.

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

## 3. A `raw` repo: blobs yes, the manifest after #25

```sh
oras push --plain-http localhost:8080/raw/assets/hello:1 file.txt
```

```text
Uploaded  44136fa355b3 application/vnd.oci.empty.v1+json
Uploaded  7f4b67b5c68d file.txt
Uploading 397da82632a3 application/vnd.oci.image.manifest.v1+json
Error response from registry: unsupported: pushing a manifest to a raw repo: the manifest hook comes with #25
```

The blobs are uploaded; the manifest `PUT` of a managed repo is `501 UNSUPPORTED` until the
manifest hook of #25 validates it against the manifest profile and hands it to ingest. Until
then a raw file goes in through its own frontend, and the facade reads it:

```sh
curl -T file.txt http://localhost:8080/raw/assets/docs/file.txt     # {"path":"docs/file.txt","version":"7f4b67b5c68dd2f5",…}
oras repo tags --plain-http localhost:8080/raw/assets/docs/file.txt # 7f4b67b5c68dd2f5
oras pull --plain-http -o out localhost:8080/raw/assets/docs/file.txt:7f4b67b5c68dd2f5
```

Tags of a managed repo come from the index, not from the backend: the versions of the package
whose escaped path is the name below the repo.

## 4. What happens to an upload

A blob upload is the backend's own upload session, driven by Lajstrom: `POST …/blobs/uploads/`
opens one in the backend and answers a `Location` with an id of Lajstrom's; that id is the key
of a row in the index that holds the backend's session, so the next `PATCH` or `PUT` may land on
any replica behind the load balancer. Every body streams from the client to the backend. A
single `POST ?digest=` with the whole blob works too, and so does a mount
(`?mount=<digest>&from=<name>`) from a repo you may read.

Not yet: a `Range` and the `307` to the backend's object storage on a blob `GET` (#27, the blob
is streamed whole), the referrers API (#28, `501`), deletes (#61, `405`).

## 5. Clean up

```sh
make dev-down          # RESET=1 also drops what the registries hold
```
