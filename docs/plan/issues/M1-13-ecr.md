---
id: M1-13
title: "feat(provider/ecr): ECR adapter: auth token refresh, CreateRepository, DescribeRepositories, immutability"
milestone: M1
lane: core
size: L
priority: p1
labels: [area/provider, kind/feature]
depends: [M0-03, M0-13]
---
**Goal**

ECR as a production backend: the reason the project exists for AWS shops.

**Scope**

- `internal/provider/ecr`: credentials via the AWS SDK default chain (IRSA, profile, env);
  `GetAuthorizationToken` cached and refreshed before the 12-hour expiry; `EnsureRepository`
  via `CreateRepository` (idempotent; honours `imageTagMutability` and encryption settings from
  config or a creation template), `ListRepositories` via `DescribeRepositories` with a prefix
  filter; `DeleteRepository` with `force` policy; capabilities per the M0-13 findings (referrers
  API or tag fallback, 307 to S3, immutable tags).
- Error mapping for `ImageTagAlreadyExistsException` → `ErrConflict`, limits → `ErrDenied`.

**Acceptance criteria**

- [ ] Manual test against a real ECR account (documented in `docs/registries/ecr.md` with the
      IAM policy needed); no ECR in CI.
- [ ] Unit tests with a mocked ECR client for token refresh timing, create-on-push vs
      pre-create, pagination.
- [ ] docs: `docs/registries/ecr.md` config section and IAM policy.
