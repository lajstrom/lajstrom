---
id: M3-12
title: "security: threat-model pass on the facade and token flow; headers; rate limits"
milestone: M3
lane: core
size: M
priority: p1
labels: [area/auth, area/facade, kind/chore]
depends: [M2-07, M1-03]
---
**Goal**

Before the first public release, a written threat model of the surfaces (facade, token endpoint,
frontends, API) and the fixes it finds.

**Scope**

- `docs/security/threat-model.md` (STRIDE over the flows; what the backend trusts; what a stolen
  deploy token can do; SSRF via provider URLs in config; path traversal in raw and maven paths;
  zip/tar bombs in parsers; JSON manifest size; header injection in `Location`).
- Fixes found; security headers; `/v2/token` and auth failure rate limits (M2-12) verified;
  dependency review with `govulncheck` clean.

**Acceptance criteria**

- [ ] Threat model merged; every high finding fixed or tracked as an issue before v0.1.0;
      SECURITY.md updated with the supported versions table.
