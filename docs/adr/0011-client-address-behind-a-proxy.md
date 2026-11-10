# 0011. The client address behind a reverse proxy: a list of trusted proxies, nothing else

- Status: accepted; amended by #248 (below, Amendment)
- Date: 2026-11-08
- Deciders: gecube

## Context

Lajstrom is meant to run behind an edge: [http-routing.md](../spec/http-routing.md) §1 says TLS
terminates at an ingress or a reverse proxy by default and Lajstrom listens on plain HTTP inside
the network. Behind an edge the peer of every TCP connection is the edge, and the client's
address survives, if at all, only in a header the edge writes. Four things in Lajstrom want that
address: the access log line (`remoteAddr`, today the peer), the `ip` column of the audit log
([rbac.md](../spec/rbac.md) §5), the per-address throttle of the token endpoint and of failed
logins (#130), and the scheme of the absolute URLs rendered when `server.externalURL` is empty
(`X-Forwarded-Proto`: the realm of the bearer challenge, the redirects).

Where rate limiting lives was decided with #129 and #130 and is the reason the address matters.
Volume is the edge's: requests per second, downloads, bandwidth, connections are stateless per
node there, free, and a deployment's policy rather than the product's (a `docker pull` is tens
of requests a second from one address, a `pip install` resolves through hundreds of index
pages, a CI farm behind NAT is one address: the numbers belong to whoever knows the clients).
Credentials are the process's: only Lajstrom knows that a request was a failed login and whose,
and only it pays the 17 ms of argon2id per attempt. The per-account throttle needs no address;
the per-address throttle of the token endpoint does, and the log and the audit want it anyway.

A forwarded header is a claim by whoever wrote it. A client can send
`X-Forwarded-For: 10.0.0.1` itself; a proxy that follows the convention appends the peer it
saw, so a forgery sits at the left of the list and the truth at the right. The question is
therefore which hops to believe, not which header to read. The project's rule, restated by the
lead for this decision, is as few knobs as possible. The spec reserved `server.trustedProxies`
within #50; the lead asked whether a `server.behindProxy` flag is needed beside or instead of
it, and what a pod sees in Kubernetes.

## Considered options

### A. A boolean `server.behindProxy`

Believe the forwarded headers from any peer when true. One line to configure, and wrong in
every deployment where something other than the edge can reach the port: a pod in the same
namespace, a host on the same network, the dev stack's loopback. `true` without a list is
"trust everyone", which is the vulnerability itself; a list beside the boolean makes the
boolean redundant and adds two contradictory combinations (`true` with an empty list, `false`
with a list) that have to be validated and explained.

### B. Implicit trust of private ranges

What several servers do by default: believe the headers when the peer is in 10/8, 172.16/12,
192.168/16 or loopback. It works in the common case and is the common hole: in a cluster every
pod has a private address, so every workload is a trusted proxy and any of them can put any
address into Lajstrom's audit log and throttle. It is also unexplainable: the operator never
said it.

### C. A header name and a hop count

nginx's `real_ip_header` with `real_ip_recursive`, Gitea's `REVERSE_PROXY_TRUSTED_PROXIES` with
`REVERSE_PROXY_LIMIT`, Envoy's `xff_num_trusted_hops`. Two more knobs: which header
(`X-Forwarded-For`, `X-Real-IP`, `Forwarded` of RFC 7239) and how many hops to skip. The header
knob buys nothing today: every edge appends to `X-Forwarded-For` (nginx, ingress-nginx,
Traefik, Envoy and Istio, Caddy, the cloud balancers), `X-Real-IP` is nginx's derived copy and
sits next to `X-Forwarded-For` in the same configurations, and `Forwarded` is rare. The hop
count is a weaker form of the list: it trusts positions, not addresses, so a forged entry
counts as a hop whenever the chain is shorter than the count expects.

### D. A list of trusted proxies, one header, one algorithm

`server.trustedProxies: ["10.0.0.0/8", "127.0.0.0/8"]`, a list of prefixes. When the peer is in
the list, the client address is read from `X-Forwarded-For` from the right: every entry in the
list is skipped, the first entry not in it is the client, and when every entry is trusted the
leftmost is (the chain was all ours). `X-Forwarded-Proto` from the same request gives the
scheme. When the peer is not in the list, or the list is empty, the headers are ignored and the
peer is the client. `0.0.0.0/0` and `::/0` are ordinary entries meaning "every peer is a proxy",
for a pod that only the ingress can reach because a NetworkPolicy says so. Nothing else is
configurable: no header name, no hop count, no keyword. If an edge that sets only another
header ever appears, the header is one more field, not a redesign.

### E. The PROXY protocol

The balancer prefixes each connection with the client's address (HAProxy's PROXY protocol v1
and v2), and Lajstrom reads it on its own listener. Exact and unforgeable where the balancer
speaks it, and the only way to know the client behind an L4 balancer with no L7 edge in
between (an NLB or MetalLB straight to Lajstrom with TLS in the process, the single-host
topology of http-routing.md §1). The cost is not the parser, about 150 lines or a small
library: it is that the same trust problem comes back one layer down. A client that reaches
the port directly can send a PROXY header too, so the listener may accept it only from the
balancer's ranges, which is a second trusted list, a second listener mode, and a second thing
to get wrong, for one topology in which the address buys little: the per-account throttle
needs no address, and `audit.ip` is best effort by this ADR's own admission. Rejected for the
MVP, with the lead's explicit wish that the front be stated: Lajstrom speaks HTTP, and the
client address is known behind an L7 edge that writes `X-Forwarded-For` or behind nothing at
all. A deployment that wants it behind an L4 balancer puts an L7 edge between them; the PROXY
protocol between the balancer and that edge is the edge's setting, not Lajstrom's.

### The options side by side

| | A. boolean | B. private ranges | C. header + hops | D. list | E. PROXY protocol |
| --- | --- | --- | --- | --- | --- |
| Settings | 1 | 0 | 2 or 3 | 1 | 1, plus a listener mode |
| Forgeable by another workload on the network | yes | yes | when the chain is short | no | no |
| Says what the operator meant | no | no | partly | yes | yes |
| Works with every edge | yes | yes | yes | yes | L4 edges that speak it |
| Code | trivial | trivial | a parser and two knobs | a parser and a prefix match | a listener |

## Decision

D. `server.trustedProxies` is the one setting, and its emptiness means "the peer is the client",
so there is no `behindProxy` flag. The header is `X-Forwarded-For` and the algorithm is
rightmost-untrusted, both fixed. The resolution lives in `internal/server`, once per request,
and everything that wants the client address reads its result: the access log's `remoteAddr`,
`audit.ip`, the throttle of #130, the scheme of rendered URLs. No other code reads a forwarded
header. An empty list behind an edge is a valid configuration: the server says at startup that
the per-address throttle is off (every request shares the edge's address) and that the log
records the edge's address.

Lajstrom speaks HTTP and nothing below it: no PROXY protocol listener. The supported fronts
are two: nothing (a single host with `server.tls.*`, the peer is the client) and an L7 reverse
proxy or ingress that writes `X-Forwarded-For` (nginx, ingress-nginx, Traefik, Envoy and Istio,
Caddy, a cloud L7 balancer). An L4 balancer alone in front is fine for traffic, and the client
address is then the balancer's, by design; to keep the address, an L7 edge goes between them.

## Kubernetes: what the pod sees

The peer address at Lajstrom depends on the road into the pod, and is almost never the
client's:

| Road into the pod | Peer address at Lajstrom | Entry for `trustedProxies` |
| --- | --- | --- |
| an ingress controller as a Deployment | the controller pod's address: one per replica, new after every reschedule | the pod CIDR |
| an ingress controller with `hostNetwork` (the DaemonSet flavour of ingress-nginx) | the node's address | the node CIDR |
| a service mesh sidecar (Istio) | the sidecar's loopback, `127.0.0.6` | `127.0.0.6/32`; what `X-Forwarded-For` then holds is the mesh's rule (Istio's `numTrustedProxies`) |
| a cloud L7 balancer targeting pods directly (an ALB with target type `ip`) | the balancer's addresses | the balancer's published ranges |
| a NetworkPolicy that admits only the ingress namespace | any of the above | `0.0.0.0/0`: the policy is the trust boundary, the list only says so |

Whether the client's address reaches the ingress at all is decided before Lajstrom, by one of
three roads, each with its own setting:

1. `externalTrafficPolicy: Local` on the ingress Service: kube-proxy does not SNAT, so the
   ingress sees the client (or the L4 balancer), at the price that traffic goes only to nodes
   that run an ingress pod.
2. The PROXY protocol between an L4 balancer (an NLB, MetalLB) and the ingress:
   `use-proxy-protocol: "true"` in ingress-nginx and the balancer's matching switch.
3. An L7 balancer (an ALB, a cloud HTTP(S) balancer) that writes `X-Forwarded-For` itself, with
   the ingress told to believe it: `use-forwarded-headers: "true"` and `proxy-real-ip-cidr` set
   to the balancer's ranges.

With none of them the ingress sees the SNAT'd node address under `externalTrafficPolicy:
Cluster`, writes that into `X-Forwarded-For`, and Lajstrom faithfully records a node. The check
is one request from a known address and the `remoteAddr` of its access log line; #129 carries
it as a recipe. When the chain cannot be made honest, the recommendation is to leave the list
empty, keep the per-account throttle, which needs no address, and limit by address at the edge,
where the address is still real.

## Consequences

- #130 adds the key (`server.trustedProxies`: a list of prefixes, validated at load; a bare
  address is taken as `/32` or `/128`), the resolution in `internal/server`, the startup line
  and the configuration row; #129 documents the edge side with the table above.
- The meaning of the access log's `remoteAddr` changes from the peer to the client; when a
  trusted proxy was skipped, the line also carries the peer as `proxy`, so that nothing an
  operator could see before is lost.
- `audit.ip` is exactly as reliable as the chain in front of Lajstrom, and no more; the doc
  says so where the column is described.
- No PROXY protocol listener, no header knob, no hop count. Each would be a new ADR, and the
  header knob one field. `docs/reverse-proxy.md` (#129) opens with the two supported fronts and
  says that an L4 balancer alone leaves the address unknown.
- `0.0.0.0/0` is a loaded gun by design; the configuration row says what it means and that a
  NetworkPolicy is what makes it safe.
- Behind an edge with an empty list the per-address throttle degrades to "off", never to "every
  client shares one bucket": a misconfiguration costs protection, not availability.
- Entries and header values may be IPv6; an `X-Forwarded-For` entry that carries a port (some
  edges write `address:port`) is parsed without it; an entry that is not an address makes the
  whole header untrusted and the peer the client, logged at debug.

## Amendment: TLS in Lajstrom (#248, 2026-11-10)

With `server.tls` set, Lajstrom terminates TLS itself: the front is "nothing" of the two
supported fronts above, and the peer is the client. An empty `server.trustedProxies` then turns
the per-address throttles on with the peer as the client, and the startup line says so; behind
an edge (no `server.tls`) an empty list still turns them off. The one front where this makes
every client one bucket is an L4 balancer that hides the client's address in front of
`server.tls`: as above, the address is then the balancer's, and an L7 edge with
`server.trustedProxies` is the way to keep it.
