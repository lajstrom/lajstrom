# Quick start: a pull-through cache of Docker Hub, ghcr.io and quay.io

An `oci` proxy repo serves `docker pull`, `crane pull` and containerd from an upstream registry
and keeps what it fetched in your backend registry: the next pull of a layer does not leave your
network, and the upstream's rate limits count once per manifest, not once per node. One repo
kind covers any registry of the distribution API; Docker Hub gets its `library/` names and its
rate limits handled on top. The rules are [proxy-and-group.md](../spec/proxy-and-group.md) §11
and [ADR 0018](../adr/0018-oci-proxy-repos-pull-through-from-any-registry.md).

## 1. Configure the upstreams

An upstream is the operator's: the configuration names its URL and its credentials, and a repo
refers to it by name ([configuration.md](../configuration.md#upstreams)).

```yaml
upstreams:
  # Docker Hub. Without credentials, 100 pulls per window per address (its docs say 6 hours,
  # its ratelimit-limit header said 1 hour in November 2026); a personal access token of a free
  # account raises it (docs.docker.com/docker-hub/usage/).
  - name: dockerhub
    format: oci
    url: "https://registry-1.docker.io"
    auth: {username: acme, passwordEnv: DOCKERHUB_TOKEN}
  - {name: ghcr, format: oci, url: "https://ghcr.io"}
  - {name: quay, format: oci, url: "https://quay.io"}
  - {name: k8s, format: oci, url: "https://registry.k8s.io"}
repos:
  - {name: hub, format: oci, registry: local, kind: proxy, anonymousRead: true, upstream: {name: dockerhub, metadataTTL: 1h}}
  - {name: ghcr, format: oci, registry: local, kind: proxy, anonymousRead: true, upstream: {name: ghcr}}
  - {name: quay, format: oci, registry: local, kind: proxy, anonymousRead: true, upstream: {name: quay}}
  - {name: k8s, format: oci, registry: local, kind: proxy, anonymousRead: true, upstream: {name: k8s}}
```

- `url` is the registry's root, without a path. Lajstrom answers the upstream's bearer challenge
  at its realm when the realm is on the upstream's host (ghcr.io, quay.io, registry.k8s.io) or
  on a host the vendor table knows (`auth.docker.io` for Docker Hub); a private registry whose
  token service is elsewhere lists it in `upstreams[].tokenRealmHosts`.
- `auth` takes a username with `passwordEnv` or `passwordFile`, or a static token in
  `tokenFile`. Without it the upstream is read anonymously.
- `metadataTTL` (default `10m`) is how long a tag is used without asking the upstream. A tag is
  then revalidated with a `HEAD`, which Docker Hub does not count.
- A repo is created from the configuration at start, or with
  `lajstrom admin repo create oci/hub --registry local --kind proxy --upstream '{"name": "dockerhub"}'`.

## 2. Pull

```sh
docker pull lajstrom.example.com/oci/hub/nginx:1.27              # Docker Hub's library/nginx
docker pull lajstrom.example.com/oci/hub/bitnami/redis:7.4
crane pull --platform linux/arm64 lajstrom.example.com/oci/ghcr/oras-project/oras:v1.3.0 oras.tar
```

`oci/hub/nginx` and `oci/hub/library/nginx` are one repository, kept once as `library/nginx`.
What happens on the first pull of a tag:

1. The tag is resolved upstream with a `HEAD`; the index of a multi-arch image is fetched once
   (a counted pull on Docker Hub) and kept in Lajstrom's index database.
2. The platform's manifest is fetched when the client asks for it (another counted pull), and
   only that platform's layers when the client asks for them: a pull of `linux/arm64` stores
   `linux/arm64`.
3. Each layer streams into the backend and, at the same time, to the client that asked first;
   its last byte waits until the backend has committed bytes of the right digest. Other clients
   that ask for it meanwhile wait and are served from the backend. A layer the backend holds
   under another repository of the repo (`kube-apiserver` and `kube-controller-manager` share
   most of theirs) is mounted instead, with no request to the upstream, even when the repo is
   offline; a backend that does not mount (Nexus's `docker` format, JFrog) gets it fetched.
4. A pulled platform is held whole, whatever layers the client asks for: containerd asks only
   for the layers it does not hold, so an image whose layers it has from another image would
   otherwise never be held whole. In the background:
   - a `GET` of a platform's manifest mounts the layers held under another repository of the
     repo, offline too, and fetches nothing: `crane manifest`, `skopeo inspect` or Renovate
     reading the config cost mounts only;
   - once a client asked for one of its layers, the layers still missing are fetched, once no
     client of the replica has asked for a layer of that repository for 10 seconds, at most 4 at
     a time per replica, within `maxFileSize`. A layer that two versions cached under one
     repository share does not say which one is pulled, and starts neither;
   - a `HEAD`, and the other platforms of an index, cost nothing.

   A layer that the backend holds nowhere and no client asks for, because the nodes got it
   elsewhere, stays out until a client asks for a layer of that platform.
5. Once a platform's layers are held, its manifest is pushed to the backend under a keep-tag
   `_keep.sha256.<hex>`, which `tags/list` does not show and garbage collection leaves alone;
   once every platform is held, the index too, and the upstream's tag where the backend lets
   tags move (on ECR's `IMMUTABLE` repositories, Quay or Harbor with tag immutability, or Zot
   with `capabilities.immutableTags: true`, tags stay in Lajstrom's index only).

The second pull of a layer makes no request to the upstream.

## 3. containerd and Talos

containerd uses Lajstrom as a mirror with `hosts.toml`
([hosts.md](https://github.com/containerd/containerd/blob/main/docs/hosts.md)); `override_path`
keeps the repo in the path, and containerd's `?ns=docker.io` is ignored:

```toml
# /etc/containerd/certs.d/docker.io/hosts.toml
server = "https://registry-1.docker.io"

[host."https://lajstrom.example.com/v2/oci/hub"]
  capabilities = ["pull", "resolve"]
  override_path = true
```

Talos takes the same in its machine configuration, one mirror per upstream registry
([#340](https://github.com/lajstrom/lajstrom/issues/340) tracks booting a cluster through it):

```yaml
machine:
  registries:
    mirrors:
      docker.io:
        endpoints: ["https://lajstrom.example.com/v2/oci/hub"]
        overridePath: true
      ghcr.io:
        endpoints: ["https://lajstrom.example.com/v2/oci/ghcr"]
        overridePath: true
      registry.k8s.io:
        endpoints: ["https://lajstrom.example.com/v2/oci/k8s"]
        overridePath: true
```

A repo with `anonymousRead: true` (and `auth.anonymousRead: true`) needs no credentials on the
nodes; otherwise give containerd a token of a user with `read` on the repo
(`machine.registries.config.<host>.auth`).

## 4. What to watch

- **Rate limits.** `lajstrom_proxy_upstream_ratelimit_remaining{upstream="dockerhub"}`, with
  `_limit` and `_window_seconds`, is what Docker Hub said last; a warning is logged below 10 %.
  A `429` serves the cached tag when there is one, otherwise the client gets `503
  TOOMANYREQUESTS` with Docker Hub's `Retry-After`. Every replica behind one egress address
  shares one anonymous budget ([operations.md](../operations.md#upstreams-of-oci-proxy-repos)).
- **Names the backend refuses.** GitLab behind its token service takes four components:
  `oci/hub/library/nginx` fits, `oci/ghcr/owner/img/sub` does not (`400 NAME_INVALID`). A
  Quay backend with [the build rule](../registries/quay.md#the-build-rule) refuses Docker Hub's
  `<namespace>/build` images.
- **A repository that does not exist** upstream is a `404`, remembered for `negativeTTL`
  (default `2m`): Docker Hub answers it `401`, which Lajstrom reads as "not there". A credential
  that lost access to a private repository looks the same.
- **What a pull cost.** `lajstrom_proxy_fetches_total` counts by `result` the layers fetched for
  a client (`fetched`), mounted from another repository of the repo (`mounted`), and fetched to
  hold a platform whole though no client asked for them (`completed`).
- **The audit log** has a `proxy.fetch` row for each manifest pushed to the backend, with the
  upstream, the digest and the tag, and a `proxy.reject` row for bytes that did not have their
  digest or a layer above `maxFileSize`.
- `DELETE /api/v1/repos/oci/hub/upstream-cache?name=library/nginx` drops what Lajstrom cached of
  one repository: its tags are asked again, and a refused tag push is tried once more.

Groups of `oci` repos, one URL for a hosted and a proxy repo, are not in this version
([#848](https://github.com/lajstrom/lajstrom/issues/848)).
