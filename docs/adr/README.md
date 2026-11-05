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
to `superseded by NNNN`. Typos and broken links may be fixed in place.

## Statuses

| Status | Meaning |
| --- | --- |
| `proposed` | Under discussion in a pull request. |
| `accepted` | In force. |
| `rejected` | Considered and declined; kept for the record. |
| `deprecated` | No longer relevant, without a replacement. |
| `superseded by NNNN` | Replaced by a later ADR. |

## Index

| ADR | Title | Status |
| --- | --- | --- |
| [0001](0001-oci-registry-as-sole-storage.md) | The OCI registry is the only storage | accepted |
| [0002](0002-server-side-protocol-emulation.md) | Server-side protocol emulation, no client plugins | accepted |
| [0003](0003-license-and-contribution-terms.md) | License and contribution terms | accepted |
