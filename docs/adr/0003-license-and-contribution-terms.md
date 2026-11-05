# 0003. License and contribution terms

- Status: accepted
- Date: 2026-11-05
- Deciders: gecube

## Context

Lajstrom is a new open source project. It needs a license that companies can adopt without legal
friction, contribution terms that keep the provenance of the code clear, and rules for a code base
that will be written to a large extent with AI assistance.

## Decision

- **License: Apache-2.0**, with a `NOTICE` file and the copyright line
  "Copyright 2026 The Lajstrom Authors". It is permissive, widely accepted, and includes an
  explicit patent license.
- **DCO and CLA, both.** Every commit is signed off under the Developer Certificate of Origin,
  which records provenance per commit. Every contributor also signs the [CLA](../../CLA.md) once,
  which grants the project owner a copyright license with the right to sublicense and to relicense
  contributions under other OSI-approved licenses, plus a patent license. The CLA is collected by
  a GitHub Actions bot that stores signatures in this repository, with no external service.
- **Permissive dependencies only.** Allowed: Apache-2.0, MIT, BSD-2-Clause, BSD-3-Clause, ISC,
  MPL-2.0, Unlicense, CC0-1.0, 0BSD. Everything else, including GPL, LGPL, AGPL, SSPL, BSL and
  EUPL, is denied by default and needs a maintainer decision. CI enforces the allowlist.
- **AI assistance is allowed and disclosed.** Commits record tools with an `Assisted-by:` trailer;
  the human author is responsible for the change; a snippet scan in CI flags matches with public
  open source code; bulk or fully automated contributions are closed.

The details are in [CONTRIBUTING.md](../../CONTRIBUTING.md).

## Consequences

- Contributors take two steps (sign-off on each commit, a one-time CLA signature); CI checks both.
- The CLA must name the legal entity that owns the project before the bot is enabled.
- The relicensing right keeps the project's licensing options open, at the cost of some contributors
  declining to sign a CLA.
- Some useful dependencies will be unavailable or need an explicit decision.
- The snippet scan can produce false positives; maintainers have to triage and record them.
