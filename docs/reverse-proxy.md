# Lajstrom behind a reverse proxy

Lajstrom listens on plain HTTP and expects an edge in front of it, an nginx, a Caddy, an
ingress or a Gateway, that terminates TLS for one public host
([http-routing.md](spec/http-routing.md) §1). Part 1 is what the edge must do for the
protocols to work; part 2 is how to limit request rates there, where volume limits belong by
decision ([ADR 0011](adr/0011-client-address-behind-a-proxy.md)): Lajstrom itself only
throttles what the edge cannot see, failed logins and the token endpoint (#130,
[auth.md](auth.md#failed-logins)).

Every configuration here was run on macOS with OrbStack, on 2026-11-09. nginx and Caddy stood in
front of the Lajstrom of the Compose example (#58, Postgres and distribution 3.1.2 with
htpasswd), over TLS. nginx is also the edge of the dev stack's `ha` profile, two replicas on
Postgres and distribution over plain HTTP on loopback
([dev-stack.md](dev-stack.md#the-ha-profile)), and `test/e2e/ha.sh` runs through it
([testing.md](testing.md#the-ha-stack)). Traefik and Envoy each ran in a container on the network
of that profile, in front of the same two replicas.

| Edge | Version | Configuration |
| --- | --- | --- |
| nginx | `nginx:1.30.6-alpine` | [deploy/nginx/lajstrom.conf](../deploy/nginx/lajstrom.conf); in the `ha` profile through [nginx.sed](../deploy/compose/ha/nginx.sed): the two replicas, port 80 without TLS |
| Caddy | `caddy:2.11.9-alpine` | [deploy/caddy/Caddyfile](../deploy/caddy/Caddyfile) |
| Traefik | `traefik:v3.7.17` | [below](#traefik) |
| Envoy | `envoyproxy/envoy:v1.40.2` | [below](#envoy) |

Through nginx and Caddy in front of the Compose example: a raw file and an `oras` artifact of 100 MiB pushed and pulled with the same
sha256; through nginx also an `oras` artifact of 30 layers, and the token limit below. The tests
used `https://localhost:<port>` with a certificate of a local CA (Caddy's internal CA), hence
`curl --cacert`, and `oras --ca-file` with `--plain-http=false`, since oras talks plain HTTP to
`localhost` unless told otherwise.

## Part 1: what the edge must do

| What | Why | nginx | Caddy |
| --- | --- | --- | --- |
| Terminate TLS, and reach Lajstrom over plain HTTP on a network only the edge can reach | Lajstrom has no TLS of its own yet (`server.tls.*` and `server.insecureHTTP` are #50); docker, pip and Maven want HTTPS | `listen 443 ssl` | automatic |
| Route the whole host, without a path prefix or rewriting | OCI clients always ask `https://<host>/v2/…`; Lajstrom owns the tree | `location /` | `reverse_proxy` |
| Set `server.externalURL` to the public URL, with the port if it is not 443 | the realm of the bearer challenge of `/v2` is `<externalURL>/v2/token` | | |
| Pass bodies of any size, unbuffered | a layer is gigabytes; nginx refuses more than 1 MiB by default and spools bodies to its disk | `client_max_body_size 0`, `proxy_request_buffering off` | the default |
| Stream responses | a blob download should not be spooled by the edge | `proxy_buffering off` | `flush_interval -1` |
| Time out a stalled transfer, not a long one | Lajstrom bounds no whole transfer, only a body that moves no byte for `server.stallTimeout` (1 min), the headers (10 s) and idle connections (120 s) | `proxy_send_timeout`, `proxy_read_timeout`, `client_body_timeout`, `send_timeout`: 600 s between two reads or writes | no timeout set in the file |
| Leave `Location` and `WWW-Authenticate` as they are | upload sessions and the challenge carry them | `proxy_redirect off` | the default |
| HTTP/1.1 and keep-alive to the upstream, idle connections closed before Lajstrom's 120 s | no new connection per request, and no request sent on a connection Lajstrom is closing | `proxy_http_version 1.1`, `Connection ""`, `keepalive` (idle 60 s by default) | `transport http { keepalive 60s }` |
| Pass `Host`, `X-Forwarded-For`, `X-Forwarded-Proto` | `X-Forwarded-For` names the client when the edge is in `server.trustedProxies` ([below](#the-client-address)) | `proxy_set_header` | the default |
| Never route the metrics listener | `/metrics` and `/debug/pprof/` are on `server.metricsListen`, another port; through both edges `/metrics` was `404` | | |
| Health check on `/-/readyz` | `503` while the database or a registry is down ([operations.md](operations.md#readiness)) | (nginx Plus or an external check) | `health_uri /-/readyz` |

### `server.externalURL`

Today it makes the realm and the service of the bearer challenge, and nothing else: the
`Location` of an upload or a raw publish is a path, the file links of the PyPI simple index are
relative, and a blob's `307` is the registry's own URL. Without it the realm is
`http://<Host>/v2/token`, plain HTTP behind a TLS edge, which clients refuse or should:

```text
externalURL "": www-authenticate: Bearer realm="http://localhost:18743/v2/token",service="localhost:18743"
externalURL "https://localhost:18743": www-authenticate: Bearer realm="https://localhost:18743/v2/token",service="localhost:18743"
```

(`curl -sD - https://localhost:18743/v2/` through nginx, Lajstrom restarted between the two.)
Since #248 the key is required outside the dev stack (`server.insecureHTTP`), so the first line
is the dev stack's only.

### Uploads

With nginx's default body limit, any upload over 1 MiB fails at the edge before Lajstrom sees it:

```text
without client_max_body_size 0: PUT 2 MiB 413
with client_max_body_size 0:    PUT 2 MiB 201
```

and the error log of nginx says `client intended to send too large body: 2097152 bytes`. With the
file as it is, 100 MiB went through both ways:

```text
PUT big 201 104857600 bytes 1.043854s
GET big 200 104857600 bytes 0.525281s
```

`proxy_request_buffering off` streams the body to Lajstrom as it arrives. Lajstrom then streams raw, apt, maven, pypi and npm
uploads and `/v2` blob uploads to the registry; with buffering on, nginx would first write every upload to
its own disk as well.

### The client address

Lajstrom believes `X-Forwarded-For` only from the peers in `server.trustedProxies` (ADR 0011,
#130). With the list empty, the default, the client is the peer of the connection, the edge. The
same `PUT`, seen by nginx and by Lajstrom with the list empty:

```text
192.168.117.1 - admin [09/Nov/2026:17:25:29 +0000] "PUT /raw/assets/edge/small.txt HTTP/2.0" 201 …
{"msg":"request","method":"PUT","path":"/raw/assets/edge/small.txt","status":201,"remoteAddr":"192.168.117.5:51846"}
```

`192.168.117.5` is nginx. To name the client, put the edge in the list:

```yaml
server:
  trustedProxies: ["192.168.117.5/32"]
```

`X-Forwarded-For`, which the configurations above send already, is then read from the right:
trusted entries are skipped, and the first one that is not is the client. The access log's
`remoteAddr` is the client, with the edge as `proxy`; the audit log's `ip` is the client; and
the throttles of failed logins and of `/v2/token` per address ([auth.md](auth.md#failed-logins))
count clients, not the edge. With the list empty those two throttles are off, since every
request would come from one address.

In the `ha` profile the list is nginx's address, `172.31.226.10/32`. A `PATCH` sent through nginx
with a forged `X-Forwarded-For: 203.0.113.9`, the line nginx logged, the line of the replica that
served it, and its audit row from `GET /api/v1/audit`:

```text
172.31.226.1 - admin-1794260228 [09/Nov/2026:21:37:08 +0000] "PATCH /api/v1/repos/raw/scratch HTTP/1.1" 200 189 "-" "curl/8.7.1" "203.0.113.9"
time=2026-11-09T21:37:08.350Z level=INFO msg=request method=PATCH path=/api/v1/repos/raw/scratch status=200 bytes=189 duration=3.510191ms requestId=demo-1794260228-2 remoteAddr=172.31.226.1 principal=user:01a1c23e-587e-7ce0-9ed0-76efdf716ffe proxy=172.31.226.10:59564
{"action":"repo.update","ip":"172.31.226.1","details":{"source":"api","changed":{"anonymousRead":false},"requestId":"demo-1794260228-2"}}
```

`172.31.226.1` is the client as nginx saw it, `172.31.226.10` nginx; the forged entry, left of the
one nginx appended, is not believed. `test/e2e/ha.sh` checks the same on every run.

The list must hold what the pod actually sees as its peer, which in Kubernetes is almost
anything: the ingress controller's pod address (from the pod CIDR, new after every reschedule), a
node address (an ingress with `hostNetwork`, or kube-proxy's SNAT under
`externalTrafficPolicy: Cluster`), `127.0.0.6` behind an Istio sidecar, or a cloud balancer's
address. So it takes CIDRs, the pod and node CIDRs, or `0.0.0.0/0` behind a NetworkPolicy that
admits only the ingress namespace. The client's address reaches the ingress at all only with
`externalTrafficPolicy: Local` on the ingress Service, the PROXY protocol between an L4
balancer and the ingress, or an L7 balancer whose ranges the ingress trusts; to check the chain,
send one request from a known address and read the remote address of its access log line, as
above. When the chain cannot be made honest, leave `trustedProxies` empty and limit by address at
the edge. Only the single-host network of the `ha` profile was tried, no cluster;
`test/e2e/auth.sh` exercises the header with `trustedProxies: ["127.0.0.1/32"]` and made-up
client addresses.

## Part 2: rate limits at the edge

Limit by request count only where a request is cheap to send and dear to answer: `/v2/token`,
which checks the credentials of every request, with argon2id for a password that is not in the
verification cache ([auth.md](auth.md#the-verification-cache)), and signs a token. Do not limit the rest by count: one
`docker pull` is tens of requests a second from one address, a `pip install` resolves through
hundreds of `/simple/<project>/` pages, `apt-get update` is a handful of larger ones, and a CI farm
behind NAT is one address. Limit downloads, if at all, by bandwidth per connection and by
concurrent connections per address.

The nginx file sets 20 requests a second per address on `/v2/token`, with a burst of 40 served
at once, and answers the rest `429` with `Retry-After: 1` in the error format of `/v2`. A login
and a pull or push ask the token endpoint once or a few times, so a client stays far below that.
200 token requests sent at once from one address, with `CA`, `P` (the admin's password) and
`base` (`https://localhost:18743`) exported:

```sh
seq 1 200 | xargs -P 200 -I{} sh -c 'curl -s --cacert "$CA" -u "admin:$P" -o /dev/null -D - "$base/v2/token?service=localhost:18743" | tr -d "\r" | grep -i -E "^HTTP|^retry-after" | paste -sd " " -' | sort | uniq -c
```

```text
  67 HTTP/2 200
 133 HTTP/2 429  retry-after: 1
```

The next request three seconds later got a token. Under the same limits the 100 MiB pushes and
pulls and the 30-layer artifact above passed. pip, apt and Maven never ask the token endpoint;
how each OCI client handles a `429` from it was not tried.

The same file is the edge of the `ha` profile, where `test/e2e/ha.sh` sends the 200 requests from
one `curl --parallel` on every run; in the last, 46 reached the replicas and 154 got nginx's `429`
with `Retry-After: 1`, and the replicas answered none `429`. Through that edge, under its limits,
`ha.sh` runs the end-to-end scenarios of [testing.md](testing.md), among them `pip install` and
`uv pip install` (`pypi.sh`), `apt-get update` and `apt-get install` (`apt.sh`), `mvn deploy` and
Gradle (`maven.sh`), `cosign sign` and `verify` (`cosign.sh`); and through it an `oras push` and
`pull` of 100 MiB kept the sha256, and `docker pull` took an image of 30 layers from an `oci`
repo, pushed with `crane append`:

```text
Pushed [registry] localhost:8090/raw/scratch/edge129-1794260180.bin:fd7bd48808422321
Digest: sha256:538e8fa9785ed32aaec5063be4b432da19d13218b594e424ba9c7e2bab4f5fec
Pulled [registry] localhost:8090/raw/scratch/edge129-1794260180.bin:fd7bd48808422321
Digest: sha256:538e8fa9785ed32aaec5063be4b432da19d13218b594e424ba9c7e2bab4f5fec
oras: 100 MiB pushed and pulled through the edge, the same sha256, in 0 s
crane: localhost:8090/oci/edge129-1794260180/thirty:1 with 30 layers
Status: Downloaded newer image for localhost:8090/oci/edge129-1794260180/thirty:1
localhost:8090/oci/edge129-1794260180/thirty:1
docker pull: 30 layers in 1 s
```

The file also carries, commented out, `limit_conn` (64 connections per address) and
`limit_rate` (50 MB/s per connection after the first 100 MB). With the three lines uncommented
in the `ha` profile's nginx, the runs above passed too: every step of `ha.sh` and its scenarios,
`tokens.sh` once rerun (its first run, under a load average of 60, took more than the 2 s its
expiring token lives), the 100 MiB `oras` round trip and the `docker pull`. That is one client
on loopback, not a load test: turn them on only knowing how many clients share an address. A
limit per credential is better done in Lajstrom, which throttles failed logins per account,
than by keying nginx's zone on the `Authorization` header, which would keep the credentials
themselves in nginx's shared memory.

Caddy has no rate limiter in its standard build: a limit needs the `caddy-ratelimit` module, which
was not tried.

### Traefik

Traefik's `rateLimit` middleware keys its bucket on the client address by default, the peer of
the connection, and answers `429` with `Retry-After`. The dynamic configuration, for the file
provider:

```yaml
# Traefik in front of Lajstrom (docs/reverse-proxy.md): the dynamic configuration, for the file
# provider. Tried with Traefik v3.7.17; replace the host and the upstream addresses, and add
# tls to both routers on a TLS entry point.
http:
  routers:
    lajstrom:
      rule: Host(`art.example.com`)
      service: lajstrom
    # The longer rule wins: /v2/token takes this router and its limit.
    lajstrom-token:
      rule: Host(`art.example.com`) && Path(`/v2/token`)
      service: lajstrom
      middlewares: [lajstrom-token]
  middlewares:
    # 20 requests a second per client address, a burst of 40; the rest 429 with Retry-After.
    lajstrom-token:
      rateLimit:
        average: 20
        period: 1s
        burst: 40
  services:
    lajstrom:
      loadBalancer:
        servers:
          - url: http://lajstrom-a:8080
          - url: http://lajstrom-b:8080
        healthCheck:
          path: /-/readyz
          interval: 10s
```

It was tried with `localhost` in place of `art.example.com`, on an entry point of plain HTTP,
started as:

```sh
traefik --providers.file.filename=/etc/traefik/lajstrom.yml --entryPoints.web.address=:80 \
  --entryPoints.web.transport.respondingTimeouts.readTimeout=0
```

`readTimeout=0` is the setting of part 1 that Traefik needs: since v3 an entry point reads a
request for at most 60 s by default, the body included, and cuts a longer upload. A 70 MiB `PUT`
at 1 MB/s through it, with `readTimeout=0` and then without:

```text
PUT 70 MiB at 1 MB/s 201 70.634290 s
PUT 70 MiB at 1 MB/s, readTimeout by default: 499 62849024 bytes 60.047752 s
```

Nothing else of part 1 needed a setting: through it `test/e2e/raw.sh` and `oras.sh` passed (the
challenge, the token, the upload sessions), and a 100 MiB `PUT` and `GET` kept the sha256. 200
token requests at once (one `curl --parallel`):

```text
  42 200 retry-after=
 158 429 retry-after=1
```

### Envoy

Envoy's local rate limit filter keeps its buckets in each Envoy, without a rate limit service.
`remote_address` makes a descriptor of the client address, and a descriptor entry without a value
gives each address a bucket of its own; the route's own bucket stays a ceiling for all addresses
together, so it is set well above the one of an address. A bootstrap configuration:

```yaml
# Envoy in front of Lajstrom (docs/reverse-proxy.md): one listener, the whole host to the
# replicas, and a local rate limit on /v2/token per client address. Tried with Envoy 1.40.2;
# replace the upstream addresses and terminate TLS on the listener (or in front of it).
static_resources:
  listeners:
    - name: lajstrom
      address: {socket_address: {address: 0.0.0.0, port_value: 8080}}
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: lajstrom
                use_remote_address: true   # the peer is the client: appended to X-Forwarded-For
                stream_idle_timeout: 600s  # a stalled transfer, not a long one
                route_config:
                  virtual_hosts:
                    - name: lajstrom
                      domains: ["*"]
                      routes:
                        - match: {path: /v2/token}
                          route: {cluster: lajstrom}
                          typed_per_filter_config:
                            envoy.filters.http.local_ratelimit:
                              "@type": type.googleapis.com/envoy.extensions.filters.http.local_ratelimit.v3.LocalRateLimit
                              stat_prefix: lajstrom_token
                              # A ceiling for all addresses together on this Envoy: a request takes a
                              # token from this bucket and from its address's below.
                              token_bucket: {max_tokens: 1000, tokens_per_fill: 500, fill_interval: 1s}
                              # 20 a second per client address, a burst of 40: one bucket per address
                              # (an entry without a value), at most 10 000 addresses at a time.
                              rate_limits:
                                - actions: [{remote_address: {}}]
                              descriptors:
                                - entries: [{key: remote_address}]
                                  token_bucket: {max_tokens: 40, tokens_per_fill: 20, fill_interval: 1s}
                              max_dynamic_descriptors: 10000
                              filter_enabled:
                                default_value: {numerator: 100, denominator: HUNDRED}
                                runtime_key: lajstrom_token_enabled
                              filter_enforced:
                                default_value: {numerator: 100, denominator: HUNDRED}
                                runtime_key: lajstrom_token_enforced
                              response_headers_to_add:
                                - header: {key: retry-after, value: "1"}
                                  append_action: OVERWRITE_IF_EXISTS_OR_ADD
                        - match: {prefix: /}
                          # No timeout for a whole request (Envoy's default is 15 s): a push of
                          # gigabytes takes longer; stream_idle_timeout bounds a stalled one.
                          route: {cluster: lajstrom, timeout: 0s}
                http_filters:
                  - name: envoy.filters.http.local_ratelimit
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.local_ratelimit.v3.LocalRateLimit
                      stat_prefix: lajstrom
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
  clusters:
    - name: lajstrom
      type: STRICT_DNS
      lb_policy: ROUND_ROBIN
      load_assignment:
        cluster_name: lajstrom
        endpoints:
          - lb_endpoints:
              - endpoint: {address: {socket_address: {address: lajstrom-a, port_value: 8080}}}
              - endpoint: {address: {socket_address: {address: lajstrom-b, port_value: 8080}}}
      health_checks:
        - timeout: 2s
          interval: 10s
          unhealthy_threshold: 2
          healthy_threshold: 1
          http_health_check: {path: /-/readyz}
```

Two settings of part 1 are Envoy's own: `use_remote_address`, without which Envoy does not
append the peer to `X-Forwarded-For`, and the route `timeout`, 15 s by default, which starts once
the whole request has arrived. With the default, a 20 MiB `PUT` and a `GET` at 1 MB/s (20 s each)
still passed; but Lajstrom answers a raw or pypi upload only after it pushed the file to the
registry, which for gigabytes takes longer than 15 s, so `timeout: 0s` leaves the bound to
`stream_idle_timeout`, a stalled stream. Through it `raw.sh` and `oras.sh` passed, and a
100 MiB `PUT` and `GET` kept the sha256. 200 token requests at once:

```text
  46 200 retry-after=
 154 429 retry-after=1
```

In Istio the same filter goes into the sidecar of Lajstrom's pods with an `EnvoyFilter` (the
`typed_per_filter_config` above on the route of `/v2/token`); then each pod has its buckets, and
the client address is what the sidecar sees, which [ADR 0011](adr/0011-client-address-behind-a-proxy.md)
lists. That `EnvoyFilter` was not tried: no mesh was at hand.

### One bucket per address

Each of the three limits keys on the client address. Two containers on the network of the `ha`
profile, at `172.31.226.50` and `.51`, sent 100 token requests each in the same second:

| Edge | `.50`: 200, 429 | `.51`: 200, 429 |
| --- | --- | --- |
| nginx | 41, 59 | 41, 59 |
| Traefik | 40, 60 | 40, 60 |
| Envoy | 40, 60 | 40, 60 |

With the route's bucket of Envoy at 40, as a first try had it, the two got 40 together.

When Lajstrom throttles a login or `/v2/token` (#130), and when a registry behind it answers
`429`, `/v2` answers `429` with the code `TOOMANYREQUESTS`, as the nginx file does at the edge.

## What was not verified

- A blob `307` to object storage through the edge: the registry behind the tests was on its
  filesystem. The dev stack's `s3` profile has one.
- The Kubernetes roads of [the client address](#the-client-address), and Istio's `EnvoyFilter`:
  no cluster and no mesh.
- How each OCI client handles a `429` of the token endpoint.
- ingress-nginx annotations: ingress-nginx is retired upstream, and the Kubernetes edge is the
  Gateway API instead (#224).
