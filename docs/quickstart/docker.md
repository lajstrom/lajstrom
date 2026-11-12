# Quick start: docker and crane through an `oci` repo

Lajstrom answers the OCI distribution API at `https://<host>/v2/`, so `docker push`,
`docker pull`, buildx and crane talk to it as to a registry. An `oci` repo passes images through
unchanged: the manifests, the indexes and the blobs go to the backend registry, and Lajstrom adds
its users, roles and audit log. An image's name is `<host>/oci/<repo>/<image>`. This page pushes
and pulls images on the dev stack. [test/e2e/docker.sh](../../test/e2e/docker.sh) runs the same
clients, and cosign, in `make e2e`. The endpoints are specified in
[http-routing.md](../spec/http-routing.md) §2.

## 1. A repo and two users

Start the dev stack and the server as in the [oras quick start](oras.md) §1 (`make dev-up`, then
`make run`). Then make an `oci` repo on the Zot of the stack, the first admin, and a reader of the
repo for the pulls of a CI job:

```sh
cfg=deploy/compose/lajstrom.example.yaml
bin/lajstrom admin repo create oci/images --registry local --config $cfg
pw='a password of your own'
printf '%s' "$pw" | bin/lajstrom admin user create --name admin --admin --password-stdin --config $cfg
rpw='another password'
printf '%s' "$rpw" | bin/lajstrom admin user create --name ci-reader --password-stdin --config $cfg
bin/lajstrom admin binding create --principal user:ci-reader --role reader --mask oci/images --config $cfg
```

## 2. Log in

```sh
printf '%s' "$pw" | docker login -u admin --password-stdin localhost:8080   # Login Succeeded
```

The docker daemon speaks plain HTTP to `127.0.0.0/8` by itself, so the dev stack needs no
`insecure-registries`. A server elsewhere needs TLS ([reverse-proxy.md](../reverse-proxy.md)).
`docker login` sends the password once, as Basic. `/v2/token` exchanges it for a bearer token of
20 minutes, with the actions the user may do on each repository
([auth.md](../auth.md#bearer-tokens-on-v2)). A CI job logs in with a deploy token instead:
`docker login -u deploy --password-stdin` ([auth.md](../auth.md#in-ci)).

## 3. Push and pull an image

```sh
mkdir -p ctx && printf 'hello from lajstrom\n' > ctx/hello.txt
printf 'FROM scratch\nCOPY hello.txt /hello.txt\n' > ctx/Dockerfile
docker build -t localhost:8080/oci/images/hello:1 ctx
docker push localhost:8080/oci/images/hello:1
```

```text
The push refers to repository [localhost:8080/oci/images/hello]
44136fa355b3: Pushed
b6072d41ee64: Pushed
21d2fb7f238c: Pushed
1: digest: sha256:b3b35f9a39e7beb8f3b854daa2eef4cd0bde744f883beb51487a6d1299a3fac5 size: 855
```

```sh
docker rmi localhost:8080/oci/images/hello:1
docker pull localhost:8080/oci/images/hello:1
crane ls localhost:8080/oci/images/hello                  # 1
crane manifest localhost:8080/oci/images/hello:1
```

That was docker 29 with its containerd image store, which pushes an image index even for one
platform: the image of the build, and buildx's attestation manifest about it. Docker's classic
image store pushes the image manifest alone. Either way, what the backend holds is what docker
pushed, byte for byte, under the same digest:

```json
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.index.v1+json",
  "manifests": [
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "digest": "sha256:f3b216f24cc73acaa53c2173e776180054fa772f7b35461de68f42a1dce05cce",
      "size": 476,
      "platform": { "architecture": "arm64", "os": "linux" }
    },
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "digest": "sha256:359799a0fd46a6818ad777817cc689ac05cebf994ae529fc25ca36a305acbf16",
      "size": 835,
      "annotations": {
        "vnd.docker.reference.digest": "sha256:f3b216f24cc73acaa53c2173e776180054fa772f7b35461de68f42a1dce05cce",
        "vnd.docker.reference.type": "attestation-manifest"
      },
      "platform": { "architecture": "unknown", "os": "unknown" }
    }
  ]
}
```

Every manifest pushed has its row in the audit log ([api.md](../api.md)), with the user, the OCI
name, the digest and, for a push by tag, the tag. The attestation is a referrer of the image, so
its subject is the image's digest:

```sh
curl -s -u "admin:$pw" 'http://localhost:8080/api/v1/audit?action=push&repo=oci/images' |
  jq -c '.events[] | {principalName, subject, digest, tag: .details.tag, state}'
```

```text
{"principalName":"admin","subject":"oci/images/hello","digest":"sha256:b3b35f9a39e7beb8f3b854daa2eef4cd0bde744f883beb51487a6d1299a3fac5","tag":"1","state":"ok"}
{"principalName":"admin","subject":"sha256:f3b216f24cc73acaa53c2173e776180054fa772f7b35461de68f42a1dce05cce","digest":"sha256:359799a0fd46a6818ad777817cc689ac05cebf994ae529fc25ca36a305acbf16","tag":null,"state":"ok"}
{"principalName":"admin","subject":"oci/images/hello","digest":"sha256:f3b216f24cc73acaa53c2173e776180054fa772f7b35461de68f42a1dce05cce","tag":null,"state":"ok"}
```

A blob `GET` is answered with a `307` to the backend's object storage when the backend hands out
presigned URLs, so layers do not flow through Lajstrom ([oras.md](oras.md) §4).

## 4. A multi-arch image

The index of a multi-arch image is made by the client, and Lajstrom passes it through as it
passes any manifest. One build makes all platforms:

```sh
printf 'FROM scratch\nARG TARGETARCH\nCOPY hello-${TARGETARCH}.txt /hello.txt\n' > ctx/Dockerfile.multi
printf 'hello amd64\n' > ctx/hello-amd64.txt && printf 'hello arm64\n' > ctx/hello-arm64.txt
docker buildx build --platform linux/amd64,linux/arm64 -f ctx/Dockerfile.multi -t localhost:8080/oci/images/app:1 --push ctx
docker buildx imagetools inspect localhost:8080/oci/images/app:1
```

```text
Name:      localhost:8080/oci/images/app:1
MediaType: application/vnd.oci.image.index.v1+json
Digest:    sha256:f19d79f899920f17182013f698b440c4761fe97472bf40898278da49a635c0b9

Manifests:
  Name:        localhost:8080/oci/images/app:1@sha256:00c95e6f7d251c73c82e84fad4dcf472fa6b2245833fcdeee8449e457d4d6e81
  MediaType:   application/vnd.oci.image.manifest.v1+json
  Platform:    linux/amd64

  Name:        localhost:8080/oci/images/app:1@sha256:2e39b0ce9e29ae1473577eef16de37b18442b75b9c2746995e543ab664c2a12a
  MediaType:   application/vnd.oci.image.manifest.v1+json
  Platform:    linux/arm64

  Name:        localhost:8080/oci/images/app:1@sha256:add8d8c005d5ce7868057643e816323b17045e6b54a0aeb82739158fc266013c
  MediaType:   application/vnd.oci.image.manifest.v1+json
  Platform:    unknown/unknown
  Annotations:
    vnd.docker.reference.type:   attestation-manifest
    vnd.docker.reference.digest: sha256:00c95e6f7d251c73c82e84fad4dcf472fa6b2245833fcdeee8449e457d4d6e81

  Name:        localhost:8080/oci/images/app:1@sha256:df3a1380c51d915008f619ed6ffff3e2484fbbb1624574fa29e13c5285a26ea9
  MediaType:   application/vnd.oci.image.manifest.v1+json
  Platform:    unknown/unknown
  Annotations:
    vnd.docker.reference.digest: sha256:2e39b0ce9e29ae1473577eef16de37b18442b75b9c2746995e543ab664c2a12a
    vnd.docker.reference.type:   attestation-manifest
```

A build of several platforms needs a builder that can push them: docker's containerd image store,
as here, or a `docker-container` builder. A `docker-container` builder reaches a plain-HTTP
registry only with `--output type=image,push=true,registry.insecure=true`.

Native runners often build each platform alone, push it under a tag of its own, and a last job
makes the index. buildx and crane both do it:

```sh
for a in amd64 arm64; do   # on a runner of each platform, in CI
  docker buildx build --platform linux/$a --provenance=false -f ctx/Dockerfile.multi \
    -t localhost:8080/oci/images/app:2-$a --push ctx
done
docker buildx imagetools create -t localhost:8080/oci/images/app:2 \
  localhost:8080/oci/images/app:2-amd64 localhost:8080/oci/images/app:2-arm64
crane index append -t localhost:8080/oci/images/app:3 \
  -m localhost:8080/oci/images/app:2-amd64 -m localhost:8080/oci/images/app:2-arm64
docker pull --platform linux/arm64 localhost:8080/oci/images/app:3
```

```text
2026/11/12 22:34:10 base unspecified, using empty index
2026/11/12 22:34:10 existing manifest: sha256:00c95e6f7d251c73c82e84fad4dcf472fa6b2245833fcdeee8449e457d4d6e81
2026/11/12 22:34:10 existing manifest: sha256:2e39b0ce9e29ae1473577eef16de37b18442b75b9c2746995e543ab664c2a12a
2026/11/12 22:34:10 localhost:8080/oci/images/app:3: digest: sha256:462aa4494b580b931c185476d3fb5658d861520e488eb5049f791756e9476ecb size: 607
localhost:8080/oci/images/app@sha256:462aa4494b580b931c185476d3fb5658d861520e488eb5049f791756e9476ecb
```

Lajstrom does not make indexes itself: architecture.md §7 sketches a server-side stitching of
per-platform pushes, and whether to build it is open on
[#71](https://github.com/lajstrom/lajstrom/issues/71). The index a client made is a real object
in the backend: a pull by digest works around Lajstrom too, and it is what cosign signs.

## 5. Sign with cosign

cosign signs an image by its digest. The signature is a referrer of the index, which
`GET /v2/<name>/referrers/<digest>` lists ([oras.md](oras.md) §5):

```sh
cosign generate-key-pair
cosign sign --key cosign.key --allow-http-registry localhost:8080/oci/images/app@sha256:462aa4494b580b931c185476d3fb5658d861520e488eb5049f791756e9476ecb
cosign verify --key cosign.pub --allow-http-registry localhost:8080/oci/images/app@sha256:462aa4494b580b931c185476d3fb5658d861520e488eb5049f791756e9476ecb
```

`--allow-http-registry` is for the plain HTTP of the dev stack. Without a transparency log, as
`docker.sh` runs it offline, add `--tlog-upload=false --use-signing-config=false` to `sign` and
`--insecure-ignore-tlog=true` to `verify`.

## 6. Who may do what

The reader pulls, and its push is refused:

```sh
printf '%s' "$rpw" | docker login -u ci-reader --password-stdin localhost:8080
docker pull localhost:8080/oci/images/hello:1
docker tag localhost:8080/oci/images/hello:1 localhost:8080/oci/images/hello:2
docker push localhost:8080/oci/images/hello:2
```

```text
21d2fb7f238c: Layer already exists
44136fa355b3: Already exists
error from registry: the bearer token does not grant push on oci/images/hello
```

A client that has not logged in gets `401 Unauthorized`, unless the repo has `anonymousRead`
([auth.md](../auth.md)). A repo that the user may not read answers as one that does not exist.

## 7. What Lajstrom does not do

- **Make indexes.** The client does, as in section 4 (#71).
- **Freeze tags.** The tags of an `oci` repo are as mutable as the backend makes them:
  `docker push` of another image under `app:1` moves the tag
  ([guarantees.md](../spec/guarantees.md), INV-7). For immutable tags, turn on the backend's own
  policy: ECR's `IMMUTABLE` (Lajstrom's default for the repositories it creates there), or the
  tag rules of Harbor, Quay or Zot.
- **Audit pulls.** The audit log has pushes and deletes ([rbac.md](../spec/rbac.md) §5). The
  access log names the user of every request.
- **Cache Docker Hub.** A pull-through cache of Docker Hub, ghcr.io and quay.io is
  [#442](https://github.com/lajstrom/lajstrom/issues/442).

## 8. Clean up

```sh
docker logout localhost:8080
docker rmi localhost:8080/oci/images/hello:1 localhost:8080/oci/images/hello:2 localhost:8080/oci/images/app:3
rm -r ctx cosign.key cosign.pub
```

A manifest of an `oci` repo is deleted with `crane delete` or `oras manifest delete` by its digest,
which takes every tag that names it and its referrers. An index goes first: Zot keeps a manifest
that an index lists. `make dev-down` stops the stack, as in the [oras quick start](oras.md) §6.
