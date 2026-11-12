# Amazon ECR

**Tested against moto; live ECR pending #19.** The provider of `registries[].type: ecr`
(`internal/provider/ecr`, #36) was run on 2026-11-10 against moto 5.2.5, the AWS mock, for the
ECR API, and against distribution 3.1.2 behind htpasswd for `/v2`, which moto does not serve.
Nothing on this page was measured against ECR itself, because the project has no AWS account:
the ECR row of the [matrix](README.md#versions-tested) stays `untested` until the spike #19 runs
with one. A fact from AWS's documentation links its page (read on 2026-11-11). A fact only users
or other tools report says *reported* and links the report. Anything neither source states is
marked *unverified*.

## What the provider does

- **Credentials**: the AWS SDK's default chain, in its order: the environment
  (`AWS_ACCESS_KEY_ID`, …), the shared profile (`AWS_PROFILE`), web identity (IRSA on EKS:
  `AWS_ROLE_ARN` and `AWS_WEB_IDENTITY_TOKEN_FILE`), the container or instance role. Nothing in
  the file; `registries[].auth` stays empty.
- **`/v2`**: the generic provider's data path, the same as for every other registry, with the
  token of `GetAuthorizationToken` as its Basic credentials (`AWS:<password>`). The token lives
  12 hours ([registry authentication](https://docs.aws.amazon.com/AmazonECR/latest/userguide/registry_auth.html)); it is
  cached and replaced an hour before it expires, and every request sends the current one, so a
  long upload session never meets an expired one. A refresh that fails is
  logged and tried again at most once a minute while the token still works.
- **Repositories**: ECR creates a repository on push only when a
  [repository creation template](https://docs.aws.amazon.com/AmazonECR/latest/userguide/repository-creation-templates.html)
  applied for `CREATE_ON_PUSH` matches its name: the longest prefix wins, and `ROOT` matches
  the rest. Without such a template, "Amazon ECR will not create a repository with default
  settings". Create on push exists since 2025-12-19
  ([announcement](https://aws.amazon.com/about-aws/whats-new/2025/12/amazon-ecr-creating-repositories-on-push)).
  A template that sets KMS encryption or resource tags needs a repository creation role, or
  "the repository creation will fail". `EnsureRepository`, which ingest
  calls before each publish and the facade before a blob upload or a mount (#356), creates it with `CreateRepository`, with the tag mutability, its
  exclusion filters and the encryption of `registries[].ecr`; a repository that exists, created
  by Terraform or another replica, is left as it is. A repository is asked for once per 10
  minutes. With `capabilities.createOnPush: true` (a creation template with `CREATE_ON_PUSH`, or
  every repository made beforehand) the provider creates nothing.
- **Listing**: `DescribeRepositories`, every page of 1000, its maximum, filtered by the
  instance prefix: without `repositoryNames` it lists every repository of the registry, and
  `repositoryNames` takes exact names only
  ([DescribeRepositories](https://docs.aws.amazon.com/AmazonECR/latest/APIReference/API_DescribeRepositories.html)).
  ECR also serves `/v2/_catalog`, which this page used to deny: it pages with an opaque token in `Link` and
  answers a repository name as `last` with `405 UNSUPPORTED` (reported,
  [zot#4528](https://github.com/project-zot/zot/pull/4528), 2026-10; AWS documents no
  `_catalog`). The provider does not use it.
- **Delete**: `DeleteRepository`; `registries[].ecr.forceDelete` deletes the images too, and
  without it ECR refuses a repository that holds any (`409`).
- **Errors**: `RepositoryNotFoundException`, `ImageNotFoundException` → `ErrNotFound` (404);
  `ImageTagAlreadyExistsException`, `RepositoryNotEmptyException` and `LimitExceededException`
  (a quota: repositories per region, images per repository) → `ErrConflict` (409); IAM and
  signature errors (`AccessDeniedException`, …) and a token that cannot be had →
  `ErrUnauthorized` (502). The messages carry the error code, never ECR's text, which names the
  prefixed repository and the account. For a tag pushed again to an immutable repository AWS
  documents `ImageTagAlreadyExistsException`
  ([tag mutability](https://docs.aws.amazon.com/AmazonECR/latest/userguide/image-tag-mutability.html)), not the answer
  of `/v2`. There `TAG_INVALID` "The image tag already exists in the repository and cannot be
  overwritten because the tag is immutable." is reported
  ([kaniko#3481](https://github.com/GoogleContainerTools/kaniko/issues/3481)), with an
  unverified status. The generic provider maps `TAG_INVALID` to `ErrConflict` whatever the
  status.
- **Retries**: the SDK's standard retryer with `registries[].retries`: throttling
  (`ThrottlingException`, …), 5xx and connection errors, with jittered backoff.
  `LimitExceededException` is a quota on ECR and is not retried. `registries[].timeout` bounds
  each API call up to the response headers.
- **Uploads**: the generic provider's, as for every registry. AWS documents part sizes for its
  API only: 5 to 10 MiB a part and 4,200 parts, "only applicable if you are using Amazon ECR
  API actions directly" ([quotas](https://docs.aws.amazon.com/AmazonECR/latest/userguide/service-quotas.html)). It
  documents none for `/v2`. Reported in
  [aws/containers-roadmap#2831](https://github.com/aws/containers-roadmap/issues/2831): the
  `POST` of a session answers `OCI-Chunk-Min-Length: 10485760`; a `PATCH` is answered `201`
  where the distribution specification says `202`; the `Range` of later `PATCH`es stayed at the
  first chunk until a fix deployed on 2026-07-13. That every part but the last must be at least
  5 MB is in neither source and is unverified. The facade passes the backend's
  `OCI-Chunk-Min-Length` on to the client in its answers for a session (`facade.answerSession`,
  #664), and sends none when the backend sends none. It does not merge chunks: a client that
  ignores the header sends its chunks to ECR as it cut them. #19 measures whether small chunks
  pass.
- **Mounts**: ECR mounts a blob from another repository only when the registry setting
  `BLOB_MOUNTING` is `ENABLED` (`aws ecr put-account-setting --name BLOB_MOUNTING --value
  ENABLED`, per region). Both repositories must be in one account and region, with the same
  encryption type and key; images of a pull-through cache are excluded; and the caller needs
  `ecr:GetDownloadUrlForLayer` on the source ([blob mounting](https://docs.aws.amazon.com/AmazonECR/latest/userguide/blob-mounting.html);
  since 2026-01-20, [announcement](https://aws.amazon.com/about-aws/whats-new/2026/01/amazon-ecr-cross-repository-layer-sharing/)).
  AWS does not state the default. Its pages only say how to enable the setting, so it is
  presumably off. With the setting on, a mount ECR cannot do yet is answered `202` with a
  session (reported, [ocync](https://clowdhaus.github.io/ocync/registries/ecr)); without it,
  the answer is unverified. The facade passes a `202` on as a plain session.
- **Quotas** ([service quotas](https://docs.aws.amazon.com/AmazonECR/latest/userguide/service-quotas.html)):
  100,000 repositories a region and 100,000 images a repository, both adjustable; 1,000 tags an
  image, fixed. Each manifest is an image to ECR, and so is each referrer, which "counts as an
  image consumed for your repository" ([images](https://docs.aws.amazon.com/AmazonECR/latest/userguide/images.html)).
  Rates a region, all adjustable: `PutImage` 10 a second, which a push of a manifest calls once (as the quotas page
  describes a push), `InitiateLayerUpload` and `CompleteLayerUpload` 100, `UploadLayerPart`
  500, `BatchGetImage` 2,000, `GetDownloadUrlForLayer` 3,000.

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
`CreateRepository` takes `MUTABLE`, `IMMUTABLE`, `IMMUTABLE_WITH_EXCLUSION` and
`MUTABLE_WITH_EXCLUSION`, and at most five filters of type `WILDCARD`, each up to 128
characters of `[0-9a-zA-Z._*-]`
([CreateRepository](https://docs.aws.amazon.com/AmazonECR/latest/APIReference/API_CreateRepository.html),
[filter](https://docs.aws.amazon.com/AmazonECR/latest/APIReference/API_ImageTagMutabilityExclusionFilter.html)).

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
        "ecr:BatchDeleteImage"
      ],
      "Resource": "arn:aws:ecr:eu-west-1:123456789012:repository/lajstrom/*"
    }
  ]
}
```

`ecr:CreateRepository` is not needed with `capabilities.createOnPush: true`, nor
`ecr:DeleteRepository` while nothing deletes repos. There is no `ecr:ListImageReferrers`
action, which this policy used to name: AWS authorizes the `ListImageReferrers` API with
`ecr:BatchGetImage` ([actions](https://docs.aws.amazon.com/service-authorization/latest/reference/list_ecr.html),
[ListImageReferrers](https://docs.aws.amazon.com/AmazonECR/latest/APIReference/API_ListImageReferrers.html)). With
`encryption: KMS` the role also needs `kms:CreateGrant`, `kms:RetireGrant` and
`kms:DescribeKey` on the key. `kms:RetireGrant` must be in the role's own policy; the other two
may be in the key policy instead ([encryption at rest](https://docs.aws.amazon.com/AmazonECR/latest/userguide/encryption-at-rest.html)).
To be measured by #19: whether `DescribeRepositories` without names works with a policy scoped
to the prefix (AWS gives the action the resource type `repository`; the statement above allows
`*`, and the provider filters the names), and which IAM action the `GET` of `/v2`'s referrers
needs (unverified; `ecr:BatchGetImage`, as for `ListImageReferrers`, is likely).

## Capabilities

`lajstrom registry probe` does not probe a registry of type `ecr`: its credentials come from the
AWS SDK; the live check is #19.

| Capability | Value | Where it comes from |
| --- | --- | --- |
| Create on push | no | ECR without a creation template for `CREATE_ON_PUSH` (above); `CreateRepository` from `EnsureRepository`; `createOnPush: true` with such a template |
| Referrers | the generic probe decides | the OCI 1.1 referrers API since 2024-06-27 ([announcement](https://aws.amazon.com/about-aws/whats-new/2024/06/amazon-ecr-oci-image-distribution-version-1-1)); a referrer pushed with `oras copy -r` was answered `405 UNSUPPORTED` "Invalid JSON syntax" (reported, [aws/containers-roadmap#2783](https://github.com/aws/containers-roadmap/issues/2783), open): to be measured by #19 |
| Catalog | yes | `DescribeRepositories`; `catalog: false` turns it off. ECR's own `_catalog` (reported, above) is not used |
| Delete manifest | yes, until a 405 | the generic default. AWS documents `BatchDeleteImage` for its API, not a `DELETE` on `/v2`: to be measured by #19 |
| Immutable tags | yes unless `MUTABLE*` | follows `ecr.imageTagMutability`; a repository created otherwise keeps its own setting |
| Blob redirect | yes | a client gets the manifest from ECR "and then Amazon S3 to download the actual image layers" ([VPC endpoints](https://docs.aws.amazon.com/AmazonECR/latest/userguide/vpc-endpoints.html#ecr-setting-up-s3-gateway)); the status of the redirect and the lifetime of its URL are not documented: to be measured by #19 |
| Max blob size | 52,000 MiB | the maximum layer size of the [quotas](https://docs.aws.amazon.com/AmazonECR/latest/userguide/service-quotas.html) page. Since 2026-08-03 AWS takes layers of up to 200 GB "for images pushed via Docker push", and 50 GB through `UploadLayerPart` ([announcement](https://aws.amazon.com/about-aws/whats-new/2026/08/amazon-ecr-image-layers/)); the quotas page still says 52,000 MiB. Whether a `/v2` push through Lajstrom gets 200 GB is unverified; `capabilities.maxBlobSize` raises the bound |
| Consistency | eventual, no bound | from users' and tool maintainers' reports (below); `consistencyBound` once #19 measures one |

**Consistency.** AWS documents no read-after-write guarantee for ECR, and none against it.
Practitioners and tool maintainers have met a manifest or a blob answered `404` right after its
push, for 100 ms to several seconds and more under concurrent pushes:
[ocync's commit gate](https://github.com/clowdhaus/ocync/pull/104) for the blobs before the
manifest, [docker/github-builder#30](https://github.com/docker/github-builder/issues/30) (ECR
Public) and its [5 s wait for ECR](https://github.com/docker/github-builder/commit/0396b4ecfb)
before attestations. Replication to other regions takes minutes: "The majority of images
replicate in less than 30 minutes, but in rare cases the replication might take longer"
([replication](https://docs.aws.amazon.com/AmazonECR/latest/userguide/replication.html)). A pull-through cache checks its upstream when
it has not done so for 24 hours ([pull through cache](https://docs.aws.amazon.com/AmazonECR/latest/userguide/pull-through-cache.html)).
A Lajstrom that reads a replica or a cache sees them later still. The type `ecr` says `consistency: eventual`, and
without `consistencyBound` the start warns ([Consistency](README.md#consistency)). On such a
registry a publish waits until ECR serves it, a manifest `PUT` answered `MANIFEST_BLOB_UNKNOWN`
and the first write to an upload session answered `BLOB_UPLOAD_UNKNOWN` are sent again within the
bound, and reindex keeps out a version deleted through Lajstrom that ECR still lists
([operations.md](../operations.md#an-eventually-consistent-registry),
[registry-consistency-2026-11.md](../design/registry-consistency-2026-11.md)). None of it is
measured against ECR itself yet: #19.

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

With an account, the spike measures what this page marks "to be measured by #19" or
*unverified*, and #36's manual criterion: the provider against a real registry with the IAM
policy above, the token past 12 hours, a creation template with `CREATE_ON_PUSH`, a tag pushed
again to an immutable repository on `/v2` (its status), the image quota, the referrers with oras
and through Lajstrom, chunks under 5 MB and under `OCI-Chunk-Min-Length`, a mount with and
without `BLOB_MOUNTING`, `_catalog`, a blob above 52,000 MiB, and a `DELETE` of a manifest on
`/v2`.
