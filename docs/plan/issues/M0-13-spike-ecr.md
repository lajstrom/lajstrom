---
id: M0-13
title: "spike(provider): registry capability matrix: ECR"
milestone: M0
lane: platform
size: M
priority: p1
labels: [area/provider, kind/spike]
depends: []
assignee: gecube
---
**Question to answer**

Which cells of the capability matrix (`docs/architecture.md` §5) are true for ECR, measured
with `oras`, `curl` and the registry's own API, not from memory.

**Method**

Needs an AWS account (lead). Verify: repository creation template with `CREATE_ON_PUSH` by prefix vs `CreateRepository` from the adapter; OCI 1.1 referrers push with `oras attach` (issue aws/containers-roadmap#2783 reported 405); `DescribeRepositories` pagination; manifest delete; tag immutability setting and the error returned on re-push; 307 redirects to S3 on blob GET and their expiry; token lifetime (12 h) and refresh; the 10k images per repo limit; manifest size limit; any rejection of unknown artifactType or non-image media types.

For each row: repo created on push · referrers API (push and list) · repo listing and its
pagination · delete manifest · immutable tags · blob redirect (307 and to where) · max blob size
· anything that rejects our manifest profile (artifactType, empty config, custom media types).

**Deliverable**

`docs/registries/ecr.md`: the filled row, the exact commands and responses (trimmed), the
registry version tested, and "what the provider must do differently" as a short list. Link the
page from `docs/registries/README.md`. M1-12 consolidates the pages.

**Timebox**

Half a day (M). If blocked (account, access), write what was learned and stop.
