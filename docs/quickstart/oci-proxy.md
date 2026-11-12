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
   that ask for it meanwhile, on any replica, get a stream of their own from the upstream, with
   the same check of the digest, and nothing of it is stored: containerd gives up on a response
   whose headers take 30 s. `upstreams[].maxConcurrentTransits` caps those streams per blob and
   replica; `0` makes them wait for the copy and be served from the backend. A layer the backend holds
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
([the Talos guide](../guides/talos.md) has the rest: credentials, `skipFallback`, installers and
boot assets):

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
nodes; otherwise give containerd a username and a token of a user with `read` on the repo
(`machine.registries.config.<host>.auth`), not an `identityToken` alone
([#908](https://github.com/lajstrom/lajstrom/issues/908)).

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

## 5. A poor link to the upstream

What a repo does when the link to its upstream is blocked, slow, drops or hangs, and what the
operator does about it. Each row says whether it was measured or read in the code. The
measurements are of 2026-11-12, on the head of #869 (91a980a9), with the defaults
`timeout: 30s` and `retries: 2`:

- registry:3.1.2 as the upstream and as the backend, and a layer of 104,866,195 bytes;
- in front of the upstream, nginx with `proxy_buffering on; proxy_max_temp_file_size 0;
  limit_rate 2m;` (1.57 MB/s measured; `limit_rate` does nothing with `proxy_buffering off`), or
  a proxy that injects faults;
- curl and crane as the clients.

| The link | What happens | What to do |
| --- | --- | --- |
| **Slow** (1.57 MB/s) | Measured. The first client streams through the tee (§2, point 3) in 70.1 s, against 66.9 s straight from nginx; the upstream gets one `GET`. A client that asked 10 s later got its headers when the copy was committed, after 60.1 s, and then the layer from the backend in 0.1 s. A client that waits 30 s for headers gets nothing, as containerd does ([#875]). | Warm the cache before the rollout (§5.1). |
| **Drops mid-blob** (a TCP reset 20 s into the body, at 2 MB/s) | Measured. The client that held the claim got `200` and 40.0 of 104.9 MB, then its connection closed. Clients that waited on the claim, and one that sent `Range`, got `500` ([#884]). The claim was gone within a second; the backend's upload session was cancelled, with no bytes left. The next request fetched from byte 0, without `Range`: four attempts got 40.0 MB each, and none completed ([#885]). With a reset every 60 s, the layer came in 52.5 s. On a link that drops every N s at R bytes/s, a layer above N × R never comes. | Warm the cache over a link that holds (§5.1), or send Lajstrom's egress through a proxy on a stable line (§5.3). |
| **Sends part of a blob, then nothing**, the connection open | Measured. Unbounded: the fetch and its claim stayed until the upstream's side closed the connection, 292 s later. The claim kept beating, so another replica waits as long, and later requests for the blob wait until their client gives up. `timeout: 5s` does not change it: the timeout bounds the wait for headers, not a body that stops ([#883]). When the upstream's host was stopped mid-blob behind nginx, nginx's `proxy_read_timeout` (60 s) ended the wait, and the rest went as in the row above. | A proxy in the path with a read timeout; otherwise restart the replica. |
| **Accepts the connection, never answers** | Measured. A blob, or a tag never seen, is `504` after `timeout` × (`retries` + 1): 90.5 s for either with the defaults, 5.0 s for a tag with `timeout: 5s, retries: -1`. The claim is held meanwhile, and a client that asks meanwhile gets the same `504`. A cached tag whose row expired is served from the row, but only after the same 90.6 s, longer than containerd waits for headers. | Keep `timeout` × (`retries` + 1) under 30 s, and a longer `metadataTTL` (§5.4). |
| **Blocked, packets dropped** (no route, a firewall that drops) | Measured. `504` after 30.7 s: three connects of 10 s. | Allow the hosts (§5.2), or use an egress proxy (§5.3). |
| **Blocked, refused, or the name does not resolve** | Measured. `502` after 0.6 s (refused, three attempts) or 0.04 s (DNS, not retried). | The same. |
| **The blob CDN blocked, the registry reachable** | Measured on registry.k8s.io, with an egress proxy that resets every `CONNECT` to `cdn.registry.k8s.io`. Tags and manifests answer; every blob is `502` after 0.5 to 0.8 s (three attempts). The log says `upstream k8s: … (the connection was reset)` and does not name the CDN's host. The circuit breaker stayed closed: in the code, the registry's redirect, each blob's first request, counts as an answer. | Allow the CDN's hosts (§5.2); the egress proxy's log names the host. |
| **Down entirely** | Measured, the upstream refusing connections. What the backend holds is served, and a tag from its row: at once while the row is fresh, and with `proxy: the upstream did not answer for a tag; the cached digest is served` at warn once it has expired. A blob that is listed but not held, and a tag never seen, are `502` (`504` on timeouts). After five failed requests, `502` at once for 30 s (§5.4). | Warm what the rollout needs; `offline: true` while the upstream is gone (§5.4). |

### 5.1 Warm the cache before a rollout

Before the nodes pull, pull every image and platform the rollout needs through Lajstrom, from
one host. The backend then holds them, and the nodes no longer depend on the link:

```sh
# images.txt: one image a line, as the repo names it (nginx:1.27, bitnami/redis:7.4)
while read -r image; do
  for platform in linux/amd64 linux/arm64; do
    crane pull --platform "$platform" "lajstrom.example.com/oci/hub/$image" /dev/null
  done
done < images.txt
```

- Measured: after `crane pull --platform … /dev/null` of an image, the backend held its tag and
  its keep-tag.
- Warm with crane, which keeps no layers between runs. docker and containerd skip the layers they
  hold already, and an image whose layers were skipped is never completed in the backend
  ([#876]).
- Each layer comes in one upstream `GET`. On a link that drops, run a failed pull again: it
  starts that layer from byte 0 ([#885]).
- On Docker Hub, a cold image costs a counted pull for its index and one for each platform
  ([operations.md](../operations.md#upstreams-of-oci-proxy-repos)).
- Through the rollout, a longer `metadataTTL`, or `offline: true`, keeps the tags off the
  upstream (§5.4).
- [#886] proposes `lajstrom admin repo prefetch` for `oci` repos, which would do this in the
  server.

### 5.2 The hosts to allow

An egress firewall or an allowlisting proxy must let Lajstrom reach:

- the registry;
- its token service;
- the hosts that its blobs redirect to, and on registry.k8s.io its manifests.

Measured on 2026-11-12 from one address in Europe, anonymously: a token, then a blob `GET`
without following the redirect, and repeated with the same hosts:

| Upstream | Registry | Token | Redirects |
| --- | --- | --- | --- |
| Docker Hub | `registry-1.docker.io` | `auth.docker.io` | blobs: `production.cloudfront.docker.com` |
| ghcr.io | `ghcr.io` | `ghcr.io` | blobs: `pkg-containers.githubusercontent.com` |
| quay.io | `quay.io` | `quay.io` | blobs: `cdn01.quay.io` |
| registry.k8s.io | `registry.k8s.io` | none | manifests: `europe-west8-docker.pkg.dev`; blobs: `cdn.registry.k8s.io` |

- These are one address's hosts on one day. A vendor may change its CDN, and registry.k8s.io's
  manifest host is a regional one (`europe-west8` from here). The hosts your egress proxy logs
  (§5.3) are the list that counts.
- From the code (proxy-and-group.md §11.1): Lajstrom follows a redirect to another host only
  over https, to public addresses, and without the upstream's credentials. Behind an egress
  proxy it does not resolve the target itself, and the proxy enforces the rest.

### 5.3 An egress proxy for Lajstrom only

Lajstrom's requests to its upstreams read `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY` (code: Go's
`http.ProxyFromEnvironment` in the generic provider's transport). Set them in Lajstrom's
environment: the nodes reach only Lajstrom.

```yaml
# the Lajstrom container
env:
  - {name: HTTPS_PROXY, value: "http://egress.internal:3128"}
  - {name: NO_PROXY, value: "registry.internal,.svc,.cluster.local"}
```

- **Measured** with a `CONNECT` proxy that logs hosts, while Docker Hub's `hello-world` and
  registry.k8s.io's `pause:3.10` were pulled. The proxy saw `registry-1.docker.io`,
  `auth.docker.io`, `production.cloudfront.docker.com`, `registry.k8s.io`,
  `europe-west8-docker.pkg.dev` and `cdn.registry.k8s.io`, and none of the backend's requests,
  since the backend was in `NO_PROXY`.
- **The backend** reads the same variables. On its own, Go leaves out only `localhost` and
  loopback addresses, so put the backend's host name in `NO_PROXY`. Without it, the same run
  sent 34 requests to the backend through the proxy (the backend was on http, so they went
  through `HTTP_PROXY`).
- **A read timeout** on the proxy bounds a blob whose bytes stop coming ([#883]). nginx's
  `proxy_read_timeout` did so as a reverse proxy; an egress proxy's idle timeout on a `CONNECT`
  tunnel was not measured.

### 5.4 Timeouts, retries and the TTLs

- **`upstreams[].timeout`** (default `30s`) bounds each attempt: connecting (at most 10 s) and the
  wait for response headers.
- **`upstreams[].retries`** (default `2`) tries again after a timeout, a refused or reset
  connection, one closed without an answer, a `5xx` other than `501` and `505`, or a `429`. A
  name that does not resolve is not retried (measured).
- **The total.** A hung or blocked upstream therefore costs up to `timeout` × (`retries` + 1),
  plus less than a second of backoff. Measured: 90.5 s with the defaults, 30.7 s when the
  connects time out, and 5.0 s with `timeout: 5s, retries: -1`.
- **containerd waits 30 s for response headers** ([#875]): on a link that hangs, keep the total
  under that. A short timeout costs only an upstream that is slow to send its headers: a blob
  that keeps moving is never cut.
- **A body that stops** is not bounded by the timeout ([#883]).
- **The circuit breaker** opens after five failed requests in a row (measured):
  - for 30 s, everything asked of the upstream is `502` at once (0.05 s), without `Retry-After`;
  - expired tags are served from their rows at once;
  - then one request is the trial and waits the whole total. Another tag asked during the trial
    got `502` at once.
- **`metadataTTL`** (default `10m`): a tag whose row is fresh costs no request, so an outage or a
  hang does not touch it. A longer one through a rollout (`1h`, `24h`) keeps every warmed tag off
  the upstream, at the price of seeing a moved tag later.
- **`offline: true`** asks the upstream nothing (code, proxy-and-group.md §4.4). Tags come from
  their rows whatever their age, what the backend holds is served, and the rest is `404`. It
  suits a rollout after a warm-up while the link is bad.
- **`negativeTTL`** (default `2m`): a tag or a repository the upstream does not have is `404`,
  with no request, for that long. While the upstream fails, an expired negative row still answers
  `404` (measured 27 s after it was written, with `negativeTTL: 10s`). From the code: until the
  sweep deletes it, once a minute, and then `502`.

[#875]: https://github.com/lajstrom/lajstrom/issues/875
[#876]: https://github.com/lajstrom/lajstrom/issues/876
[#883]: https://github.com/lajstrom/lajstrom/issues/883
[#884]: https://github.com/lajstrom/lajstrom/issues/884
[#885]: https://github.com/lajstrom/lajstrom/issues/885
[#886]: https://github.com/lajstrom/lajstrom/issues/886

Groups of `oci` repos, one URL for a hosted and a proxy repo, are not in this version
([#848](https://github.com/lajstrom/lajstrom/issues/848)).
