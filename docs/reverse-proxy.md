# Lajstrom behind a reverse proxy

Lajstrom listens on plain HTTP and expects an edge in front of it, an nginx, a Caddy, an
ingress or a Gateway, that terminates TLS for one public host
([http-routing.md](spec/http-routing.md) §1). Part 1 is what the edge must do for the
protocols to work; part 2 is how to limit request rates there, where volume limits belong by
decision ([ADR 0011](adr/0011-client-address-behind-a-proxy.md)): Lajstrom itself will only
throttle what the edge cannot see, failed logins and the token endpoint (#130).

Two configurations were run, each in front of the Lajstrom of the Compose example (#58,
Postgres and distribution 3.1.2 with htpasswd), on macOS with OrbStack, on 2026-11-09:

| Edge | Version | File |
| --- | --- | --- |
| nginx | `nginx:1.30.6-alpine` | [deploy/nginx/lajstrom.conf](../deploy/nginx/lajstrom.conf) |
| Caddy | `caddy:2.11.9-alpine` | [deploy/caddy/Caddyfile](../deploy/caddy/Caddyfile) |

Through both: a raw file and an `oras` artifact of 100 MiB pushed and pulled with the same
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
| Pass `Host`, `X-Forwarded-For`, `X-Forwarded-Proto` | read by nothing today; `server.trustedProxies` (#130) will read them | `proxy_set_header` | the default |
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

`proxy_request_buffering off` streams the body to Lajstrom as it arrives. Lajstrom then spools
a raw, apt, pypi or maven upload to its own `/tmp` for the length of the request and streams the
`/v2` blob uploads to the registry; with buffering on, nginx would first write every upload to
its own disk as well.

### The client address

Lajstrom reads no forwarded header yet: its access log names the edge. The same `PUT`, seen by
nginx and by Lajstrom:

```text
192.168.117.1 - admin [09/Nov/2026:17:25:29 +0000] "PUT /raw/assets/edge/small.txt HTTP/2.0" 201 …
{"msg":"request","method":"PUT","path":"/raw/assets/edge/small.txt","status":201,"remoteAddr":"192.168.117.5:51846"}
```

`192.168.117.5` is nginx. `server.trustedProxies` of ADR 0011 does not exist yet (#130); a
configuration that sets it is refused: `field trustedProxies not found in type config.Server`.
Until then the audit log's `ip` and the access log name the edge, and limits by address can only
be the edge's. The configurations above send `X-Forwarded-For` and `X-Forwarded-Proto` already,
so nothing changes at the edge when #130 lands.

When it lands, the list must hold what the pod actually sees as its peer, which in Kubernetes is
almost anything: the ingress controller's pod address (from the pod CIDR, new after every
reschedule), a node address (an ingress with `hostNetwork`, or kube-proxy's SNAT under
`externalTrafficPolicy: Cluster`), `127.0.0.6` behind an Istio sidecar, or a cloud balancer's
address. So it takes CIDRs, the pod and node CIDRs, or `0.0.0.0/0` behind a NetworkPolicy that
admits only the ingress namespace. The client's address reaches the ingress at all only with
`externalTrafficPolicy: Local` on the ingress Service, the PROXY protocol between an L4
balancer and the ingress, or an L7 balancer whose ranges the ingress trusts; to check the chain,
send one request from a known address and read the remote address of its access log line. When
the chain cannot be made honest, leave `trustedProxies` empty and limit by address at the edge.
None of this was tried here: it waits for #130.

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

The file also carries, commented out, `limit_conn` (64 connections per address) and
`limit_rate` (50 MB/s per connection after the first 100 MB). Uncommented, they pass
`nginx -t`; they were not load-tested; turn them on only knowing how many clients share an address. A limit per
credential is better done in Lajstrom (#130) than by keying nginx's zone on the
`Authorization` header, which would keep the credentials themselves in nginx's shared memory.

Caddy has no rate limiter in its standard build: a limit needs the `caddy-ratelimit` module, which
was not tried.

When Lajstrom limits itself (#130), and when a registry behind it answers `429`, `/v2` answers
`429` with the code `TOOMANYREQUESTS`, as the nginx file does at the edge.

## What was not verified

- The `ha` stack of [acceptance.md](acceptance.md) §H does not exist yet, so the acceptance runs
  of #129 (two replicas, `pip install`, `apt-get install` and a `docker pull` of a 30-layer image
  through the limits) were not run; an `oras` artifact of 30 layers stood in for the image.
- A blob `307` to object storage through the edge: the registry behind the tests was on its
  filesystem. The dev stack's `s3` profile has one.
- ingress-nginx annotations, a Traefik `RateLimit` middleware, an Envoy or Istio local rate limit:
  no snippet here, since none was run.
