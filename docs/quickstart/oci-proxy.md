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
- An upstream on Amazon ECR takes `auth: {ecr: {}}`. Its credential is ECR's token for the AWS
  SDK's credentials (IRSA, the instance role, the environment), replaced before it expires, so
  nothing secret goes in the file and no restart is needed
  ([registries/ecr.md](../registries/ecr.md#ecr-as-an-upstream)).
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

The rows the fixes of [#875], [#883], [#884], [#885] and [#889] changed were measured again on the
same stand with those fixes (pull requests #891, #899, #915, #917 and #924, on top of #881), and
say what happens with them.

| The link | What happens | What to do |
| --- | --- | --- |
| **Slow** (1.57 MB/s; with the fixes, 2 MB/s) | Measured. The first client streams through the tee (§2, point 3) in 70.1 s, against 66.9 s straight from nginx. With the fixes, a client that asks while the copy runs gets a transit ([#875]): its first byte after 0.006 s (47.5 s before, past containerd's 30 s wait for headers), and the whole layer from a `GET` of its own, checked against the digest and stored nowhere. The upstream sends the layer once per such client; the backend gets one copy. A `Range` request still waits for the copy (45.4 s). | Warm the cache before the rollout (§5.1). `upstreams[].maxConcurrentTransits` caps the extra transfers per blob and replica; `0` makes clients wait for the copy, as before. |
| **Drops mid-blob** (a TCP reset 20 s into the body, at 2 MB/s) | Measured. The fetch resumes with `Range` from the byte it reached ([#885]): the 104.9 MB layer came in 52.5 s after two resumes, each a log line at info, 105 MB from the upstream in all. The claim holder got the whole layer in one response; a client that waited, and one that sent `Range`, were served from the backend once it was committed. Without the fix, every attempt got 40.0 MB and none completed. A fetch that cannot resume (the upstream answers the `Range` with `200`, or refuses the connection) ends: the waiting clients get `502` ([#884]), the claim holder's connection is aborted, and one warn line names the bytes it got. | Nothing while the upstream answers `Range`: the four public CDNs of §5.2 do (`206`). Otherwise warm the cache over a link that holds (§5.1), or send Lajstrom's egress through a proxy on a stable line (§5.3). |
| **Sends part of a blob, then nothing**, the connection open | Measured. A read that gets no byte for `timeout` ends the body ([#883]), and the fetch resumes with `Range` as on a drop. When the resume gets nothing either, the fetch ends with `504` and the claim is released: with `timeout: 5s, retries: -1`, the claim holder's connection was aborted at 5.1 s, and a client that waited got `504` 5 s after the fetch began; with the default `30s`, at 30.1 s. Without the fix, the fetch and its claim stayed until the upstream closed the connection, 292 s later. | Nothing; `timeout` sets how long a silent upstream is given. |
| **Accepts the connection, never answers** | Measured. A blob, or a tag never seen, is `504` after `timeout` × (`retries` + 1): 90.5 s for either with the defaults, 5.0 s for a tag with `timeout: 5s, retries: -1`. The claim is held meanwhile, and a client that asks meanwhile gets the same `504`. A cached tag whose row expired is served from the row, but only after the same 90.6 s, longer than containerd waits for headers. | Keep `timeout` × (`retries` + 1) under 30 s, and a longer `metadataTTL` (§5.4). |
| **Blocked, packets dropped** (no route, a firewall that drops) | Measured. `504` after 30.7 s: three connects of 10 s. | Allow the hosts (§5.2), or use an egress proxy (§5.3). |
| **Blocked, refused, or the name does not resolve** | Measured. `502` after 0.6 s (refused, three attempts) or 0.04 s (DNS, not retried). | The same. |
| **The blob CDN blocked, the registry reachable** | Measured on registry.k8s.io, with an egress proxy that resets every `CONNECT` to `cdn.registry.k8s.io`. Tags and manifests answer; every blob is `502` after 0.5 to 0.8 s (three attempts). The warn line `proxy: a blob not fetched from the upstream` names the CDN: `host=cdn.registry.k8s.io redirected=true`, and the error ends with `(at cdn.registry.k8s.io, where the upstream redirected the blob)` ([#889]). The circuit breaker stayed closed: in the code, the registry's redirect, each blob's first request, counts as an answer. | Allow the host the warn line names (§5.2). |
| **Down entirely** | Measured, the upstream refusing connections. What the backend holds is served, and a tag from its row: at once while the row is fresh, and with `proxy: the upstream did not answer for a tag; the cached digest is served` at warn once it has expired. A blob that is listed but not held, and a tag never seen, are `502` (`504` on timeouts). After five failed requests, `503` at once with `Retry-After` (30 at first, measured), until the circuit breaker lets a trial through (§5.4, [#889]). | Warm what the rollout needs; `offline: true` while the upstream is gone (§5.4). |

### 5.1 Warm the cache before a rollout

Before the nodes pull, prefetch every image and platform the rollout needs into the backend
([#886]; [api.md](../api.md#lajstrom-admin-repo)). The nodes then no longer depend on the link:

```sh
# images.txt: one image a line, as the cluster names them (docker.io/library/nginx:1.27, nginx:1.27)
kubectl get pods -A -o jsonpath='{range ..containers[*]}{.image}{"\n"}{end}' | sort -u > images.txt
lajstrom admin repo prefetch oci/hub --from-file images.txt --host docker.io \
  --platform linux/amd64,linux/arm64 --report report.json
```

- The server fetches each image the way a pull goes: the tag, the index, the manifest of each
  platform named, every layer under its claim. It answers once the manifests, the index (when
  every platform it lists is held) and the tag are in the backend.
- `--platform` is required for an image that is an index: without it, the image fails `400`
  and the message lists its platforms. A line may name its own platforms after the reference.
- An image whose layers another image of the repo holds is completed at its own path: the layers
  are mounted where the backend mounts, not fetched ([#876]).
- A second run prints `held` for each image and platform, and fetches nothing.
- An index fetched for some of its platforms stays in the cache with its tag, so `offline: true`
  serves it after the prefetch (§5.4).
- A layer that failed (a link that dropped beyond what the resumes recover, [#885]) fails its
  platform. `--failed` sends those images again:

  ```sh
  lajstrom admin repo prefetch oci/hub --from-file report.json --failed --report report.json
  ```

- On Docker Hub, a cold image costs a counted pull for its index and one for each platform
  ([operations.md](../operations.md#upstreams-of-oci-proxy-repos)): 50 images of two platforms
  are 150, above the anonymous 100 an hour. A `429` fails the image `503`, and the line says how
  long to wait before `--failed`.
- Through the rollout, a longer `metadataTTL`, or `offline: true`, keeps the tags off the
  upstream (§5.4).

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
- **A read timeout** on the proxy is not needed for a blob whose bytes stop coming: Lajstrom cuts
  it after `upstreams[].timeout` ([#883]).

### 5.4 Timeouts, retries and the TTLs

- **`upstreams[].timeout`** (default `30s`) bounds each attempt: connecting (at most 10 s), the
  wait for response headers, and every wait for the next bytes of a body ([#883]).
- **`upstreams[].retries`** (default `2`) tries again after a timeout, a refused or reset
  connection, one closed without an answer, a `5xx` other than `501` and `505`, or a `429`. A
  name that does not resolve is not retried (measured). On a blob whose body breaks, it also
  bounds the resumes in a row that get no byte; a resume after progress is not counted ([#885]).
- **The total.** A hung or blocked upstream therefore costs up to `timeout` × (`retries` + 1),
  plus less than a second of backoff. Measured: 90.5 s with the defaults, 30.7 s when the
  connects time out, and 5.0 s with `timeout: 5s, retries: -1`.
- **containerd waits 30 s for response headers** ([#875]): on a link that hangs, keep the total
  under that. A short timeout costs only an upstream that is slow to send its headers: a blob
  that keeps moving is never cut.
- **A body that stops** is cut after `timeout` without a byte, and resumed with `Range` ([#883],
  [#885]).
- **The circuit breaker** opens after five failed requests in a row (measured):
  - for 30 s, everything asked of the upstream is `503` at once with `Retry-After`, the seconds
    until the trial (measured: 30); before [#889] it was `502` without one;
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
[#889]: https://github.com/lajstrom/lajstrom/issues/889

## 6. One address for your images and Docker Hub: a group

A group of `oci` repos serves a hosted repo and a proxy under one URL: one login and one base in
CI for internal images and Docker Hub's, the pull side of a Nexus or Artifactory docker group.
Each repository path is served by exactly one member, which the group's routes choose; that
member answers everything for the path, as at its own URL, and nothing is written through the
group ([proxy-and-group.md](../spec/proxy-and-group.md) §12,
[ADR 0019](../adr/0019-oci-groups-one-member-per-repository-path.md)).

```yaml
repos:
  - {name: internal, format: oci, registry: local}
  - {name: hub, format: oci, registry: local, kind: proxy, upstream: {name: dockerhub, deny: ["acme/*"]}}
  - name: all
    format: oci
    kind: group
    anonymousRead: true
    group:
      members: [internal, hub]
      paths:
        internal: ["acme/*", "acme"]   # every internal path, and acme itself if it is a repository
        hub: ["*"]                     # the rest goes to Docker Hub
```

```sh
docker login lajstrom.example.com
docker pull lajstrom.example.com/oci/all/acme/api:1.4    # from internal
docker pull lajstrom.example.com/oci/all/nginx:1.27      # Docker Hub's library/nginx, through hub
docker push lajstrom.example.com/oci/internal/acme/api:1.5   # pushes go to the member's own URL
```

- **Routes.** Each member lists the paths it serves: a path (`acme`), a prefix and `/*`
  (`acme/*`: every path below `acme`, not `acme` itself), or `*`. The most specific pattern wins:
  the path itself, then the longest prefix, then `*`, so `acme/*` and `*` live together. The same
  pattern on two members is refused: `400` on the API, and `serve` does not start with it in the
  configuration. A route is decided from the group's settings alone, the same on every replica.
- **Route every internal prefix to the hosted member.** A path a route sends to the proxy is asked
  of Docker Hub: without `acme/*` on `internal`, `oci/all/acme/api` would go to `hub`. The
  proxy's `deny` still guards its own URL.
- **No fallback.** The routed member's answer is the group's: a tag `internal` does not have is
  `404`, never asked of Docker Hub, and a `5xx` of `hub` is not answered by `internal`. An index
  and its platforms, a manifest and its layers, always come from one member. A path no route
  selects, or one the proxy's lists refuse, is `404 NAME_UNKNOWN`.
- **Read-only.** A push, an upload or a delete through the group is `405 UNSUPPORTED` with
  `Allow: GET, HEAD`; `docker push` stops at the first upload. Push to the member's own URL, a
  signature for a mirrored image to `oci/hub/…` with `write` on the proxy: it is then seen through
  the group.
- **Who may pull.** Read on the group is the whole permission: bind readers on `oci/all`, and they
  need nothing on `internal` or `hub`. `/v2/token` grants `pull` alone on the group's paths. Only
  the admin role changes `members` and `paths`, since they decide what the group's readers get.
- **Several hosted members** may share a group, each with its own prefixes (`team-a/*`,
  `team-b/*`).
- `tags/list` through the group is the routed member's, named after the group's path, and the
  catalog lists to the group's readers the repositories each member holds at paths routed to it.
  A proxy member's fetches through the group are its `proxy.fetch` and `proxy.reject` rows, with
  `details.via` the group.

**containerd and Talos.** Point the `docker.io` mirror at the group instead of `hub`:

```toml
# /etc/containerd/certs.d/docker.io/hosts.toml
server = "https://registry-1.docker.io"

[host."https://lajstrom.example.com/v2/oci/all"]
  capabilities = ["pull", "resolve"]
  override_path = true
```

For `nginx:1.27` containerd asks `/v2/oci/all/library/nginx/manifests/1.27?ns=docker.io`: the
route sees `library/nginx`, `?ns=` is ignored, and `hub`'s `*` serves it. A pod's
`image: acme/api:1.4`, which is `docker.io/acme/api`, gets the internal image: the routes send
`acme/*` to `internal`, never to Docker Hub. Talos takes the same endpoint for `docker.io`, with
`overridePath: true`. Keep each other registry mirrored at its own proxy (`ghcr.io` at
`/v2/oci/ghcr`, as in section 3): the routes do not know which registry a path is from, and a
`ghcr.io` mirror pointed at the group would send its paths to Docker Hub.
