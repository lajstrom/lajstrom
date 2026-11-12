# Architecture decision records

This directory holds the architecture decision records (ADRs) of Lajstrom: short documents that
capture one significant decision, its context and its consequences. They explain *why* the
project is built the way it is, so that later contributors do not have to reverse-engineer it.

The format is a trimmed-down [MADR](https://adr.github.io/madr/) (Markdown Architectural Decision
Records).

## When to write one

Write an ADR for a change that alters a design principle, the storage model, a public protocol or
API, the security model (RBAC, audit) or the project's legal terms, or that adds a significant
dependency. See [CONTRIBUTING.md](../../CONTRIBUTING.md#architecture-decision-records-adrs).

## How to write one

1. Copy [`0000-template.md`](0000-template.md) to `NNNN-short-title-with-dashes.md`, where `NNNN`
   is the next free number, zero-padded to four digits. Numbers are never reused.
2. Fill in context, decision and consequences. Keep it under a page; link to issues or
   discussions for the details.
3. Open a pull request with status `proposed`. The ADR is discussed in review, together with the
   code change if there is one.
4. Before merging, set the status to `accepted`, or to `rejected`: rejected ADRs are merged as
   well, so the reasoning is kept.

Accepted ADRs are not rewritten. To change a decision, write a new ADR and set the old one's status
to `superseded by NNNN`, or to `accepted; <the part> superseded by NNNN` when the new ADR replaces
a part of it only (0005). An amendment takes the place of a new ADR only when it keeps the
decision and extends it to a case the ADR did not foresee (0011, 0017): a section
`## Amendment: <title> (<issue>, <date>)` at the end, with its context and decision, the text
above it left as it was, and the status `accepted; amended … (below, Amendment)`, in a pull
request labelled `contract`. Typos and broken links may be fixed in place.

## Statuses

| Status | Meaning |
| --- | --- |
| `proposed` | Under discussion in a pull request. |
| `accepted` | In force. |
| `accepted; amended … (below, Amendment)` | In force, extended by the Amendment section at its end. |
| `accepted; <the part> superseded by NNNN` | In force, but for the part a later ADR replaced. |
| `rejected` | Considered and declined; kept for the record. |
| `deprecated` | No longer relevant, without a replacement. |
| `superseded by NNNN` | Replaced by a later ADR. |

## Index

| ADR | Title | Status |
| --- | --- | --- |
| [0001](0001-oci-registry-as-sole-storage.md) | The OCI registry is the only storage | accepted |
| [0002](0002-server-side-protocol-emulation.md) | Server-side protocol emulation, no client plugins | accepted |
| [0003](0003-license-and-contribution-terms.md) | License and contribution terms | accepted |
| [0004](0004-path-first-addressing.md) | Path-first addressing: one host, `/<format>/<repo>/` | accepted |
| [0005](0005-manifest-profile-and-immutable-tags.md) | Manifest profile: OCI 1.1 artifacts with one immutable tag per variant | accepted; the hashed path component `h0__` superseded by [0017](0017-the-grammar-of-oci-names.md) |
| [0006](0006-derived-index-and-control-plane.md) | Derived index plus a small control plane; SQLite and Postgres | accepted |
| [0007](0007-rbac-model.md) | Authentication and RBAC model | accepted |
| [0008](0008-policy-verdicts-and-enforcement.md) | Policy verdicts: deny lists, quarantine, and where enforcement lives | accepted |
| [0009](0009-encryption-at-rest-in-the-registry.md) | Encryption at rest in the registry: OCI encrypted layers, keys held by Lajstrom | accepted |
| [0010](0010-bearer-token-signing-key.md) | Where the bearer-token signing key lives: the database, under the master key | accepted |
| [0011](0011-client-address-behind-a-proxy.md) | The client address behind a reverse proxy: a list of trusted proxies, nothing else | accepted; amended by #248 |
| [0012](0012-npm-profile-routes-and-dist-tags-journal.md) | npm: the profile, the routes, and dist-tags as a journal in the registry | accepted |
| [0013](0013-proxy-and-group-repos.md) | Proxy and group repos: fetch on miss into the one write path, hosted names hide the upstream | accepted |
| [0014](0014-uploads-stream-into-the-registry.md) | Uploads stream into the registry: no request spool | accepted |
| [0015](0015-declarative-management-one-owner-per-object.md) | Declarative management: one owner per object | accepted |
| [0016](0016-go-the-language-of-the-network-os.md) | Go: the language of the network operating system | accepted |
| [0017](0017-the-grammar-of-oci-names.md) | The grammar of OCI names | accepted; amended for Harbor's routes |
