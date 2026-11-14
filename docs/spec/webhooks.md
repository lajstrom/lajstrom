# Spec: webhooks and events

- Status: normative (2026-11-14), issue #77. Owned by `internal/webhook`. Changes need an issue
  labelled `contract`.
- Related: [http-routing.md](http-routing.md) §4 (the routes), [rbac.md](rbac.md) §2, §3, §5
  (who may, the audit rows), [index-schema.md](index-schema.md#webhooks) (the tables),
  [guarantees.md](guarantees.md) INV-43 and INV-44.

A webhook is a URL that Lajstrom sends an HTTP `POST` to when a write it asks for commits: a
version published or deleted, a repo created or deleted. The request is a CloudEvent, signed
with the webhook's secret, sent from an outbox in the index database by any replica, again
until the receiver answers `2xx` or the attempts run out. Webhooks are control plane: they and
their deliveries are rows of the index that reindex neither reads nor rebuilds.

## 1. Events

| Type (`events` of a webhook) | CloudEvents `type` | Sent when |
| --- | --- | --- |
| `version.published` | `dev.lajstrom.version.published` | a version is indexed by a publish through ingest (any frontend, `/v2`, a maven commit), `data.action` `push`; or by a proxied ingest, `data.action` `proxy.fetch` (INV-33). The repeat of a publish that another replica confirmed first (the audit row's `existed`) sends none |
| `version.deleted` | `dev.lajstrom.version.deleted` | a version's rows are deleted by a delete through ingest (`DELETE` of the management API, `versions:delete`, `/v2`, a format's own delete or unpublish), one event per variant, `data.action` `delete` |
| `repo.created` | `dev.lajstrom.repo.created` | a repo is created: `POST /api/v1/repos`, `lajstrom admin repo create`, the `repos[]` bootstrap |
| `repo.deleted` | `dev.lajstrom.repo.deleted` | a repo is deleted: `DELETE /api/v1/repos/…`, `lajstrom admin repo delete` |
| (none) | `dev.lajstrom.webhook.ping` | only by the test of one webhook (http-routing.md §4) |

Each event is the write of an audit row, and is written in that row's transaction (§4). Not
events: pushes and deletes of an `oci` repo, referrers, journal entries (npm dist-tags and
deprecations), a `proxy.reject`, a reindex or the reconcile loop adding or removing rows (they
are not writes through Lajstrom), a change of a repo's settings, a proxy converted. A type added
later is added to this table; a receiver ignores a type it does not know.

## 2. The request

`POST <url>` with the event as one JSON document, the CloudEvents 1.0 **structured content
mode** of its HTTP binding:

| Header | Value |
| --- | --- |
| `Content-Type` | `application/cloudevents+json; charset=utf-8` |
| `User-Agent` | `lajstrom/<version>` |
| `webhook-id` | the event's `id` (below): the same on every attempt |
| `webhook-timestamp` | the time of the attempt, in seconds since the Unix epoch |
| `webhook-signature` | `v1,` and the base64 (standard, padded) of HMAC-SHA256 over `<webhook-id>.<webhook-timestamp>.<body>`, keyed with the secret's bytes |

The three `webhook-*` headers are those of the Standard Webhooks specification
(standardwebhooks.com), so its verifiers check them. The secret is `whsec_` followed by the
base64 of 32 random bytes; the key of the HMAC is those bytes. A receiver recomputes the
signature over the body as it arrived, compares it in constant time, and refuses a timestamp
too far from its clock (Standard Webhooks suggests 5 minutes): a captured request replays only
within that window. A header carries one signature; a later rotation with a grace period may
send two, separated by a space, as the specification allows.

No other header is sent: no credential of a client, of Lajstrom or of a registry, no cookie.

The document:

| Attribute | Value |
| --- | --- |
| `specversion` | `1.0` |
| `id` | a UUID v7, made when the event is written: one per event, the same for every webhook that gets it and on every attempt. A receiver deduplicates on it |
| `source` | `<server.externalURL>/<format>/<repo>`; for a ping `<server.externalURL>/api/v1/webhooks/<id>`. Without `server.externalURL` (the dev stack), the path alone |
| `type` | of §1 |
| `subject` | the audit row's subject: `<package> <version>[ <variant>]` for a version; absent for a repo and a ping |
| `time` | the time of the event's write, RFC 3339, UTC |
| `datacontenttype` | `application/json` |
| `data` | §3 |

The body is the same bytes on every attempt: it is stored when the event is written.

## 3. The payload: `data`

`data.schema` is `1`. Within schema 1, fields are only added; a receiver ignores fields it does
not know. A change that removes a field or changes its meaning is schema 2, and is sent only to
webhooks that ask for it.

Every event:

| Field | Value |
| --- | --- |
| `schema` | `1` |
| `action` | the audit row's action: `push`, `proxy.fetch`, `delete`, `repo.create`, `repo.delete`, `webhook.test` |
| `actor` | `{"principal", "name"}`: the principal of the write as rbac.md §1 writes it (`user:<uid>`, `token:<id>`, `anonymous`), and a user's name at the time of the event (absent for the others) |
| `audit` | the id of the event's audit row; absent for a ping |
| `requestId` | the request's id, when the write came from a request |

A version (`version.published`, `version.deleted`) adds:

| Field | Value |
| --- | --- |
| `repo` | `{"format", "name", "kind"}` |
| `package` | the package name, as the format spells it (`lajstrom.name`) |
| `version`, `variant` | `lajstrom.version` and `lajstrom.variant` (`""` when the format has none) |
| `tag` | the tag of the version's manifest |
| `digest` | the manifest digest |
| `artifactType` | the manifest's `artifactType` |
| `ociRepository` | `<format>/<repo>/<package path>`, below the instance prefix: `/v2/<ociRepository>/manifests/<digest>` serves the manifest |
| `files` | `version.published` only: the file layers in manifest order, each `{"name", "mediaType", "digest", "size"}` |
| `upstream` | `proxy.fetch` only: the name of the upstream in the configuration |

A repo (`repo.created`, `repo.deleted`) adds `repo`, as above; `repo.deleted` also `versions`,
the number of versions it had, and `force`. A ping adds `webhook`: `{"id", "mask"}`.

**Never in an event:** a secret of any kind (a webhook's secret, a token, a password, a registry's
or an upstream's credentials, the master key), the client's address, a request's headers, an
upstream's URL, a version's metadata layer, the details of its audit row. Nothing in an event
needs more than `read` on its repo but `actor`, which the audit log shows to the admin role only
(rbac.md §5): who may manage a repo's webhooks learns who wrote to it.

## 4. Delivery

**The outbox.** An event is written in the transaction that writes its audit row `ok` (a
publish's and a delete's, rbac.md §5), or that appends it (a repo's): one `webhook_deliveries`
row per active webhook whose mask matches the repo (rbac.md §3, as a binding's) and whose
`events` include the type, each with the body of §2. So an event is never lost once its write
committed, and never sent for a write that rolled back. A transaction whose event no webhook
matches reads the webhooks and writes nothing more. A webhook created after the commit does not
get the event; one deleted before its delivery does not either.

**The workers.** Every replica runs `webhooks.workers` deliveries at a time (`0`: none on that
replica). A worker claims due rows (`pending`, `next_at` passed, of an active webhook) with its
replica's id and the time; a claim older than 2 minutes is stale, and another replica takes the
row over. An attempt ends within `webhooks.timeout`, at most 60 seconds, well inside that bound,
so a claim needs no heartbeat. The outcome is recorded only while the claim is the worker's
(a compare-and-set): a worker that lost its claim drops its outcome, and the row is sent again.
Without a master key no worker runs, and the rows wait.

**Attempts.**

- An answer `2xx`, within `webhooks.timeout` of the request (headers and body), is a delivery:
  `delivered`. The receiver's body is read up to 4 KiB and dropped.
- Any other answer, a timeout, and an error of the connection are a failed attempt: the row is
  due again after a backoff, 30 seconds doubled at every attempt up to 1 hour, with up to 10 %
  jitter, until it has had `webhooks.maxAttempts` attempts; then `failed`. A `Retry-After` is not
  read.
- A target that the egress rules refuse (below) is `failed` at once: another attempt would be
  refused alike.

**Redirects.** A `307` or `308` is followed with the same method and body, at most 5 times, each
target checked as the URL is, and never from `https` to `http`. Any other `3xx` is an answer that
is not `2xx`.

**Egress** (INV-44). The URL's host, and each redirect's, is dialed as it is when it is in
`webhooks.allowedHosts` (or is the egress proxy the environment chooses, `HTTPS_PROXY`); any other
host only over `https` and only at public unicast addresses: never loopback, a private range
(RFC 1918, RFC 4193), link-local (`169.254.169.254`, the metadata service of every cloud),
carrier-grade NAT, unspecified, multicast or broadcast. Every address the name resolves to must be
public, and the dial goes to those addresses, so that a second resolution cannot change the
answer (`egress.Guard`, proxy-and-group.md §9.2). With an egress proxy Lajstrom does not resolve
the name: an address literal of a private range is refused, and the proxy enforces the rest.

**The delivery log** is the rows: the state, the attempts, when the next is due, the time, status
and error of the last, when it was delivered. The error is Lajstrom's words (`timeout`,
`connection refused`, `answered 503`, `refused: the host has no public address`), never the
receiver's body, and never the address a name resolved to: a refused address and a name that
does not resolve read the same, so that a webhook is no oracle for the names of an internal DNS.
Rows older than `webhooks.retention` are deleted, whatever their state.

## 5. Webhooks

A webhook is `{id, mask, url, events, active, description}` and its secret.

- **`mask`**: a repo mask of rbac.md §3 (`apt/ubuntu-internal`, `apt/*`, `*/payments`, `*/*`),
  set at creation and never changed. A webhook whose mask is one repo is that repo's; one of a
  wildcard is the admin's. Like a binding, it outlives the repo: a repo created again under the
  name gets the webhooks of its mask.
- **`url`**: absolute, `http` or `https`, with a host, without user information or a fragment, at
  most 2048 bytes; `http`, or an address literal that is not public, only for a host of
  `webhooks.allowedHosts`. It is shown to whoever may manage the webhook, and logged by its host
  only: receivers put tokens in paths.
- **`events`**: the types of §1 it gets; absent or empty, every type.
- **`active`**: an inactive webhook gets no new event, and its pending rows wait until it is
  active again, or until `webhooks.retention`.
- **`description`**: at most 256 bytes.
- **The secret** is made by Lajstrom, shown once in the answer that creates it or rotates it,
  and stored encrypted with the master key (AES-256-GCM, the webhook's uid as additional data, as
  `keys.private_enc`). It is never logged, never in an audit row, a list or an error. A rotation
  counts for every attempt after its commit, the retries of earlier events included. Without a
  master key (`keys.masterKeyEnv`, `keys.masterKeyFile`) a webhook is neither created nor rotated
  (`501 UNAVAILABLE`).
- **A test** sends a `webhook.ping` to the webhook at once, active or not, from the replica of
  the request, under the rules of §4 but for the retries: one attempt, whose row is in the
  delivery log, and whose outcome is the answer.

Who may manage webhooks, and their audit rows, are rbac.md §2, §3 and §5; the routes
http-routing.md §4; the shapes [api.md](../api.md#webhooks).

## 6. What is not promised

- **Order.** Retries and the workers of several replicas deliver the events of one webhook in any
  order. `time`, and the UUID v7 `id`, order them.
- **Exactly once.** A receiver gets an event again when its answer did not reach Lajstrom in time,
  or when a worker's claim went stale: it deduplicates on `id`.
- **Timing.** How soon after its commit an event is sent, the backoff and its jitter, how often
  the workers look for due rows: as for every retry ([guarantees.md](guarantees.md#not-guaranteed)).
- **Events of writes that bypass Lajstrom**, and of what reindex adds or removes.
