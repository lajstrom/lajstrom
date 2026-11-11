# Amazon ECR

**Tested against moto; live ECR pending #19.** The provider of `registries[].type: ecr`
(`internal/provider/ecr`, #36) was run on 2026-11-10 against moto 5.2.5, the AWS mock, for the
ECR API, and against distribution 3.1.2 behind htpasswd for `/v2`, which moto does not serve.
Nothing on this page was measured against ECR itself: the ECR row of the
[matrix](README.md#versions-tested) stays `untested` until the spike #19 runs with an AWS
account, and what is written below from the AWS documentation or from memory says so.

## What the provider does

- **Credentials**: the AWS SDK's default chain, in its order: the environment
  (`AWS_ACCESS_KEY_ID`, …), the shared profile (`AWS_PROFILE`), web identity (IRSA on EKS:
  `AWS_ROLE_ARN` and `AWS_WEB_IDENTITY_TOKEN_FILE`), the container or instance role. Nothing in
  the file; `registries[].auth` stays empty.
- **`/v2`**: the generic provider's data path, the same as for every other registry, with the
  token of `GetAuthorizationToken` as its Basic credentials (`AWS:<password>`). The token lives
  12 hours; it is cached and replaced an hour before it expires, and every request sends the
  current one, so a long upload session never meets an expired one. A refresh that fails is
  logged and tried again at most once a minute while the token still works.
- **Repositories**: ECR does not create a repository on push. `EnsureRepository`, which ingest
  calls before each publish and the facade before a blob upload or a mount (#356), creates it with `CreateRepository`, with the tag mutability, its
  exclusion filters and the encryption of `registries[].ecr`; a repository that exists, created
  by Terraform or another replica, is left as it is. A repository is asked for once per 10
  minutes. With `capabilities.createOnPush: true` (a creation template with `CREATE_ON_PUSH`, or
  every repository made beforehand) the provider creates nothing.
- **Listing**: `DescribeRepositories`, every page of 1000, filtered by the instance prefix:
  ECR filters by exact names only. ECR has no `_catalog`.
- **Delete**: `DeleteRepository`; `registries[].ecr.forceDelete` deletes the images too, and
  without it ECR refuses a repository that holds any (`409`).
- **Errors**: `RepositoryNotFoundException`, `ImageNotFoundException` → `ErrNotFound` (404);
  `ImageTagAlreadyExistsException`, `RepositoryNotEmptyException` and `LimitExceededException`
  (a quota: repositories per region, images per repository) → `ErrConflict` (409); IAM and
  signature errors (`AccessDeniedException`, …) and a token that cannot be had →
  `ErrUnauthorized` (502). The messages carry the error code, never ECR's text, which names the
  prefixed repository and the account. On `/v2`, the generic provider maps ECR's answer to a
  pushed-again immutable tag, `400 TAG_INVALID` (from the AWS documentation), to `ErrConflict`.
- **Retries**: the SDK's standard retryer with `registries[].retries`: throttling
  (`ThrottlingException`, …), 5xx and connection errors, with jittered backoff.
  `LimitExceededException` is a quota on ECR and is not retried. `registries[].timeout` bounds
  each API call up to the response headers.

## Configuration

```yaml
registries:
  - name: ecr
    type: ecr
    url: https://123456789012.dkr.ecr.eu-west-1.amazonaws.com  # the account and region come from the host
    prefix: lajstrom/
    ecr:
      imageTagMutability: IMMUTABLE_WITH_EXCLUSION  # default IMMUTABLE
      imageTagMutabilityExclusions: ["sha256-*"]    # the referrers tag schema and cosign's tags
      encryption: KMS                               # default AES256
      kmsKey: alias/lajstrom
      forceDelete: false
```

Every key is in [configuration.md](../configuration.md#registries). `ecr.region` is needed only
when `url` is not ECR's own host (a proxy, or the dev stack); `ecr.endpoint` replaces the API
endpoint, for a VPC endpoint or for moto:

```yaml
  - name: moto
    type: ecr
    url: http://localhost:5011        # the distribution of the ecr profile
    ecr: {region: us-east-1, endpoint: http://localhost:5010}
```

`IMMUTABLE` refuses a second push of any tag, the `sha256-<digest>` index of the referrers tag
schema and cosign's `.sig` included; where the referrers API is not used (to be measured by #19),
`IMMUTABLE_WITH_EXCLUSION` with `sha256-*` keeps those mutable and Lajstrom's own tags immutable.

## IAM policy

What the provider calls, for the repositories below the instance prefix `lajstrom/`:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Sid": "Token", "Effect": "Allow", "Action": "ecr:GetAuthorizationToken", "Resource": "*" },
    { "Sid": "Catalog", "Effect": "Allow", "Action": "ecr:DescribeRepositories", "Resource": "*" },
    {
      "Sid": "Repositories",
      "Effect": "Allow",
      "Action": [
        "ecr:CreateRepository", "ecr:DeleteRepository",
        "ecr:BatchCheckLayerAvailability", "ecr:InitiateLayerUpload", "ecr:UploadLayerPart",
        "ecr:CompleteLayerUpload", "ecr:PutImage",
        "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer", "ecr:ListImages",
        "ecr:BatchDeleteImage", "ecr:ListImageReferrers"
      ],
      "Resource": "arn:aws:ecr:eu-west-1:123456789012:repository/lajstrom/*"
    }
  ]
}
```

`ecr:CreateRepository` is not needed with `capabilities.createOnPush: true`, nor
`ecr:DeleteRepository` while nothing deletes repos. With `encryption: KMS` the role also needs
`kms:CreateGrant`, `kms:RetireGrant` and `kms:DescribeKey` on the key. To be measured by #19:
whether `DescribeRepositories` without names works with a policy scoped to the prefix (the
statement above allows `*`; the provider filters the names), which `/v2` call needs
`ecr:ListImageReferrers`, and the KMS actions.

## Capabilities

`lajstrom registry probe` does not probe a registry of type `ecr`: its credentials come from the
AWS SDK; the live check is #19.

| Capability | Value | Where it comes from |
| --- | --- | --- |
| Create on push | no | ECR; `CreateRepository` from `EnsureRepository`; `createOnPush: true` for a creation template |
| Referrers | the generic probe decides | to be measured by #19: the referrers API since 2024, but aws/containers-roadmap#2783 reported a 405 for a referrer push with oras |
| Catalog | yes | `DescribeRepositories`; `catalog: false` turns it off |
| Delete manifest | yes, until a 405 | the generic default; to be measured by #19 |
| Immutable tags | yes unless `MUTABLE*` | follows `ecr.imageTagMutability`; a repository created otherwise keeps its own setting |
| Blob redirect | yes | from memory, a 307 to S3: to be measured by #19 |
| Max blob size | 52,000 MiB | the layer size quota of the AWS service quotas page: to be measured by #19 |
| Consistency | eventual, no bound | from users' and tool maintainers' reports (below); `consistencyBound` once #19 measures one |

**Consistency.** AWS documents no read-after-write guarantee for ECR, and none against it.
Practitioners and tool maintainers have met a manifest or a blob answered `404` right after its
push, for 100 ms to several seconds and more under concurrent pushes:
[ocync's commit gate](https://github.com/clowdhaus/ocync/pull/104) for the blobs before the
manifest, [docker/github-builder#30](https://github.com/docker/github-builder/issues/30) (ECR
Public) and its [5 s wait for ECR](https://github.com/docker/github-builder/commit/0396b4ecfb)
before attestations. Replication to other regions takes minutes ("the majority within 30
minutes") and a pull-through cache checks its upstream every 24 hours: a Lajstrom that reads a
replica or a cache sees them later still. The type `ecr` says `consistency: eventual`, and
without `consistencyBound` the start warns ([Consistency](README.md#consistency)).

## Tested against moto

`make dev-up PROFILES=ecr` starts `moto` (`localhost:5010`, `motoserver/moto:5.2.5`, index
`sha256:59a1965d845249b04def37a0e2565f9cc6c44543cb89b68e90f28f344b3b0d1a`) and
`ecr-distribution` (`localhost:5011`, `registry:3.1.2` behind htpasswd, whose one user is moto's
token, `AWS:123456789012-auth-token`, [deploy/compose/ecr/htpasswd](../../deploy/compose/ecr/htpasswd)).
Then:

```sh
LAJSTROM_TEST_ECR_ENDPOINT=http://localhost:5010 LAJSTROM_TEST_ECR_REGISTRY=localhost:5011 \
  go test -race -tags integration -run 'TestIntegrationMoto|TestIntegrationECRNewPath' ./internal/provider/ecr/ ./internal/facade/
```

`TestIntegrationMoto` builds the provider with the SDK against moto: `Capabilities` pings the
distribution with moto's token, `EnsureRepository` creates a repository twice (one
`CreateRepository`) with `IMMUTABLE_WITH_EXCLUSION`, a filter and KMS, which
`DescribeRepositories` reads back, and accepts one made beforehand; a blob and a tagged manifest
go through `/v2` and come back; `ListRepositories` lists the run's prefix; `DeleteRepository` of
a repository with an image is `ErrConflict`, with `forceDelete` it goes, and again it is
`ErrNotFound`. `TestIntegrationECRNewPath` pushes a tagged artifact to a new path with oras-go
through the facade: the facade creates the repository in moto before the first blob, which
`ListRepositories` shows (the distribution would take the blobs without it). Both skip without
the two variables, so the integration job of CI, which has no moto, skips them.

What moto does not do, tested with the fake client of the unit tests instead:

- **Token lifetime**: every token of moto expires on 2015-01-01. The provider uses a token
  whose expiry is past for at most a minute, with a warning, so moto works; the 12-hour
  refresh, the refresh that fails and the concurrent fetch are `TestTokenRefresh`,
  `TestTokenLifetime` and `TestTokenConcurrent`.
- **Pagination**: moto's `DescribeRepositories` ignores `maxResults` and answers one page;
  `TestListRepositories` pages by two.
- **Quotas and throttling**: moto enforces neither. `TestTranslate` maps the codes,
  `TestRetryer` checks which are retried, and `TestDataPath` runs the SDK against an endpoint
  that throttles twice.
- **`/v2`**: moto serves none; distribution stands in, without ECR's immutable tags, its 307 to
  S3 or its referrers. `TestDataPath` answers as the AWS documentation says ECR does.
- **IAM and KMS**: moto checks neither the policy nor that the KMS key exists.

## The live check (#19)

With an account, the spike measures what this page marks "to be measured by #19", and #36's
manual criterion: the provider against a real registry with the IAM policy above, the token
past 12 hours, a creation template with `CREATE_ON_PUSH`, a tag pushed again to an immutable
repository on `/v2`, the image quota, and the referrers with oras and through Lajstrom.
