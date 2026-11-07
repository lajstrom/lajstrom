# Quick start: a raw repo

A raw repo stores any file under a path: `curl -T` puts it in, `curl` gets it out, and the file
is an OCI artifact in the registry behind Lajstrom. This page runs the M0 demo on your machine
in a few minutes. The endpoints are specified in [http-routing.md](../spec/http-routing.md) §3
(raw); what lands in the registry is the [manifest profile](../spec/manifest-profile.md).

## 1. Start the dev stack

You need Go, Docker, `curl` and `jq`; `crane` (or `oras`) to look into the registry is optional.

```sh
make dev-up                                           # Zot, distribution and Postgres: docs/dev-stack.md
make run                                              # builds bin/lajstrom, serves deploy/compose/lajstrom.example.yaml
curl -s http://localhost:8080/raw/scratch/            # {"entries":[]}
```

The example configuration uses SQLite (`lajstrom.db` in the current directory), the Zot at
`localhost:5000`, and creates the repo `raw/scratch` at startup, so there is nothing else to set
up. Everything is open on the dev stack: authentication comes with M2. Another repo is one call:
`bin/lajstrom admin repo create raw/assets --registry local --config deploy/compose/lajstrom.example.yaml`
(see [api.md](../api.md)). A repo can cap the file size with the policy `{"maxFileSize": <bytes>}`;
a larger upload is `413`.

In another terminal, set the base URL once:

```sh
base=http://localhost:8080/raw/scratch
printf 'hello raw\n' > file.txt
```

## 2. Upload a file (R-01)

```sh
curl -i -T file.txt -H 'Content-Type: text/plain' $base/docs/file.txt
```

```text
HTTP/1.1 201 Created
Content-Type: application/json
Docker-Content-Digest: sha256:9c1e…
Location: /raw/scratch/docs/file.txt

{"path":"docs/file.txt","version":"3011d31f64522770","digest":"sha256:9c1e…","size":10,"created":"2026-11-07T10:00:00Z"}
```

The path after `/raw/scratch/` is the package, exactly as sent: case is preserved, `/` makes
directories, a component may not be empty, `.` or `..`, and control bytes are refused (`400`).
A path is unique without regard to case (R-09): once `Docs/Readme.MD` exists, a `PUT` of
`docs/readme.md` is `409` naming it, because both are one OCI repository (section 8).
The version is the first 16 hex digits of the sha256 of the bytes; `digest` and
`Docker-Content-Digest` are the digest of the manifest in the registry. Without `-H
'Content-Type: …'` (curl sends none with `-T`) the file is served as `application/octet-stream`.

## 3. Download it

```sh
curl $base/docs/file.txt          # hello raw
curl -I $base/docs/file.txt       # the same headers, no body
```

```text
HTTP/1.1 200 OK
Accept-Ranges: bytes
Content-Length: 10
Content-Type: text/plain
Docker-Content-Digest: sha256:9c1e…
Etag: "sha256:3011d31f645227702f583789df2e751d3045b947a84606a7bd27781821774965"
Last-Modified: Sat, 07 Nov 2026 10:00:00 GMT
```

The `ETag` is the digest of the file, so `curl -H 'If-None-Match: "sha256:3011…"'` answers
`304`, and `curl -r 0-4` answers `206` with the first five bytes. When the registry hands out
download URLs (distribution on S3, for example), `GET` answers `307` to the blob instead.

## 4. Upload it again, then change it (R-02, R-03)

```sh
curl -s -T file.txt $base/docs/file.txt | jq -r .digest    # the same digest: 201, nothing new is stored
printf 'hello again\n' > file.txt
curl -s -T file.txt $base/docs/file.txt | jq -r .digest    # a new digest: a new version
curl $base/docs/file.txt                                   # hello again
curl "$base/docs/file.txt?digest=sha256:9c1e…"             # hello raw: ?digest= pins a version
```

A path resolves to its newest version, the most recently created one. Every version stays in
the registry under its own tag; tags are never moved. Uploading bytes that an older version
already holds is the idempotent repeat of that version, not a new one, so it does not become
the newest: to roll back, delete the newer version (section 7) or pin `?digest=`.

## 5. List (R-04)

```sh
curl -s $base/ | jq .
curl -s $base/docs/ | jq .
```

```json
{
  "entries": [
    {"path": "docs/file.txt", "size": 10, "digest": "sha256:9c1e…", "created": "2026-11-07T10:00:00Z"},
    {"path": "docs/file.txt", "size": 12, "digest": "sha256:5be0…", "created": "2026-11-07T10:01:00Z"}
  ]
}
```

A path that ends with `/` is a listing of everything below it, at any depth: one entry per
version, sorted by path and then `created`, with the manifest digest that `?digest=` takes. A
prefix that holds no file is `404`.

## 6. An empty file (R-10)

```sh
: > empty
curl -s -o /dev/null -w '%{http_code}\n' -T empty $base/empty    # 201
curl -sI $base/empty | grep -i content-length                    # Content-Length: 0
```

## 7. Delete

```sh
curl -i -X DELETE $base/docs/file.txt     # 204: every version of the path is gone
curl -i $base/docs/file.txt               # 404
```

The manifests are deleted in the registry first, then the index rows; the blobs are left to the
registry's garbage collection. A registry that cannot delete manifests (distribution without
`REGISTRY_STORAGE_DELETE_ENABLED=true`) answers `501` and nothing changes.

## 8. Look into the registry

Each path is an OCI repository below `raw/scratch/`, each version a tag. The repository path is
the path lower-cased and escaped ([manifest-profile.md](../spec/manifest-profile.md) §4.1); the
original spelling is in the annotations.

```sh
crane manifest --insecure localhost:5000/raw/scratch/docs/file.txt:3011d31f64522770 | jq .
```

```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "artifactType": "application/vnd.lajstrom.raw.v1",
  "config": {"mediaType": "application/vnd.oci.empty.v1+json", "digest": "sha256:44136fa…", "size": 2},
  "layers": [
    {"mediaType": "application/vnd.lajstrom.raw.file.v1", "digest": "sha256:3011d31…", "size": 10,
     "annotations": {"org.opencontainers.image.title": "docs/file.txt"}},
    {"mediaType": "application/vnd.lajstrom.raw.metadata.v1+json", "digest": "sha256:f3a0685…", "size": 151,
     "annotations": {"org.opencontainers.image.title": "metadata.json"}}
  ],
  "annotations": {
    "lajstrom.format": "raw",
    "lajstrom.name": "docs/file.txt",
    "lajstrom.raw.path": "docs/file.txt",
    "lajstrom.repo": "scratch",
    "lajstrom.schema": "1",
    "lajstrom.uploaded-by": "anonymous",
    "lajstrom.version": "3011d31f64522770",
    "org.opencontainers.image.created": "2026-11-07T10:00:00Z"
  }
}
```

Because the file layer's title is the path, `oras pull --plain-http
localhost:5000/raw/scratch/docs/file.txt:3011d31f64522770` writes `docs/file.txt` (R-08). The
metadata layer is `{"path", "contentType", "size", "sha256"}`; Lajstrom's index can be rebuilt
from the manifests alone.

## The end-to-end script

`test/e2e/raw.sh` runs all of the above with checks, against any Lajstrom with a writable raw
repo, and cleans up after itself:

```sh
test/e2e/raw.sh http://localhost:8080 scratch
```
