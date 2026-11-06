# Roadmap: the first month (2026-11-06 → 2026-12-06)

- Owner: @gecube. Updated when a milestone closes; dates move only by a decision recorded here.
- Tracking: GitHub milestones M0–M3 and the issues in [`docs/plan/`](plan/). This page is the
  narrative; the issues are the truth for status.

## Goal

**v0.1.0 by Friday 2026-12-04**: a single Go binary that serves hosted repositories for four
formats (raw, pypi, apt, maven) on top of an OCI registry (Zot and docker distribution verified;
ECR adapter if the spike allows), with local users, tokens and RBAC, a reindex command that is
proven to rebuild the index from the registry, an OCI facade that `oras` and `cosign` can use,
end-to-end tests with the real clients, a container image and installation docs.

Not this month (post-MVP, tracked as `post-mvp` issues): rpm, npm, terraform, proxy and group
repos, OIDC, UI, docker passthrough and multi-arch stitching, retention, Harbor and GHCR
adapters beyond the spike, FQDN mode.

## Milestones

| Milestone | Due | Done when (the demo) |
| --- | --- | --- |
| **M0 — Skeleton and raw end-to-end** | Fri 2026-11-13 | `make dev-up && make run`; `curl -T file.txt http://localhost:8080/raw/assets/docs/file.txt` then `curl http://localhost:8080/raw/assets/docs/file.txt` returns the bytes; the manifest is visible in Zot with the profile annotations; `make check` and the integration job are green on SQLite and Postgres. Interfaces for plugin, provider, index and ingest merged. |
| **M1 — Facade, reindex, pypi** | Fri 2026-11-20 | `oras push localhost:8080/raw/assets/hello:1.0 …` works through the facade with a token; `twine upload` + `pip install --index-url` works; drop the derived tables, `lajstrom reindex`, every row is back (test asserts equality); `cosign sign` + `cosign verify` through the facade; the registry capability matrix is filled from spikes, not memory. |
| **M2 — apt and auth** | Fri 2026-11-27 | an Ubuntu container does `apt-get update && apt-get install hello` from `http://host/apt/ubuntu-internal jammy main` with `signed-by` our key; a user with a `writer` binding can push, a `reader` cannot, a deploy token works on one repo only, anonymous read toggles per repo; writes are in the audit log; all of it via `/api/v1` and the `admin` CLI. |
| **M3 — maven and v0.1.0** | Fri 2026-12-04 | `mvn deploy` of a multi-file artifact and `mvn dependency:get` from a clean `~/.m2`; `goreleaser` produces binaries, `ghcr.io/lajstrom/lajstrom:v0.1.0`, `docker.io/lajstrom/lajstrom:v0.1.0`, SBOM and a cosign signature; the install guide works on a fresh machine; tag `v0.1.0` with release notes. |
| Buffer | Sat–Sun 2026-12-05/06 | slack for whatever slipped; no new scope. |

Priority inside a milestone: `priority/p0` issues define "done"; `p1` are planned and get cut
first if the week runs out; `p2` are opportunistic.

## Lanes

Three lanes so three people do not touch the same packages in the same week. A person claims
from one lane as a rule; crossing lanes is fine when the lane is empty.

| Lane | Packages | Week 1 | Week 2 | Week 3 | Week 4 |
| --- | --- | --- | --- | --- | --- |
| **core** (lead) | `cmd`, `config`, `server`, `provider`, `index`, `ingest`, `facade`, `auth`, `api`, `keys` | M0-01 skeleton, M0-03 provider, M0-04 index, M0-06 ingest, M0-08 repo API | M1-01..05 facade, M1-06 reindex, M1-13 ECR | M2-03 keys/GPG, M2-05..09 auth, API | M3-02 sessions, M3-10, M3-12 |
| **formats** | `oci/profile`, `format/*`, `testutil` fixtures | M0-02 profile, M0-05 plugin iface, M0-16 fixtures, M0-07 raw | M1-08..10 pypi | M2-01 deb parse, M2-02 apt index, M2-04 pool/upload | M3-01 maven, M3-03 deploy, M3-04 resolve |
| **platform** | `deploy/`, CI, e2e, `docs/`, release, spikes | M0-09 compose, M0-10 CI, M0-11..15 spikes, M0-17 observability | M1-11 e2e harness, M1-12 matrix, M1-15 docs | M2-10 apt e2e, M2-11 docs, M2-12 hardening | M3-05 mvn e2e, M3-06 goreleaser, M3-07 docs, M3-08 release |

## Week by week

**Kickoff, Fri 2026-11-06 – Sun 2026-11-08.** Everyone reads `docs/onboarding.md` and the
specs, installs the toolchain, claims a week-1 issue. The lead opens the skeleton pull request
(M0-01) so that by Monday there is a module to build on. Formats lane starts on the pure
packages (M0-02, M0-16), platform lane on the compose stack (M0-09) and spikes; none of these
need the skeleton.

**Week 1, 2026-11-09 – 11-13: M0.** Skeleton merged Monday; provider and index land by
Wednesday with integration tests; ingest and raw by Friday. Friday demo: the raw round trip.

**Week 2, 2026-11-16 – 11-20: M1.** Facade (proxy, hook, token, redirect) and reindex in core;
pypi in formats; e2e harness in platform. Friday demo: `oras push`, `twine`/`pip`, reindex proof,
cosign.

**Week 3, 2026-11-23 – 11-27: M2.** apt (parse, index, GPG, pool, upload) in formats; users,
tokens, RBAC, API, audit in core; apt e2e, hardening, docs in platform. Friday demo: apt-get on
Ubuntu, RBAC matrix.

**Week 4, 2026-11-30 – 12-04: M3.** maven in formats; sessions in core; release engineering and
docs in platform. Thursday: feature freeze, release candidate from `main`. Friday: v0.1.0.

## Capacity assumption and cut line

The generated effort table in [`plan/backlog.md`](plan/backlog.md) puts the `p0` set at roughly
17–23 person-days per milestone on the size scale (S = 0.5, M = 1.5, L = 4 days). Three people
near full time with AI assistance cover that; three people at 10–15 hours a week cover about a
third of it. So the plan is honest only with one of two readings:

- **Full-time reading.** Each week ships its milestone as written. Review the actual burn on
  Friday of week 1: if M0's p0 set closed, keep the dates.
- **Part-time reading (the fallback).** The formats lane slips by one milestone: pypi lands in
  week 3, apt in week 4, maven after the MVP; v0.1.0 on 2026-12-04 then contains raw, pypi and
  apt with auth and reindex. Core and platform lanes keep their order, because facade, reindex,
  auth and release engineering are what make the thing shippable at all.

Inside a milestone the cut order is: `p2`, then `p1`, then the format of the week moves to the
next milestone. Facade, reindex, auth and the e2e harness are never cut, only re-dated. The lead
decides on Friday and records the change in this page's decision log.

If only two people are available, the formats lane is shared by the lead and platform, and the
part-time reading applies from day one.

## Risks and what we do about them

| Risk | Mitigation |
| --- | --- |
| ECR referrers push returns 405 (aws/containers-roadmap#2783) | M0-13 spike first; tag-schema fallback (M1-14) is the plan B; ECR adapter is p1 |
| apt signing details (InRelease, by-hash, `Acquire-By-Hash`) eat the week | M2-02/03 use a fixture repo checked with real `apt-get` in a container from day one (M2-10 harness is ready in week 2) |
| maven session semantics (checksum PUTs, partial uploads, retries) | M3-02 sessions are designed in week 3 (issue has the state machine), implemented week 4; `seal` vs `abort` policy is explicit |
| three people and three AI assistants rewrite the same package | contracts in `docs/spec/`, lanes, small PRs, draft PRs on day one, `contract` label and lead approval for interface changes |
| CI flakiness with registries in containers | pinned images, health checks, one retry at the job level only, flaky tests disabled with an issue |
| dependency licenses (GPL debian parsers) | parse `.deb` with the standard library; allowlist enforced by CI |
| scope creep from the ecosystem table | anything not in this page is a `post-mvp` issue, discussed on Fridays, not implemented |

## Decisions recorded while planning (2026-11-06)

- Format order raw → pypi → apt → maven (complexity order; apt before maven because of the
  signing work; rpm and npm after the MVP).
- Tag escape scheme changed from the draft (`_` → `_5f`, separator `__`): ADR 0005.
- No auto-creation of repos on push; one API call creates a repo.
- Maven SNAPSHOT versions are post-MVP; releases only.
- Static users from config are allowed in M1 only, replaced by the control plane in M2.
- Zot is the primary development registry; distribution is the second one in CI; ECR by spike.

## After v0.1.0 (preview, not committed)

Month 2 candidates in rough order: docker passthrough with RBAC, proxy repos (pypi, maven
first), rpm, npm, OIDC, retention, UI read-only, Harbor adapter, apt-transport-oci export.
