# Amazon ECR

**Measured on ECR on 2026-11-13** (#19): a private registry in `ap-southeast-1`, an account and
region with no repository of anyone else's, as a role with the [IAM policy](#iam-policy) below
and nothing more. `lajstrom registry probe`, `TestIntegrationECRLive`, a Lajstrom on it (an oci
proxy repo and a hosted oci repo, with crane, oras and curl through the facade) and requests by
hand; what each found is in [Measured on ECR](#measured-on-ecr-19), the departures from the
distribution specification in [Deviations](#deviations-from-the-specification). Four answers of
ECR broke Lajstrom, and are fixed since (#19's pull requests): the uploads, the tag lists, a
delete an index refuses, and the probe's own manifests. Before that, the provider of
`registries[].type: ecr` (`internal/provider/ecr`, #36) was run against moto 5.2.5, the AWS mock,
for the ECR API, and distribution 3.1.2 behind htpasswd for `/v2` ([Tested against
moto](#tested-against-moto)). A fact from AWS's documentation links its page (read on
2026-11-11). A fact only users or other tools report says *reported* and links the report.
Anything neither source states nor the run measured is marked *unverified*.

## What the provider does

- **Credentials**: the AWS SDK's default chain, in its order: the environment
  (`AWS_ACCESS_KEY_ID`, …), the shared profile (`AWS_PROFILE`), web identity (IRSA on EKS:
  `AWS_ROLE_ARN` and `AWS_WEB_IDENTITY_TOKEN_FILE`), the container or instance role. Nothing in
  the file; `registries[].auth` stays empty.
- **`/v2`**: the generic provider's data path, the same as for every other registry, with the
  token of `GetAuthorizationToken` as its Basic credentials (`AWS:<password>`). The token lives
  12 hours ([registry authentication](https://docs.aws.amazon.com/AmazonECR/latest/userguide/registry_auth.html); measured: `expiresAt` is 12 hours after the call); it is
  cached and replaced an hour before it expires, and every request sends the current one, so a
  long upload session never meets an expired one. A refresh that fails is
  logged and tried again at most once a minute while the token still works.
- **Repositories**: ECR creates a repository on push only when a
  [repository creation template](https://docs.aws.amazon.com/AmazonECR/latest/userguide/repository-creation-templates.html)
  applied for `CREATE_ON_PUSH` matches its name: the longest prefix wins, and `ROOT` matches
  the rest. Without such a template, "Amazon ECR will not create a repository with default
  settings": the first upload session to a new name is 404 `NAME_UNKNOWN` (measured). With a
  template for `CREATE_ON_PUSH` whose prefix covers the name, that session creates the
  repository with the template's settings (measured: a template for `lajstrom/cop` made
  `lajstrom/cop/x` `MUTABLE` as it said). Create on push exists since 2025-12-19
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
  `DescribeRepositories` pages with `nextToken` (measured with `--max-results 1`). ECR also
  serves `/v2/_catalog`, which AWS documents nowhere: it pages with an opaque token as `last` in
  an absolute `Link`, takes `n` up to 1000 (1001 is 405 `UNSUPPORTED`), and answers a
  repository name as `last` with 405 `UNSUPPORTED` "Invalid parameter at 'NextToken' failed to
  satisfy constraint: 'Invalid next token provided'" (measured; reported before in
  [zot#4528](https://github.com/project-zot/zot/pull/4528)). The provider does not use it.
- **Tags**: `tags/list` is in an order of ECR's own (`Z, 1.10, b, a, _x, 1.2, A` as pushed
  `b, A, 1.10, _x, 1.2, Z, a`), and pages the same way as `_catalog`: `n` with an opaque `last`
  in its `Link`, and a tag name as `last` is the same 405. The provider reads every page by its
  `Link` and sorts them, so `/v2` of an oci repo on ECR lists tags in lexical order and pages
  them with a tag name, as the specification says (measured through Lajstrom; #19).
- **Delete**: `DeleteRepository`; `registries[].ecr.forceDelete` deletes the images too, and
  without it ECR refuses a repository that holds any (`409`). On `/v2` (measured): a `DELETE`
  by digest is 202 and takes the manifest's tags with it, and leaves its referrers; a `DELETE`
  by tag deletes the manifest too, not the tag alone, which is what a delete by tag through
  Lajstrom means anyway (it refuses one whose manifest other tags name, #284); a manifest
  that an image index lists is 405 `UNSUPPORTED` "Requested image referenced by manifest list:
  [sha256:…]", which the provider takes as one refused delete (`ErrConflict`, 409), as it takes
  Zot's 405 `DENIED`, and not as a registry without deletes (#19).
- **Errors**: `RepositoryNotFoundException`, `ImageNotFoundException` → `ErrNotFound` (404);
  `ImageTagAlreadyExistsException`, `RepositoryNotEmptyException` and `LimitExceededException`
  (a quota: repositories per region, images per repository) → `ErrConflict` (409); IAM and
  signature errors (`AccessDeniedException`, …) and a token that cannot be had →
  `ErrUnauthorized` (502). The messages carry the error code, never ECR's text, which names the
  prefixed repository and the account. For a tag pushed again to an immutable repository AWS
  documents `ImageTagAlreadyExistsException`
  ([tag mutability](https://docs.aws.amazon.com/AmazonECR/latest/userguide/image-tag-mutability.html)) for its API.
  On `/v2` another manifest under the tag is 400 `TAG_INVALID` (measured), and the same
  manifest again is 201. The generic provider maps `TAG_INVALID` to `ErrConflict`.
- **Retries**: the SDK's standard retryer with `registries[].retries`: throttling
  (`ThrottlingException`, …), 5xx and connection errors, with jittered backoff.
  `LimitExceededException` is a quota on ECR and is not retried. `registries[].timeout` bounds
  each API call up to the response headers.
- **Uploads** (measured): the `POST` of a session is 202 with `OCI-Chunk-Min-Length: 10485760`
  and `Range: bytes=0-10485760`, though the session holds nothing; every `PATCH` is 201, where
  the specification says 202, with `Location` and `Range: 0-<end>`. Each `PATCH` but the last
  must be at least 5 MB: smaller ones are taken, and the closing `PUT` is then 400
  `BLOB_UPLOAD_INVALID` "All layer parts except the last part should be at least 5MB"; 6 MiB
  parts pass, and so does a single `PATCH` of 30 MiB. A chunk out of order is 416. The body of
  a closing `PUT` that follows a `PATCH` is dropped: the `PUT` is 201 for the digest of the
  bytes the `PATCH`es sent and 400 "None of the provided layer digests match the calculated
  layer digest" for the whole blob's; without a `PATCH` before it, the body is the blob (a
  monolithic upload, measured up to 8 MiB by the probe and 30 MiB by oras). A second `PATCH`
  without `Content-Range` is dropped too: `Range` stays at the first chunk, and the `PUT` of
  the whole blob's digest is 400. The `GET` of a session (its status) and its `DELETE` are 404
  `404 page not found`, in plain text. The provider starts a session at 0 whatever `Range`
  says, takes 201 for a `PATCH`, sends the last chunk as a `PATCH` of its own before an empty
  `PUT` (as for JFrog), sends chunks of 8 MiB with `Content-Range`, and passes
  `OCI-Chunk-Min-Length` on to a client (`facade.answerSession`, #664). Through Lajstrom
  (measured): a client's 6+6 MiB chunks, a last chunk in the closing `PUT`, and two `PATCH`es
  of a known length without `Content-Range` all stored the blob; 1 MiB chunks are ECR's 400 to
  the client, `BLOB_UPLOAD_INVALID`, without ECR's text; the session's `GET` and `DELETE` are
  ECR's 404. Before #19's fix the provider read the `POST`'s `Range` as 10485761 bytes held and
  refused the 201, and an oci proxy repo on ECR stored no blob at all. AWS's quotas say parts of
  5 to 10 MiB and 4,200 parts "only applicable if you are using Amazon ECR API actions directly"
  ([quotas](https://docs.aws.amazon.com/AmazonECR/latest/userguide/service-quotas.html)): `/v2`
  takes a 30 MiB `PATCH`, and whether 4,200 `PATCH`es bound a blob there (33,600 MiB at 8 MiB
  each) is *unverified*; `registries[].uploads.chunkSize` makes the chunks larger.
- **Mounts**: ECR mounts a blob from another repository only when the registry setting
  `BLOB_MOUNTING` is `ENABLED` (`aws ecr put-account-setting --name BLOB_MOUNTING --value
  ENABLED`, per region). Both repositories must be in one account and region, with the same
  encryption type and key; images of a pull-through cache are excluded; and the caller needs
  `ecr:GetDownloadUrlForLayer` on the source ([blob mounting](https://docs.aws.amazon.com/AmazonECR/latest/userguide/blob-mounting.html);
  since 2026-01-20, [announcement](https://aws.amazon.com/about-aws/whats-new/2026/01/amazon-ecr-cross-repository-layer-sharing/)).
  AWS does not state the default; it is `DISABLED` (measured, in four regions of an account
  that never set it). Then a mount is 202 with a new session, and the blob is not in the target
  repository (measured). With the setting on, a mount ECR cannot do yet is answered `202` with a
  session (reported, [ocync](https://clowdhaus.github.io/ocync/registries/ecr)); a mount with it
  on is *unverified*: the run did not change the account's setting. The facade passes a `202` on
  as a plain session.
- **Quotas** ([service quotas](https://docs.aws.amazon.com/AmazonECR/latest/userguide/service-quotas.html)):
  100,000 repositories a region and 100,000 images a repository, both adjustable; 1,000 tags an
  image, fixed. Each manifest is an image to ECR, and so is each referrer, which "counts as an
  image consumed for your repository" ([images](https://docs.aws.amazon.com/AmazonECR/latest/userguide/images.html)).
  The account's quotas say the same (`aws service-quotas`, 2026-11-13), and the fixed ones of the
  API: layer parts of 5 to 10 MiB, 4,200 parts, a layer of 52,000 MiB.
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
schema and cosign's `.sig` included. Lajstrom uses ECR's referrers API (measured, #19) and pushes
no such index; a client that writes the tag schema (cosign without the referrers API, or
`referrers: false`) needs `IMMUTABLE_WITH_EXCLUSION` with `sha256-*`, which keeps those mutable
and Lajstrom's own tags immutable.
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
Measured with this policy (#19), as the only one of a role: every step of the probe, the live
test and Lajstrom, the referrers of `/v2` and `DeleteRepository` with `force` included. The
statement `Catalog` must allow `*`: with `ecr:DescribeRepositories` scoped to
`repository/lajstrom/*`, `DescribeRepositories` without names is `AccessDeniedException`, and
with names below the prefix it works. Which one action the `GET` of `/v2`'s referrers needs is
*unverified* (`ecr:BatchGetImage`, as for `ListImageReferrers`, is likely; the policy above
has it).

## Capabilities

What the provider says for a registry of type `ecr`, and where it comes from; `lajstrom registry
probe` measured each on 2026-11-13 ([Measured on ECR](#measured-on-ecr-19)). The probe's
"Overrides needed" is none.

| Capability | Value | Where it comes from |
| --- | --- | --- |
| Create on push | no | ECR without a creation template for `CREATE_ON_PUSH` (above, measured); `CreateRepository` from `EnsureRepository`; `createOnPush: true` with such a template, which the probe then measures as yes |
| Referrers | yes | measured: the referrers API, with the `artifactType` filter, and `OCI-Subject` on a push with `subject`; the ECR provider says so unless `referrers` is set (#19). The 405 "Invalid JSON syntax" that [aws/containers-roadmap#2783](https://github.com/aws/containers-roadmap/issues/2783) reports for `oras copy -r` is ECR refusing a manifest with a property its schema lacks, there `platform` in `subject` ([Deviations](#deviations-from-the-specification), item 1), not the referrers |
| Catalog | yes | `DescribeRepositories`; `catalog: false` turns it off. ECR's own `_catalog` (above) is not used |
| Delete manifest | yes | measured: a `DELETE` by digest is 202; the 405 for a manifest an index lists refuses that one delete (above) |
| Immutable tags | yes unless `MUTABLE*` | follows `ecr.imageTagMutability`; a repository created otherwise keeps its own setting. Measured: 400 `TAG_INVALID` to another manifest under a tag of an `IMMUTABLE` repository |
| Blob redirect | yes | measured: a `GET` of a blob is 307 to `prod-<region>-starport-layer-bucket.s3.<region>.amazonaws.com`, presigned for 60 minutes; a `HEAD` is answered by ECR itself |
| Max blob size | 52,000 MiB | the maximum layer size of the [quotas](https://docs.aws.amazon.com/AmazonECR/latest/userguide/service-quotas.html) page and of the account's quotas. Since 2026-08-03 AWS takes layers of up to 200 GB "for images pushed via Docker push", and 50 GB through `UploadLayerPart` ([announcement](https://aws.amazon.com/about-aws/whats-new/2026/08/amazon-ecr-image-layers/)). Not measured above 30 MiB; `capabilities.maxBlobSize` raises the bound |
| Consistency | eventual, no bound | AWS documents none, and users and tool maintainers report lags (below); the run saw none (below). `consistencyBound` once one is measured |

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
A Lajstrom that reads a replica or a cache sees them later still. Measured on 2026-11-13 (#19),
in one region without load: each of 40 manifests pushed one after the other was answered by
`HEAD` by digest and by tag at once, as was a blob of 3 MB, and the probe's row
"Read-after-write lag" saw no lag for a manifest, a blob, a tag list or a delete. That bounds
nothing under concurrent pushes. The type `ecr` says `consistency: eventual`, and
without `consistencyBound` the start warns ([Consistency](README.md#consistency)). On such a
registry a publish waits until ECR serves it, a manifest `PUT` answered `MANIFEST_BLOB_UNKNOWN`
and the first write to an upload session answered `BLOB_UPLOAD_UNKNOWN` are sent again within the
bound, and reindex keeps out a version deleted through Lajstrom that ECR still lists
([operations.md](../operations.md#an-eventually-consistent-registry),
[registry-consistency-2026-11.md](../design/registry-consistency-2026-11.md)). None of it is
measured against ECR itself yet: #19.

## Deviations from the specification

Measured on 2026-11-13 against the [distribution specification
v1.1.1](https://github.com/opencontainers/distribution-spec/blob/v1.1.1/spec.md) and the [image
specification v1.1.1](https://github.com/opencontainers/image-spec/blob/v1.1.1/manifest.md).

| # | What ECR does (measured) | What the specification says | What it costs Lajstrom | Our handling |
| --- | --- | --- | --- | --- |
| 1 | Refuses a manifest with a property its schema lacks, 405 `UNSUPPORTED` "Invalid parameter at 'ImageManifest' failed to satisfy constraint: 'Invalid JSON syntax'": `platform` in `subject` or in a layer's descriptor, any other property in a descriptor or at the top. The same manifest without it is 201 | "Implementations processing content SHOULD NOT generate an error if they encounter an unknown property in a known media type" (image spec, Considerations); a manifest refused as invalid is `MANIFEST_INVALID`, and 405 is an operation the registry does not do | an oci proxy repo on ECR cannot store such a manifest of its upstream, nor a client push one: `ghcr.io/buoyantio/extension-init`'s attestations have `platform` in `subject`, and `oras cp -r` of it into ECR fails so (measured). Lajstrom's own manifests have none | none: the push is ECR's 405. What a pull through an oci proxy repo gets then is *unverified*. Upstream: [aws/containers-roadmap#2783](https://github.com/aws/containers-roadmap/issues/2783), open, which reports the 405 without its cause |
| 2 | Every `PATCH` of an upload session is 201 | "Each successful chunk upload MUST have a `202 Accepted` response code" | every chunked upload failed: an oci proxy repo stored no blob | the provider takes 201 (#19) |
| 3 | The `POST` of a session answers `Range: bytes=0-10485760`, its part size, for a session that holds nothing | defines no `Range` for that answer; elsewhere `Range` is `0-<end-of-range>`, the bytes held | the provider read 10485761 bytes held, and its first `PATCH` was 416 | a session a `POST` opened starts at 0 (#19) |
| 4 | Drops the body of a closing `PUT` that follows a `PATCH`, and checks the digest against the bytes of the `PATCH`es | "The final chunk MAY be uploaded using a `PATCH` request or it MAY be uploaded in the closing `PUT` request" | a client's last chunk in the `PUT` would be ECR's 400 | the provider sends it as a `PATCH` of its own before an empty `PUT`, as for JFrog (#352); measured through Lajstrom |
| 5 | The `GET` of an upload session is 404 `404 page not found` | "The response to an active upload `<location>` MUST be a `204 No Content`" with `Range` | a client that resumes after a 416 cannot learn the offset through Lajstrom | none: the facade passes the 404 on |
| 6 | `tags/list` is in an order of ECR's own, and a tag name as `last` is 405 `UNSUPPORTED` "Invalid next token provided": `last` is ECR's opaque token | "the tags MUST be in lexical order"; `<tagname>` "MUST be a proper tag" | a client's `last` through Lajstrom was an error, and a page was not the `n` tags after `last` | the provider reads every page by its `Link` and sorts them (#19), as for GHCR |
| 7 | A manifest that an image index lists: its `DELETE` is 405 `UNSUPPORTED` | "If manifest deletion is disabled, the registry MUST respond with either a `400 Bad Request` or a `405 Method Not Allowed`" | the provider took it for deletes disabled, and refused every delete after it until a restart | one refused delete, `ErrConflict` (#19) |
| 8 | A manifest above 4 MiB is 400 `MANIFEST_INVALID` | "SHOULD respond … with a response code `413 Payload Too Large`" | nothing: the limit is known | none needed |

Not deviations, measured: a manifest with the empty config and no `artifactType` is refused,
rightly ("If the `config.mediaType` is set to the empty value, the `artifactType` MUST be
defined"), with item 1's code and message; `lajstrom registry probe` pushed such manifests
until #19. A manifest without a layer is 405 `UNSUPPORTED` "Image has to have at least 1 layer",
where the image specification says only that `layers` SHOULD have one "for portability": every
manifest of Lajstrom's has one. Chunks below `OCI-Chunk-Min-Length` refused at the closing
`PUT`: the header says so, though ECR takes parts from 5 MB. A chunk out of order is 416, as
specified. A second `PATCH` without `Content-Range` is dropped and the closing `PUT` then fails
its digest: such a `PATCH`, docker's streamed one, is outside the specification; through
Lajstrom one of known length goes with `Content-Range`, and one of unknown length into a session
that holds bytes needs `registries[].uploads.spoolUnknownLength`. `DELETE` of an upload session
(404) and `_catalog` are not in the specification.

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

## Measured on ECR (#19)

2026-11-13, `ap-southeast-1`, as a role whose only policy was the [IAM policy](#iam-policy),
from a laptop in Europe. Every repository, the creation template and the role were deleted at the
end, and the account's ECR and IAM state compared equal to a record taken before.

| Row of the [matrix](README.md#capabilities) | ECR |
| --- | --- |
| Create on push | no: the first upload session to a new name is 404 `NAME_UNKNOWN`; yes below the prefix of a creation template with `CREATE_ON_PUSH` |
| Where a repository may be created | at any depth, inside another repository too; a name of more than 256 bytes is refused by `CreateRepository` |
| Routing words in a name (`manifests`, `referrers`, `tags/list`) | ok: 20 blob `HEAD`s in each name, all 200; `GET` 307 |
| Referrers API | yes, with the `artifactType` filter; no tag schema |
| `OCI-Subject` on a push with `subject` | yes |
| Catalog (`_catalog`) and its limits | yes, not documented: `n` at most 1000 (1001: 405), `last` an opaque token, a name there 405; the provider uses `DescribeRepositories` |
| A listing API of its own | `DescribeRepositories`, pages of up to 1000 with `nextToken` |
| Tag list order and pagination | **in its own order**; **`last` an opaque token**, a tag name 405 `UNSUPPORTED`; `n` with an absolute `Link` |
| Read-after-write lag | none seen: 40 manifests by digest and tag, a blob, a tag list, the deletes |
| Delete a manifest by digest | yes, 202, with its tags; its referrers stay; **405 `UNSUPPORTED`** for a manifest an index lists |
| Delete by tag | the tag; **the manifest too** |
| Delete a repository | no `/v2` API; `DeleteRepository`, with `force` for one with images |
| Immutable tags, where the rule lives | the repository's `imageTagMutability`: 400 `TAG_INVALID` to re-point; the same manifest again 201 |
| Blob redirect | 307 to `prod-ap-southeast-1-starport-layer-bucket.s3.ap-southeast-1.amazonaws.com`, presigned for 60 min; not for `HEAD` |
| `Range` and `Accept-Ranges` | 206, 416 past the end, after the redirect; `Accept-Ranges: bytes` on `GET` only |
| Largest blob measured | 30 MiB, in one `PATCH` and monolithic |
| Manifest size limit | 4 MiB; one byte more is 400 `MANIFEST_INVALID` |
| Lajstrom's manifest profile (`artifactType`, empty config, own media types) | accepted, stored as pushed |
| Auth | Basic `AWS:<token>` of `GetAuthorizationToken`, 12 hours |
| Rate limits seen | none |
| Upload: chunk out of order | 416 `BLOB_UPLOAD_INVALID` |
| Upload: chunk size limit | each part but the last at least 5 MB, refused at the closing `PUT`; 30 MiB in one `PATCH` taken |
| Upload: session status (`GET`) | **404**, plain text |
| Upload: cancel (`DELETE`) | 404, plain text |
| Upload: mount (`POST ?mount=&from=`) | 202, a session, with `BLOB_MOUNTING` `DISABLED` (the default); with it on *not measured* |
| Upload: wrong digest, empty blob | a wrong digest 400 `BLOB_UPLOAD_INVALID`; the empty blob not measured |
| Upload continued by another replica | not measured |
| Garbage collector and an untagged referrer (#37) | not measured |
| What the probe finds | `vendor=ecr referrers=true catalog=true` |
| Overrides needed | none |

What ran:

- `lajstrom registry probe --url https://<account>.dkr.ecr.ap-southeast-1.amazonaws.com
  --registry-prefix lajstrom`, three times: the first measured almost nothing, its manifests
  refused (Deviations, the note); the last, with every fix of #19, the column above. Once more
  with `--registry-prefix lajstrom/cop` and a creation template for that prefix: "Create on
  push: yes". The probe deleted the 22 repositories it created each time.
- `TestIntegrationECRLive`: passed once its manifest had a layer.
- A Lajstrom on ECR (`type: ecr`, prefix `lajstrom/e2e/`, SQLite): `python:3.13-alpine` pulled
  with crane through an oci proxy repo of Docker Hub (its 12.6 MB layer in `PATCH`es of
  8 MiB), pushed with crane to a hosted oci repo, an artifact of 30 MiB pushed and pulled with
  oras, two referrers attached with oras and listed with `oras discover`, with the
  `artifactType` filter, both through Lajstrom and in ECR's referrers API; chunked uploads with
  curl (above); a tag list paged by `n=3`.
- By hand, with curl and the token: the uploads, `_catalog` and `tags/list` pages, the mount,
  the manifests of the Deviations, 40 writes read at once, and `aws ecr` for the template,
  `DescribeRepositories` and the quotas.

Not measured: a blob above 52,000 MiB (and whether 4,200 `PATCH`es bound one), the token past 12
hours (the unit tests cover the refresh), a mount with `BLOB_MOUNTING` on (the run did not
change the account's setting), the image quota, a lag under concurrent pushes, KMS encryption.

## Measure a real ECR (#19)

The runbook of the live check: about an hour for whoever has an account, and a few cents. Each
step deletes what it created. Run on 2026-11-13 ([Measured on ECR](#measured-on-ecr-19)).

**What it takes.**

- A role or user with the [IAM policy](#iam-policy) above, its resource
  `repository/lajstrom/*`: every step works below `lajstrom/`.
- The AWS SDK's credentials in the environment: `AWS_PROFILE`, `AWS_ACCESS_KEY_ID` and
  `AWS_SECRET_ACCESS_KEY`, or an instance role. The region is the one in the registry's host.
- Unverified: whether the probe's `DeleteRepository` with `force` needs more than
  `ecr:DeleteRepository`.

**1. The probe.** Every cell `registry probe` measures, in a new scratch prefix below `lajstrom/`:

```sh
REGISTRY=https://123456789012.dkr.ecr.eu-west-1.amazonaws.com
lajstrom registry probe --url "$REGISTRY" --registry-prefix lajstrom > ecr-probe.md
lajstrom registry probe --url "$REGISTRY" --registry-prefix lajstrom -v -o json > ecr-probe.json   # with the exchanges
```

- `--url` of an ECR host is a registry of type `ecr`. The probe asks the ECR API for the
  authorization token and answers `/v2` with it. `--username` does not go with it.
- A registry of type `ecr` in a configuration works too, with its `ecr` settings:
  `--config lajstrom.yaml --registry <name>`.
- "Create on push" is measured first, and nothing is created for it.
  - When it says no (no creation template with `CREATE_ON_PUSH` covers the prefix), the run
    creates each of its repositories with `CreateRepository` before the first write, as the
    provider does. At the end it deletes them with `DeleteRepository` and `force`.
  - "Delete a repository" says how many went. One that did not is in the report's left-behind
    list.
- With a creation template for `lajstrom/` that has `CREATE_ON_PUSH`, run the probe again:
  "Create on push" should then say yes.

**2. The provider.** It runs `TestIntegrationMoto`'s steps without moto, below
`lajstrom/it-<random>/`, and deletes its repositories at the end:

```sh
LAJSTROM_TEST_ECR_LIVE=123456789012.dkr.ecr.eu-west-1.amazonaws.com \
  go test -tags integration -count=1 -run TestIntegrationECRLive -v ./internal/provider/ecr/
```

**3. By hand,** what neither of them measures:

```sh
HOST=123456789012.dkr.ecr.eu-west-1.amazonaws.com
aws ecr get-login-password | oras login --username AWS --password-stdin "$HOST"
aws ecr create-repository --repository-name lajstrom/manual/hello
oras push "$HOST/lajstrom/manual/hello:v1" ./hello.txt
oras attach --artifact-type application/vnd.example.sig "$HOST/lajstrom/manual/hello:v1" ./sig.txt   # the reported 405?
oras discover "$HOST/lajstrom/manual/hello:v1"
# the 307 of a blob, and how long its URL works: GET the Location again after 5, 15 and 60 minutes
curl -sI -u "AWS:$(aws ecr get-login-password)" "https://$HOST/v2/lajstrom/manual/hello/blobs/<digest>"
aws ecr get-authorization-token --query 'authorizationData[0].expiresAt'   # 12 hours?
aws ecr delete-repository --force --repository-name lajstrom/manual/hello
```

**4. Nothing left.** This should list no repository of the run:

```sh
aws ecr describe-repositories --query "repositories[?starts_with(repositoryName, 'lajstrom/')].repositoryName"
```

**5. Record it.**

- Put the probe's column on this page and in [README.md](README.md)'s row, with the date and
  the region.
- Replace each "to be measured by #19" with what was found, and the overrides the probe names in
  [Configuration](#configuration).
- The quota load test and the lag runs of #19's comments are not part of this hour.
