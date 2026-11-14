# Lajstrom compared with other artifact repositories

- Checked on 2026-11-10: Lajstrom against `main` at `1f4adca` and its open pull requests, with
  group repos, `lajstrom registry probe` and retention brought up to `main` on 2026-11-12, and
  its formats, proxy repos, OIDC, registries and plans brought up to `main` at `c12da6f` on
  2026-11-13; every other product against its own documentation, pricing page, release list or
  source, read on 2026-11-10 and listed under [Sources](#sources).
- For people choosing an artifact repository: what Lajstrom does today, what it does not do,
  and where the others are ahead.

How to read this page:

- A claim about Lajstrom links to where it is decided (an ADR, a spec, the
  [registry matrix](registries/README.md)) and, where there is one, to the test that holds it.
  The guarantees are collected with their tests in [spec/guarantees.md](spec/guarantees.md).
- A claim about another product comes from the pages listed for it under [Sources](#sources),
  all read on 2026-11-10. **?** marks what none of those pages confirmed: treat it as unverified.
- Prices, editions and versions change. Check the source before you decide on one of them.

## 1. What Lajstrom is

An artifact registry that keeps every package in an OCI registry you already run. It speaks
each format's own protocol on the server, so `pip`, `apt-get`, `mvn`, `npm` and `curl` need no
plugin ([ADR 0002](adr/0002-server-side-protocol-emulation.md)). Today it serves hosted repos of
raw files, pypi, apt, maven, npm and rpm ([configuration.md](configuration.md#repos)), `oci`
repos for any OCI artifact or image through `/v2` with its RBAC and audit
([manifest-profile.md](spec/manifest-profile.md) §7), proxy repos of npmjs.org and, as `oci`
proxy repos, of any OCI registry such as Docker Hub, ghcr.io or quay.io, and groups of npm repos
and of `oci` repos ([ADR 0013](adr/0013-proxy-and-group-repos.md),
[ADR 0018](adr/0018-oci-proxy-repos-pull-through-from-any-registry.md),
[ADR 0019](adr/0019-oci-groups-one-member-per-repository-path.md)). It rests on three things.

**Your OCI registry is the only storage, and the index is rebuildable.** Each version of a
package, or each variant of it (an apt architecture, a wheel), is one OCI 1.1 manifest under one
immutable tag, in the package's own OCI repository, in the registry you name: the files are its
layers, the metadata a layer and annotations
([ADR 0001](adr/0001-oci-registry-as-sole-storage.md),
[ADR 0005](adr/0005-manifest-profile-and-immutable-tags.md),
[manifest-profile.md](spec/manifest-profile.md)). Lajstrom stores no artifact bytes. Its
database holds an index that `lajstrom reindex` rebuilds from the tags and annotations in the
registry ([ADR 0006](adr/0006-derived-index-and-control-plane.md),
[operations.md](operations.md#reindex)). What only the database has, the repos, users, tokens,
bindings, keys and audit, is small and is backed up
([operations.md](operations.md#backup-and-restore)). The registry's replication, garbage
collection, encryption, access policies and HA apply to packages as they apply to images, and
the packages stay readable with `oras` or `crane` without Lajstrom
([quickstart/raw.md](quickstart/raw.md) looks at the manifest with `crane`).

**Supply-chain features for every format, because every package is an OCI artifact.**

- Referrers: cosign and notation signatures, SBOMs and attestations attach to a package's
  manifest as OCI referrers. They go through the same ingest path, RBAC and audit as a push, and
  are listed with the registry's referrers API or, on a registry without it, cosign's tag schema
  ([architecture.md](architecture.md#invariants) invariant 3,
  [manifest-profile.md](spec/manifest-profile.md) §6, [test/e2e/cosign.sh][e2e-cosign]).
- Immutable tags: one tag per version and variant, never re-pointed; a push of other bytes under
  it is `409` ([ADR 0005](adr/0005-manifest-profile-and-immutable-tags.md),
  [test/e2e/oras.sh][e2e-oras]).
- Digest verification: the facade hashes every blob uploaded through `/v2` as it streams to the
  registry and refuses one whose bytes do not have the digest the client named
  ([threat model](security/threat-model.md) P-9, `TestUploadVerifiesDigest` in
  `internal/facade`).
- apt repos are also signed with a GPG key of their own ([operations.md](operations.md#repo-keys)).

**One stateless Go binary with HA, Apache-2.0, and measured registry behaviour.**

- N identical replicas behind one edge on one Postgres, with no shared volume: the registry
  holds the bytes and the database the rest ([deploy/kubernetes.md](deploy/kubernetes.md),
  [test/e2e/ha.sh][e2e-ha]). SQLite is for one node.
- Apache-2.0 and one edition: HA, RBAC and audit are not paid extras
  ([LICENSE](../LICENSE), [ADR 0003](adr/0003-license-and-contribution-terms.md)).
- What Lajstrom does on each registry is measured, not assumed: the
  [capability matrix](registries/README.md#capabilities) records 31 behaviours of distribution,
  Zot, the GitLab container registry, GHCR, Harbor, Nexus, Gitea and Forgejo, and in tables of
  their own JFrog Container Registry, Project Quay and Amazon ECR. Lajstrom probes each
  registry at startup ([configuration.md](configuration.md#registry-capabilities)), and
  `lajstrom registry probe` measures a registry's column for you
  ([registries/README.md](registries/README.md#measuring-a-registry-lajstrom-registry-probe)).

## 2. Feature matrix

Products are the rows, in three tables. Each product's name links to its sources. zot and CNCF
distribution are not here: they are OCI registries that Lajstrom runs on. So are Harbor, Nexus,
the GitLab container registry and Gitea/Forgejo, which makes them as much a complement as an
alternative ([registries/README.md](registries/README.md)).

### 2.1 Formats, storage, rebuild, HA

| Product | Formats | Where the bytes live | Index rebuilt from storage | HA, and what it needs |
| --- | --- | --- | --- | --- |
| **Lajstrom** | 6: raw, pypi, apt, maven, npm, rpm; any OCI artifact through `oci` repos | the OCI registry you run; no storage of its own ([ADR 0001](adr/0001-oci-registry-as-sole-storage.md)) | yes, `lajstrom reindex` from tags and annotations; the control plane is backed up ([ADR 0006](adr/0006-derived-index-and-control-plane.md)) | yes: N stateless replicas and Postgres, no shared volume ([kubernetes.md](deploy/kubernetes.md)) |
| [Artifact Keeper](#artifact-keeper) | 45+, and WASM format plugins | filesystem, S3, Azure Blob, GCS | no: metadata only in Postgres; a repair endpoint for Maven repos only | possible, not the default: the chart ships 1 replica with `Recreate`; more need RWX or object storage and flags |
| [NORA](#nora) | 17 | local disk, S3, GCS | yes: no database; the in-memory index is rebuilt at startup from its own storage | none, "by design" |
| [OpenArtifact](#openartifact) | 23 | filesystem, S3, GCS, Azure, or an OCI registry used as a bare blob store | partly: package rows from sidecar journal blobs, refused on the OCI backend; users, permissions, tokens and audit not recovered | `web` replicas and workers on one Postgres |
| [Nexus Repository](#sonatype-nexus-repository) CE, Pro | about 25, all in CE; the EPL source has only maven, raw and apt | file or S3; Azure and GCS in Pro (Sonatype's pages disagree on S3 in CE: ?) | partly: the "Data Repair Plan" recreates records from blob `.properties` files, 13 formats; after a total DB loss: ? | Pro and up only: external Postgres, a shared blob store, a load balancer |
| [Artifactory](#jfrog-artifactory) OSS, JCR, Pro X, Enterprise X, Enterprise+ | 40+ in the paid editions; OSS: Maven, Gradle, Ivy, SBT, Generic; JCR: Docker, OCI, Helm, Generic | filesystem or DB in OSS; S3, GCS, Azure from JCR and Pro X; sharding from Enterprise X | no: "the filestore is just a collection of checksum-named files that cannot be identified or restored" without the DB | Enterprise X and up: external DB, shared filestore, load balancer |
| [Harbor](#harbor) | OCI only: images, Helm charts as OCI, other OCI artifacts | distribution v2.8 drivers: filesystem, S3, Azure, GCS, Swift, OSS | no documented way: Postgres holds projects, users, policies, scans, audit | yes: stateless services, external Postgres and Redis/Valkey, RWX or object storage |
| [Verdaccio](#verdaccio) | npm only | local filesystem; S3 and GCS through plugins | partly: package metadata is a `package.json` beside the tarballs | one process by default; several need the S3 (with DynamoDB) storage plugin and non-file auth |
| [Pulp](#pulp) | about 10 plugins: rpm, deb, python, container, ansible, maven, npm, gem, file, Hugging Face | filesystem, S3, Azure (GCS untested by Pulp) | no: Postgres is the source of truth | yes: api, content and worker processes scale out on Postgres |
| [Gitea, Forgejo](#gitea-and-forgejo) | 23, 23 | Gitea: local, S3, Azure; Forgejo: local, S3 | ? (not documented) | Gitea: its Helm chart, with RWX storage, valkey, meilisearch; Forgejo: its chart is "not HA-ready yet" |
| [GitLab Package Registry](#gitlab-package-registry) | 14 (6 GA: generic, Helm, Maven, npm, NuGet, PyPI), and the container registry | local or object storage | no for packages: the DB is authoritative; the container registry's metadata DB imports existing storage once | reference architectures with HA from 3,000 users (Rails, Sidekiq, Gitaly, Postgres, Redis) |
| [Cloudsmith](#cloudsmith) (SaaS) | 33 | the vendor's | n/a | the vendor's |
| [ProGet](#inedo-proget) | about 22 feed types and Docker, all in Free | disk; S3, Azure, GCS in paid editions | ? | Enterprise only: shared SQL Server, SMB/NAS, a load balancer |

### 2.2 License, price, identity, audit

| Product | License | Paid tiers | Auth: local, LDAP, OIDC, SAML | RBAC granularity | Audit |
| --- | --- | --- | --- | --- | --- |
| **Lajstrom** | Apache-2.0; contributors sign a [CLA](../CLA.md) that also allows another OSI-approved licence ([ADR 0003](adr/0003-license-and-contribution-terms.md)) | none | local users, personal access tokens with a required expiry, deploy tokens for one repo; OIDC login with one provider, its groups bound to roles ([auth.md](auth.md#oidc-login-and-groups)); **no LDAP or SAML yet** ([auth.md](auth.md)) | a role on a repo mask with whole-segment wildcards (`apt/*`, `*/payments`); 4 roles; token scopes ([rbac.md](spec/rbac.md)) | every write and control-plane change, in the change's own transaction; reads are not ([operations.md](operations.md#audit)) |
| [Artifact Keeper](#artifact-keeper) | MIT | none (GitHub Sponsors) | local, LDAP, OIDC, SAML 2.0, TOTP | global roles, and read, write, delete, admin per repo for users or groups; per path: ? | yes |
| [NORA](#nora) | MIT | none (a paid edition behind its "Community Edition" naming: ?) | htpasswd and API tokens; OIDC for CI workloads only; no LDAP or SSO | one global role per token (read, write, admin); no per-repo RBAC | append-only JSONL |
| [OpenArtifact](#openartifact) | AGPL-3.0-or-later | none found | local, OIDC, SAML; LDAP through Dex | permission targets: a regex on the repository and on the path | yes |
| [Nexus Repository](#sonatype-nexus-repository) | EPL-1.0 source for the core; the CE and Pro binaries are proprietary | CE $0, capped at 40,000 components and 100,000 requests a day (no new components above it); Pro from $1,950/yr (cloud) or $7,500/yr (self-hosted); Pro+ and Enterprise custom | CE: local, LDAP; Pro: SAML, OIDC, Crowd | privileges, roles, content selectors by path (CE) | `audit.log` in CE; the audit API in Pro |
| [Artifactory](#jfrog-artifactory) | OSS: AGPL-3.0-or-later; JCR and the paid editions proprietary | SaaS Pro $150/month; Pro X from $27,000/yr; Enterprise X from $51,000/yr; Enterprise+ custom; no free cloud tier (a 14-day trial) | LDAP in all; SAML from Pro X; more providers (OAuth, Crowd) from Enterprise X | permission targets with path include and exclude patterns | yes; differences by edition: ? |
| [Harbor](#harbor) | Apache-2.0 | none; commercial distributions by third parties | database, LDAP/AD, OIDC | 5 project roles, custom roles (2.16), robot accounts | yes, and forwarding to an endpoint |
| [Verdaccio](#verdaccio) | MIT | none | htpasswd and plugins (LDAP or OIDC plugins: ?) | per package pattern: access, publish, unpublish | ? |
| [Pulp](#pulp) | GPL-2.0-or-later | none | local, LDAP through the web server, OIDC (Keycloak), SAML (preview), CI workload identity | access policies per endpoint, object-level permissions | ? |
| [Gitea, Forgejo](#gitea-and-forgejo) | MIT, GPL-3.0 | Gitea Enterprise and Cloud prices: ?; Forgejo none | LDAP, PAM, OIDC, SSPI; no SAML | per owner or organisation; tokens with a `package` scope | Gitea v28: off by default; Forgejo: ? |
| [GitLab Package Registry](#gitlab-package-registry) | open core | Free; Premium $29 per user a month; Ultimate custom | LDAP, SAML, OAuth 2.0 and OmniAuth providers, Kerberos; the tier of each: ? | project and group roles (?) | audit events in Premium |
| [Cloudsmith](#cloudsmith) | proprietary | Core $0 (500 MB); Pro $149/month; Ultra and Enterprise custom; a free plan for open source (from an indexed copy dated 2026-06-11: ?) | SAML with SCIM; OIDC for CI | ? | ? |
| [ProGet](#inedo-proget) | proprietary | Free; Basic $2,395; Enterprise Essentials $11,995; Enterprise Complete from $29,995; per server a year | built-in users; LDAP/AD from Basic; SAML in Enterprise; OIDC: ? | per feed from Basic; none in Free | ? |

### 2.3 Proxy, groups, signing, operations

| Product | Proxy and cache | Groups, virtual repos | Signing; OCI referrers | GitOps, declarative | Footprint |
| --- | --- | --- | --- | --- | --- |
| **Lajstrom** | npm (npmjs.org), and `oci` proxy repos, a pull-through cache of any OCI registry: Docker Hub, ghcr.io, quay.io, ECR, Artifact Registry ([proxy-and-group.md](spec/proxy-and-group.md) §11); allow and deny name patterns, metadata TTLs, an offline mode, one upstream fetch per file across replicas, `npm audit` forwarded ([proxy-and-group.md](spec/proxy-and-group.md)); none for pypi, maven, apt or rpm | npm and `oci`: npm's hosted repo and a proxy under one URL, where a name the hosted repo holds hides the upstream's versions ([proxy-and-group.md](spec/proxy-and-group.md) §8); an `oci` group, read-only, routes each repository path to one member (§12) | cosign, notation, SBOMs and attestations as referrers of any package, with a tag-schema fallback; apt and rpm metadata GPG-signed | a YAML bootstrap that creates repos and never changes or deletes one ([configuration.md](configuration.md#repos)); REST API; `lajstrom admin` CLI; Kubernetes manifests | one Go binary, about 65 MiB resident idle and 66 MiB with 8 × 1 GiB uploads in flight ([operations.md](operations.md#the-load-test)); Postgres or SQLite; your registry |
| [Artifact Keeper](#artifact-keeper) | remote repos with freshness rules | virtual repos | GPG, RSA or ed25519 signing of rpm, deb, apk, pacman and conda metadata; stores cosign signatures; referrers API | export and apply of repository settings; Helm chart, Argo CD | Rust backend with Postgres, OpenSearch and optional scanners; a 2 GiB limit in its chart |
| [NORA](#nora) | most formats, with curation: block and allow lists, a minimum age, quarantine, OSV | no | rpm and deb indexes signed with OpenPGP; no referrers API | a config file, and curation rules as files in git; hot reload | one static binary of about 23 MB, about 15 MB resident idle; no database |
| [OpenArtifact](#openartifact) | 22 of its 23 formats | yes | apt and rpm metadata GPG-signed; referrers: a route exists in its code, ? | `oa apply`, `oa diff`, `oa export --prune` over users, groups, repos, permissions, SSO | one Go binary and Postgres |
| [Nexus Repository](#sonatype-nexus-repository) | yes, in CE | yes (Docker group deploy is Pro) | apt and yum metadata GPG-signed; referrers in `oci` repos (3.94+, CE) | REST API; community Terraform providers (?) | JVM; the smallest profile is 4 vCPU and 16 GiB |
| [Artifactory](#jfrog-artifactory) | remote repos in every edition | virtual repos in every edition | metadata GPG (Pro), JAR signing (Pro X), Evidence: DSSE and in-toto on any artifact (Pro and up); referrers since 7.90.1 | YAML configuration API, an official Terraform provider, Helm charts | JVM; 4 cores and 6 GB for up to 20 clients; Postgres |
| [Harbor](#harbor) | proxy-cache projects for 9 registry types, read-only | no | cosign and notation; a "signed images only" pull policy (whether it checks the signature cryptographically: ?); referrers since 2.11 | `harbor.yml`, Helm chart; a Terraform provider (?) | about 11 services; at least 2 CPU and 4 GB |
| [Verdaccio](#verdaccio) | uplinks per package pattern | no named groups: uplinks are merged | relays upstream signatures and provenance (experimental, 7.1.0); local packages unsigned | `config.yaml`; a Helm chart (?) | Node.js, one process |
| [Pulp](#pulp) | on-demand sync; container pull-through in tech preview | ? | metadata signing services; no referrers API ([pulp_container#1169][pulp-referrers]) | an operator and Ansible modules (?) | Python; api, content and workers at 1–3 GB each; Postgres |
| [Gitea, Forgejo](#gitea-and-forgejo) | no ([go-gitea#21223][gitea-proxy] is open) | no | Debian repos PGP-signed; no referrers API | REST API; Terraform providers (?) | one Go binary and a database |
| [GitLab Package Registry](#gitlab-package-registry) | dependency proxy for Docker Hub (all tiers); npm, PyPI and Maven forwarding (Premium) | virtual registry, beta, Maven and containers (Premium) | shows cosign signatures of images; "does not fully implement" the referrers API | Terraform provider (?) | Rails, Sidekiq, Gitaly, Postgres, Redis, the registry |
| [Cloudsmith](#cloudsmith) | upstreams: proxy only, cache and proxy, cache only | ? | repository GPG and RSA keys; every Docker image pushed is signed with cosign; referrers since 2024-11-19 | Terraform provider (?) | SaaS |
| [ProGet](#inedo-proget) | connectors; filters and caching in paid editions | ? | ? | APIs, `pgutil` (?) | .NET (?) with SQL Server or an embedded Postgres |

## 3. Where Lajstrom is different

Only what can be checked. Each point says what holds, where it is decided and tested, and how
the others compare. Not every point is unique to Lajstrom, and each says so where it is not.

### 3.1 Packages are OCI artifacts in your registry

- **What holds.** No artifact byte is stored outside the registry. A version, or a variant of
  it, is a self-describing OCI 1.1 manifest (`artifactType`, `lajstrom.*` annotations, the file
  name in `org.opencontainers.image.title`), so another OCI tool reads it without Lajstrom.
  Files that an npm proxy repo fetches are written the same way, through the same ingest path
  ([ADR 0013](adr/0013-proxy-and-group-repos.md)); an `oci` proxy repo keeps the upstream's own
  manifests and blobs unchanged, since a manifest of the profile would change the digest the
  client asked for ([ADR 0018](adr/0018-oci-proxy-repos-pull-through-from-any-registry.md)).
- **Check it.** [ADR 0001](adr/0001-oci-registry-as-sole-storage.md),
  [manifest-profile.md](spec/manifest-profile.md); [quickstart/raw.md](quickstart/raw.md)
  pushes a file with `curl -T` and reads its manifest with `crane`.
- **The others.** Artifact Keeper, NORA, Nexus, Artifactory, Pulp, Gitea, Forgejo, GitLab and
  ProGet keep the bytes in a filesystem or an object store that they own. The nearest case is
  OpenArtifact, which offers an OCI registry as one of its blob stores ([storage][oa-storage]):
  it writes bare blobs into one OCI repository, with no manifests and no tags
  ([`internal/storage/oci.go`][oa-oci]); that backend cannot list, and its rebuild from storage
  refuses it ([rebuild from storage][oa-rebuild]). So "the only product that stores packages in
  an OCI registry" is not true. What differs is one manifest per version, readable by other OCI
  tools, with an index rebuilt from it. A registry's garbage collector deletes blobs that no
  manifest references ([distribution's GC][dist-gc]), which would include such bare blobs unless
  GC is off for that repository: an inference from that page, not tested.

### 3.2 The index is rebuilt from the registry

- **What holds.** `lajstrom reindex` marks and sweeps while Lajstrom serves, reads only digests
  it has not seen, reports a tag that names another digest as drift and never re-points it, and
  attaches a prefix that CI filled with `oras push` before Lajstrom saw it. The reconcile loop
  runs it every `reconcile.interval` (1 hour by default), so a push or a delete made straight in
  the registry reaches the index. 10,000 raw manifests rebuild from an empty index in 6.1 s on
  Zot with SQLite; Postgres is not measured yet. The control plane is not derived and needs a
  backup, as it does everywhere else.
- **Check it.** [ADR 0006](adr/0006-derived-index-and-control-plane.md),
  [index-schema.md](spec/index-schema.md), [operations.md](operations.md#reindex) and
  [Reconcile](operations.md#reconcile); `TestRebuild`, `TestBypassAndDelete`, `TestDrift`,
  `TestAttach` and `TestReconcile` in `internal/index/reindex`.
- **The others.** NORA has no database and rebuilds its in-memory index from its own disk or
  bucket at startup ([ARCHITECTURE.md][nora-arch]): the same property, on storage it owns.
  Nexus's "Data Repair Plan" recreates records from per-blob `.properties` files for 13 formats
  ([repair tasks][nx-repair]). OpenArtifact rebuilds package rows from a sidecar journal, not on
  its OCI backend ([rebuild][oa-rebuild]). Artifact Keeper repairs Maven repos only
  ([admin repair endpoints][ak-repair]). Artifactory: "Without the database, the filestore is
  just a collection of checksum-named files that cannot be identified or restored"
  ([backups][jf-backups]). Harbor and Pulp keep their truth in the database and document no
  rebuild ([Harbor backup][hb-backup], [Pulp architecture][pulp-arch]). GitLab's container
  registry imports existing storage into its metadata database once, tagged images only
  ([metadata database][gl-metadata]).

### 3.3 Signatures, attestations and immutability for every format

- **What holds.** A cosign or notation signature, an SBOM or an attestation of a wheel, a
  `.deb`, a jar or an npm tarball is an OCI referrer of that package's manifest, so `cosign
  verify` or an admission policy checks it as it would an image's. It goes through the same
  RBAC and audit as a push: a reader cannot attach one. Tags are immutable and append-only. A
  blob uploaded through `/v2` is hashed as it streams, so a registry that checks no digest
  (JFrog JCR 7.161, [jfrog-jcr.md](registries/jfrog-jcr.md)) cannot be made to store a wrong
  one through Lajstrom.
- **Check it.** [architecture.md](architecture.md#invariants) invariant 3,
  [manifest-profile.md](spec/manifest-profile.md) §6,
  [ADR 0005](adr/0005-manifest-profile-and-immutable-tags.md),
  [threat model](security/threat-model.md) P-9. [test/e2e/cosign.sh][e2e-cosign] signs a raw
  file with cosign, verifies it, lists the signature with `oras discover` and the referrers API
  or the tag schema, refuses a reader's signature, and finds it again after `reindex`; the
  referrer path does not depend on the format. notation signatures, SPDX, CycloneDX and in-toto
  are recognised by media type in unit tests (`internal/oci/profile/referrer_test.go`); the e2e
  suite does not run the notation CLI. [test/e2e/oras.sh][e2e-oras] and `raw.sh` check the `409`
  on a second push under a tag; `TestUploadVerifiesDigest` checks the digest.
- **The others.** Harbor (since 2.11), Nexus `oci` repos (3.94+), Artifactory (7.90.1+),
  Cloudsmith and Artifact Keeper have the referrers API for what is in their OCI or Docker
  repositories ([Harbor v2.11.0][hb-211], [Nexus OCI][nx-oci], [Artifactory OCI][jf-oci],
  [Cloudsmith][cs-referrers], Artifact Keeper's `oci_v2.rs` in [its repository][ak-repo]);
  distribution ([#3716][dist-referrers]), Pulp ([#1169][pulp-referrers]) and NORA
  ([COMPAT.md][nora-compat]) do not, and GitLab "does not fully implement" it
  ([container registry][gl-container]). For
  their other formats, signing means GPG on the repository metadata. The closest to signatures
  on any artifact is JFrog Evidence: DSSE and in-toto on any artifact, in Pro and up, kept in
  JFrog's own store and API rather than as OCI referrers ([Evidence][jf-evidence]).

### 3.4 HA in the only edition, without a shared volume

- **What holds.** Replicas keep no state of their own across requests: tag claims, upload
  sessions, jobs and the bearer-token signing key are in Postgres, the bytes in the registry. No
  RWX volume is needed.
- **Check it.** [roadmap.md](roadmap.md) ("Multi-replica from day one"),
  [ADR 0010](adr/0010-bearer-token-signing-key.md), [deploy/kubernetes.md](deploy/kubernetes.md)
  (two replicas). [test/e2e/ha.sh][e2e-ha] runs two replicas behind nginx on one Postgres: a
  push through one replica read through the other, the same tag pushed to both at once (one
  `201`, one `409`), a chunked upload continued on the other replica, and both replicas
  restarted in turn under a loop of `pip install` with no failed request
  ([acceptance.md](acceptance.md) §4). It runs with `make e2e` on the dev stack's `ha` profile,
  and in the HA workflow every night and on pull requests that change ingest, the index, the
  facade or its own setup, as a check that is not required
  ([testing.md](testing.md#the-ha-stack)).
  [kubernetes.md](deploy/kubernetes.md#what-was-not-verified) lists what was not verified: a
  multi-node cluster, a managed Postgres, upgrades.
- **The others.** Not unique: Harbor, Pulp and OpenArtifact have HA free too
  ([Harbor HA][hb-ha], [Pulp architecture][pulp-arch], [OpenArtifact scaling][oa-scaling]).
  It is paid in Nexus (Pro, [editions][nx-editions]), Artifactory (Enterprise X,
  [feature matrix][jf-matrix]) and ProGet (Enterprise, [HA][pg-ha]). NORA has none by design
  ([ARCHITECTURE.md][nora-arch]); Forgejo's chart is "not HA-ready yet" ([HA][fj-ha]); Gitea
  ([HA][gt-ha]) and Artifact Keeper ([proxy cache HA][ak-ha]) need RWX or object storage;
  Verdaccio needs its S3 and DynamoDB plugin ([scaling][vd-scaling]).

### 3.5 Apache-2.0, no open core

- **What holds.** There is one edition, and HA, RBAC and audit are in it. The README promises
  that the licence stays Apache-2.0. Contributors sign a CLA that lets the project owner also
  license their contributions under another OSI-approved licence.
- **Check it.** [LICENSE](../LICENSE), [ADR 0003](adr/0003-license-and-contribution-terms.md),
  [CLA.md](../CLA.md) §2, the README's "Our promise to users".
- **The others.** Nexus Community Edition is a proprietary binary capped at 40,000 components
  and 100,000 requests a day ([3.87.0 notes][nx-387], [usage center][nx-usage]); its EPL source
  has only maven, raw and apt ([nexus-public][nx-public]); SAML and OIDC are Pro
  ([realms][nx-realms], [editions][nx-editions]). Artifactory OSS is AGPL-3.0 with Maven,
  Gradle, Ivy, SBT and Generic ([feature matrix][jf-matrix], [sources][jf-oss]); SAML needs
  Pro X and HA Enterprise X. ProGet
  Free has no feed permissions, LDAP, HA or retention ([license][pg-license]). GitLab puts audit
  events and package forwarding in Premium ([pricing][gl-pricing],
  [virtual registry][gl-virtual]). Open source without an open core, too: Artifact Keeper and
  NORA (MIT), OpenArtifact (AGPL-3.0), Pulp (GPL-2.0), Harbor (Apache-2.0), Verdaccio (MIT).

### 3.6 Registry behaviour is measured

- **What holds.** Lajstrom runs on other registries, so how each behaves is part of the product.
  The [matrix](registries/README.md) records 31 behaviours per registry (create on push,
  referrers, catalog, deletes, immutable tags, blob redirects, upload sessions, garbage
  collection of referrers and more) and lists each registry's deviations from the OCI
  specification. distribution 3.1.2 and Zot v2.1.23 run in CI on every pull request with code;
  the GitLab container registry, GHCR, Harbor 2.15.4, Nexus 3.98.1 CE, Gitea 1.27.3, Forgejo
  17.0.1, JFrog Container Registry 7.161.26, Project Quay 3.18.2 and Amazon ECR were measured by
  hand on the dates the matrix gives.
- **Check it.** [registries/README.md](registries/README.md#versions-tested); the startup probe
  and the vendor table in [configuration.md](configuration.md#registry-capabilities);
  `lajstrom registry probe`, which measures a registry's column
  ([registries/README.md](registries/README.md#measuring-a-registry-lajstrom-registry-probe)).
- **The others.** None of the pages under [Sources](#sources) documents how a product behaves
  on particular OCI registries. The one product that can use a registry, OpenArtifact, uses only
  the blob endpoints of `/v2` ([`internal/storage/oci.go`][oa-oci]).

### 3.7 Footprint, measured

- **What holds.** About 65 MiB resident when idle, 66 MiB at the peak with 8 uploads of 1 GiB
  in flight; bodies are streamed, never held in memory. Measured by the Load workflow on a
  GitHub runner on 2026-11-10, with SQLite and Zot.
- **Check it.** [operations.md](operations.md#the-load-test); `TestStreamingMemory` in
  `internal/facade`.
- **The others.** These are the vendors' stated requirements, not measurements under the same
  load: Nexus's smallest profile is 4 vCPU and 16 GiB ([system requirements][nx-sysreq]);
  Artifactory needs 4 cores and 6 GB for up to 20 clients ([sizing][jf-sizing]); Harbor at
  least 2 CPU and 4 GB across about 11 services ([prerequisites][hb-prereq]); Pulp 1–3 GB per
  worker ([architecture][pulp-arch]). NORA is smaller, about 15 MB resident idle, on one node
  without a database ([BENCHMARKS.md][nora-bench]).

## 4. Where Lajstrom is not the right choice today

- **You need many formats.** Lajstrom has 6 (raw, pypi, apt, maven, npm, rpm) and OCI artifacts.
  It has no terraform, go, cargo, nuget, gems or conda. A Helm chart goes into an `oci` repo as
  an OCI artifact; no `index.yaml` repository is served, and `helm` itself is not in the tests.
  Artifact Keeper has 45+, Artifactory 40+ in its paid editions, Cloudsmith 33, Nexus about 25,
  OpenArtifact, Gitea and Forgejo 23 each, ProGet about 22, NORA 17 (see the matrix).
- **You need a web UI.** There is none yet. A read-only UI is being built in its own repository,
  [lajstrom-ui](https://github.com/lajstrom/lajstrom-ui) ([#70]).
- **You need LDAP or SAML.** Besides local users and tokens there is OIDC login with one
  provider, whose groups bind to roles ([#69], [auth.md](auth.md#oidc-login-and-groups)); there
  is no LDAP or SAML.
- **You need a pull-through cache for everything.** Proxy and group repos exist for npm and for
  `oci` repos: an `oci` proxy is a pull-through cache of Docker Hub, ghcr.io, quay.io or any
  other OCI registry ([#442], [quickstart/oci-proxy.md](quickstart/oci-proxy.md)). pypi, maven,
  apt and rpm have none; pypi and maven proxies and groups are post-MVP and not scheduled
  ([#67], [#68]). Nexus, Artifactory and NORA do this today.
- **You need a released, settled product.** The repository was created on 2026-11-05 and has
  no release: v0.1.0 is due on 2026-12-04 ([roadmap.md](roadmap.md)). While it is 0.x, the
  HTTP API, the configuration and the CLI may change between versions (README, "Our promise to
  users"). The others have shipped for longer: Artifact Keeper is at v1.11.13, NORA v1.4.4,
  OpenArtifact v0.23.0, Harbor v2.16.0, Nexus 3.98.1, Artifactory 7.173.18 (see Sources).
- **Your registry is not in the matrix yet, or is in it with a limit**
  ([registries/README.md](registries/README.md)):
  - ECR was measured live, by hand, on 2026-11-13 ([#19],
    [ecr.md](registries/ecr.md#measured-on-ecr-19)); no ECR runs in CI. It refuses a manifest
    with a property its schema lacks, which an `oci` proxy repo meets in some upstreams'
    attestations ([registries/README.md](registries/README.md#amazon-ecr)). ACR and Docker Hub
    are untested as backends, Google Artifact Registry too.
  - JFrog JCR and Project Quay are measured, and each is "a backend for trying Lajstrom, not for
    data": JCR while a rebuild from an empty index finds no repository (it has no catalog
    Lajstrom uses) and Lajstrom runs with its admin's token, the work-arounds of #352, #368 and
    #813 holding since 2026-11-13 ([jfrog-jcr.md](registries/jfrog-jcr.md#again-after-813)), Quay while a referrer pushed by
    digest cannot be deleted and a rebuild from an empty index finds no repository
    ([quay.md](registries/quay.md#our-tests-against-it)).
  - GHCR cannot delete through `/v2`, so Lajstrom cannot delete versions there.
  - The GitLab container registry with its metadata database refuses Lajstrom's manifests unless
    `REGISTRY_FF_DYNAMIC_MEDIA_TYPES` is on; on GitLab.com users cannot set it
    ([gitlab.md](registries/gitlab.md)), and GitLab.com itself is not tested.
  - The GitLab registry in database mode and Nexus's "Delete unused manifests" task delete
    untagged referrers (the matrix's garbage-collection row).
  - Harbor 2.16.0 (released on 2026-11-10) and Gitea's 28.x line are newer than the versions
    the matrix measured (2.15.4 and 1.27.3), and are not measured.
- **Your registry's quotas become Lajstrom's.** Each package is an OCI repository and each
  version a manifest push, so registry limits apply, for instance ECR's default 100,000
  repositories per region and 10 `PutImage` calls a second, both adjustable
  ([ECR quotas][ecr-quotas]). [#19] measured ECR without load and read the account's quotas;
  Lajstrom under load against them, and the image quota, are not measured
  ([ecr.md](registries/ecr.md#measured-on-ecr-19)).
- **You want everything declarative.** The `repos[]` bootstrap creates repos and never changes or
  deletes one; there is no Terraform provider. The Helm chart is published with each release from
  v0.1.0 on ([#82]), but its values are a deployment, not a set of repos.
  OpenArtifact's `oa apply --prune` and Artifactory's Terraform provider go further.
- **You need curation, webhooks or encryption at rest now.** For curation the npm and `oci`
  proxies have allow and deny name patterns, nothing more. Deny rules and quarantine are decided
  ([ADR 0008](adr/0008-policy-verdicts-and-enforcement.md)) but not built; NORA, Artifact
  Keeper and OpenArtifact ship curation free. Webhooks are [#77]. Encryption at rest is decided
  ([ADR 0009](adr/0009-encryption-at-rest-in-the-registry.md)) and not built ([#114]).
- **You want the smallest possible single-node cache.** Lajstrom needs a registry and Postgres
  (or SQLite). NORA is one binary without a database; for npm alone, Verdaccio.

## 5. By design, not in the server: retention rules and vulnerability scanning

The server has no retention rules and no vulnerability scanner of its own, on purpose. Retention
is a job outside it that Lajstrom ships, `lajstrom admin retention` (below):

- **The registry owns the bytes, so it owns their lifecycle.** Freeing space is the registry's
  garbage collection, never Lajstrom's ([ADR 0001](adr/0001-oci-registry-as-sole-storage.md),
  [architecture.md](architecture.md)). Registries such as ECR and Harbor already have lifecycle
  rules (below), and such a rule applies to packages the same way it applies to images.
- **Whether to run something is the consumer's decision.**
  [ADR 0008](adr/0008-policy-verdicts-and-enforcement.md) leaves the decision on OCI content to
  the tools that have the context: admission controllers, the Sigstore policy controller,
  Kyverno. A scanner's verdict is an attestation the scanner signs and attaches. Lajstrom's
  part, as decided, is to keep and publish it, and, for apt, pip, mvn and npm, which have no
  policy hook of their own, to apply deny and quarantine rules (decided, not built).
- **A scanner in the core would be a second copy** of tools the team already runs, with its own
  vulnerability database to keep current. That is the reasoning behind the decision, not a
  measurement.

How to do each today:

- **Retention with `lajstrom admin retention`.** Keep the last N versions of each package, drop
  those older than a maximum age, per repo and package pattern; a dry run unless `--apply`. It
  is a client of the management API that a CronJob, a CI schedule or cron runs, and deletes
  through the conditional batch delete, `POST …/versions:delete`, so a version that changed
  after the job read it is not deleted; each deletion has an audit row with the job's id
  ([operations.md](operations.md#retention)).
- **Retention through Lajstrom's delete API.** The building block of that job, for a policy of
  your own: `DELETE /api/v1/repos/<format>/<name>/packages/<package>/versions/<version>` deletes
  a version's referrers first, then its manifests, its rows and the rendered indexes, and writes
  an audit row per manifest; it needs the `maintainer` role ([api.md](api.md)). On a registry
  that cannot delete manifests (GHCR, distribution with deletes off) it answers `501` and
  changes nothing.
- **Retention through the registry's own rules.** For example ECR lifecycle policies, by count
  or age and by tag pattern, which also expire the referrers of a deleted image
  ([ECR lifecycle policies][ecr-lifecycle]), or Harbor's tag retention rules, by count, days and
  tag pattern per repository ([Harbor tag retention][hb-retention]). Each package is its own
  OCI repository, so "keep the newest N" is per package. What the registry deletes leaves the
  index at the next reconcile ([operations.md](operations.md#reconcile)). Such a rule sees tags,
  not versions: Lajstrom's npm dist-tag journal (`_j…` tags,
  [ADR 0012](adr/0012-npm-profile-routes-and-dist-tags-journal.md)) and cosign's `sha256-…` tags
  live in the same OCI repository, so exclude them, and preview the rule first
  ([operations.md](operations.md#the-registrys-own-rules) has what to exclude per registry). This
  route is not tested against Lajstrom's layout.
- **Scanning.** Get the package with `oras pull` or its own client, or the SBOM attached to it,
  and scan it with the scanner you run: Trivy reads CycloneDX and SPDX SBOMs
  ([Trivy SBOM][trivy-sbom]), Grype scans images, directories and SBOMs ([Grype][grype]).
  Attach the report or the SBOM back to the package as a referrer through Lajstrom. Harbor's
  built-in scanner scans the images in a Harbor project; whether it scans Lajstrom's package
  manifests is not tested (?).

## 6. Planned

As of 2026-11-13, `main` at `c12da6f`. Four rows of 2026-11-10 are on `main` and left the
table: OIDC login with groups from claims ([#69]), rpm ([#64]), the pull-through cache of Docker
Hub, ghcr.io and quay.io ([#442]) and the live ECR measurements ([#19]); so did group repos and
`lajstrom registry probe` before them. The issues are the truth for status
([roadmap.md](roadmap.md)).

| What | State | Tracking |
| --- | --- | --- |
| Read-only web UI | in progress in [lajstrom-ui](https://github.com/lajstrom/lajstrom-ui) | [#70] |
| pypi and maven proxy and group repos | post-MVP, not scheduled | [#67], [#68] |
| Helm chart | in the repository ([deploy/helm.md](deploy/helm.md)), published with each release from v0.1.0 as `oci://ghcr.io/lajstrom/charts/lajstrom` | [#82] |
| Go modules, cargo, NuGet | on demand: no issue yet; open one with the use case | none |

## 7. Who it fits

**It fits** a platform team that already runs ECR, Harbor, Zot or GitLab and wants pypi, apt,
maven, npm and rpm repositories without a second storage system: no filestore to size, back up
or cluster, the registry's replication, encryption and IAM apply to packages, and the packages
stay readable with `oras` if Lajstrom goes away. Check your registry's column in the matrix
first: Zot runs in CI, Harbor 2.15.4, the GitLab registry and ECR were measured by hand (ECR on
2026-11-13, [#19]), and the GitLab registry with its metadata database needs
`REGISTRY_FF_DYNAMIC_MEDIA_TYPES`.

It also fits teams with OCI tooling everywhere (Flux, Helm charts as OCI, cosign, admission
policies) who want one signing and attestation model for packages and images.

**It does not fit yet** when you need to replace Artifactory or Nexus across many formats, with
LDAP or SAML, a UI, scanning and pull-through caches for all of them; when you want one small
cache for docker, npm and pip with curation on a single node (NORA), or for npm alone
(Verdaccio); or when the package registry built into GitLab, Gitea or Forgejo already covers
what you need.

## Sources

Every page below was read on 2026-11-10; release versions and creation dates come from the
GitHub API or the vendor's release notes on the same day. A cell or sentence above that none of
these pages confirms is marked **?**.

### Artifact Keeper

[README][ak-readme]; docs: [index](https://artifactkeeper.com/docs/),
[architecture](https://artifactkeeper.com/docs/getting-started/architecture/),
[storage](https://artifactkeeper.com/docs/advanced/storage/),
[auth and RBAC](https://artifactkeeper.com/docs/advanced/auth/),
[signing](https://artifactkeeper.com/docs/security/signing/),
[installation](https://artifactkeeper.com/docs/getting-started/installation/),
[support policy](https://artifactkeeper.com/docs/getting-started/support-policy/); in the
repository: [admin repair endpoints][ak-repair], [proxy cache HA][ak-ha],
[config as code](https://github.com/artifact-keeper/artifact-keeper/blob/main/docs/config-as-code.md);
the Helm values in [artifact-keeper-iac](https://github.com/artifact-keeper/artifact-keeper-iac).
Created 2026-01-15; v1.11.13 on 2026-11-10.

### NORA

[getnora.dev](https://getnora.dev),
[authentication](https://getnora.dev/configuration/authentication/),
[README](https://github.com/getnora-io/nora/blob/main/README.md), [ARCHITECTURE.md][nora-arch]
(its ADRs and "Known Trade-offs"), [COMPAT.md][nora-compat], [BENCHMARKS.md][nora-bench],
[GOVERNANCE.md](https://github.com/getnora-io/nora/blob/main/GOVERNANCE.md),
[ROADMAP.md](https://github.com/getnora-io/nora/blob/main/ROADMAP.md). Created 2026-01-25;
v1.4.4 on 2026-11-07.

### OpenArtifact

[README](https://github.com/openartifact/openartifact/blob/main/README.md),
[storage][oa-storage], [rebuild from storage][oa-rebuild], [scaling][oa-scaling],
[RBAC](https://github.com/openartifact/openartifact/blob/main/docs/security/rbac.md),
[declarative](https://github.com/openartifact/openartifact/blob/main/docs/integrations/declarative.md),
[`internal/storage/oci.go`][oa-oci]. Created 2026-04-21; v0.23.0 on 2026-11-09.

### Sonatype Nexus Repository

[feature matrix](https://help.sonatype.com/en/nexus-repository-feature-matrix.html),
[editions][nx-editions], [nexus-public][nx-public],
[blob stores](https://help.sonatype.com/en/configuring-blob-stores.html),
[system requirements][nx-sysreq],
[database options](https://help.sonatype.com/en/database-options.html),
[repair tasks][nx-repair], [3.87.0 release notes][nx-387], [usage center][nx-usage],
[pricing](https://www.sonatype.com/products/pricing), [realms][nx-realms],
[auditing](https://help.sonatype.com/en/auditing.html), [OCI repositories][nx-oci],
[2026 release notes](https://help.sonatype.com/en/nexus-repository-2026-release-notes.html)
(3.98.1 on 2026-11-05).

### JFrog Artifactory

[feature matrix][jf-matrix],
[JCR](https://docs.jfrog.com/artifactory/docs/container-registry-functionality),
[filestore](https://docs.jfrog.com/installation/docs/filestore-configuration-landing),
[backups][jf-backups], [OSS 7.161.26 sources][jf-oss],
[pricing](https://jfrog.com/pricing/), [start free](https://jfrog.com/start-free/),
[OCI repositories][jf-oci], [Evidence][jf-evidence],
[Terraform provider](https://github.com/jfrog/terraform-provider-artifactory),
[sizing][jf-sizing],
[releases](https://docs.jfrog.com/releases/docs/artifactory-self-managed-releases) (7.173.18 on
2026-11-03).

### Harbor

[proxy cache](https://goharbor.io/docs/main/administration/configure-proxy-cache/),
[architecture](https://github.com/goharbor/harbor/wiki/Architecture-Overview-of-Harbor),
[backup and restore][hb-backup], [HA][hb-ha],
[authentication](https://goharbor.io/docs/main/administration/configure-authentication/),
[audit log](https://goharbor.io/docs/main/administration/audit-log/),
[tag retention][hb-retention], [prerequisites][hb-prereq], [v2.11.0][hb-211],
[v2.16.0](https://github.com/goharbor/harbor/releases/tag/v2.16.0) (2026-11-10).

### Verdaccio

[configuration](https://verdaccio.org/docs/configuration),
[storage plugins](https://www.verdaccio.org/dev/plugin-storage/), [scaling][vd-scaling],
[v7.1.0](https://github.com/verdaccio/verdaccio/releases/tag/v7.1.0) (2026-11-08).

### Pulp

[architecture][pulp-arch],
[settings](https://github.com/pulp/pulpcore/blob/main/docs/admin/reference/settings.md),
[pulp_container#1169][pulp-referrers]; plugin releases from `gh release list` on each plugin's
repository (pulpcore 3.123.2 on 2026-11-05).

### Gitea and Forgejo

Gitea: [packages](https://docs.gitea.com/usage/packages/overview), [HA][gt-ha],
[go-gitea#21223][gitea-proxy], [go-gitea#38189](https://github.com/go-gitea/gitea/pull/38189)
(the audit log), v28.3.0 on 2026-11-01. Forgejo:
[packages](https://forgejo.org/docs/latest/user/packages/), [HA][fj-ha], v17.0.1 on 2026-10-26.

### GitLab Package Registry

[supported functionality](https://docs.gitlab.com/user/packages/package_registry/supported_functionality/),
[container registry metadata database][gl-metadata], [virtual registry][gl-virtual],
[container registry][gl-container], [pricing][gl-pricing],
[authentication](https://docs.gitlab.com/administration/auth/),
[reference architectures](https://docs.gitlab.com/administration/reference_architectures/).

### Cloudsmith

[formats](https://docs.cloudsmith.com/formats),
[upstreams](https://docs.cloudsmith.com/repositories/upstreams),
[signing keys](https://docs.cloudsmith.com/supply-chain-security/signing-keys),
[OCI artifacts and referrers][cs-referrers],
[vulnerability scanning](https://docs.cloudsmith.com/supply-chain-security/vulnerability-scanning),
[authentication](https://docs.cloudsmith.com/authentication). The prices come from a search
engine's copy of [cloudsmith.com/pricing](https://cloudsmith.com/pricing) dated 2026-06-11,
because the live page answered `429`: unverified.

### Inedo ProGet

[feeds](https://docs.inedo.com/docs/proget/feeds/feed-overview),
[cloud storage](https://docs.inedo.com/docs/proget/cloud-storage), [HA][pg-ha],
[license][pg-license], [pricing](https://inedo.com/proget/pricing).

### Registries and tools named on this page

[distribution's garbage collection][dist-gc],
[distribution#3716][dist-referrers] (referrers), [ECR service quotas][ecr-quotas],
[ECR lifecycle policies][ecr-lifecycle], [Trivy SBOM scanning][trivy-sbom], [Grype][grype].

<!-- Lajstrom issues and tests -->
[#19]: https://github.com/lajstrom/lajstrom/issues/19
[#64]: https://github.com/lajstrom/lajstrom/issues/64
[#67]: https://github.com/lajstrom/lajstrom/issues/67
[#68]: https://github.com/lajstrom/lajstrom/issues/68
[#69]: https://github.com/lajstrom/lajstrom/issues/69
[#70]: https://github.com/lajstrom/lajstrom/issues/70
[#77]: https://github.com/lajstrom/lajstrom/issues/77
[#82]: https://github.com/lajstrom/lajstrom/issues/82
[#114]: https://github.com/lajstrom/lajstrom/issues/114
[#442]: https://github.com/lajstrom/lajstrom/issues/442
[e2e-cosign]: ../test/e2e/cosign.sh
[e2e-oras]: ../test/e2e/oras.sh
[e2e-ha]: ../test/e2e/ha.sh

<!-- Sources, all read on 2026-11-10 -->
[ak-readme]: https://github.com/artifact-keeper/artifact-keeper/blob/main/README.md
[ak-repo]: https://github.com/artifact-keeper/artifact-keeper
[ak-repair]: https://github.com/artifact-keeper/artifact-keeper/blob/main/docs/operations/admin-repair-endpoints.md
[ak-ha]: https://github.com/artifact-keeper/artifact-keeper/blob/main/docs/operations/proxy-cache-ha.md
[nora-arch]: https://github.com/getnora-io/nora/blob/main/ARCHITECTURE.md
[nora-compat]: https://github.com/getnora-io/nora/blob/main/COMPAT.md
[nora-bench]: https://github.com/getnora-io/nora/blob/main/BENCHMARKS.md
[oa-storage]: https://github.com/openartifact/openartifact/blob/main/docs/operations/storage.md
[oa-rebuild]: https://github.com/openartifact/openartifact/blob/main/docs/operations/rebuild-from-storage.md
[oa-scaling]: https://github.com/openartifact/openartifact/blob/main/docs/operations/scaling.md
[oa-oci]: https://github.com/openartifact/openartifact/blob/main/internal/storage/oci.go
[nx-editions]: https://help.sonatype.com/en/nexus-repository-tiers-and-feature-availability.html
[nx-public]: https://github.com/sonatype/nexus-public
[nx-sysreq]: https://help.sonatype.com/en/sonatype-nexus-repository-system-requirements.html
[nx-repair]: https://help.sonatype.com/en/verify-and-repair-data-consistency-tasks.html
[nx-387]: https://help.sonatype.com/en/sonatype-nexus-repository-3-87-0-release-notes.html
[nx-usage]: https://help.sonatype.com/en/usage-center.html
[nx-realms]: https://help.sonatype.com/en/realms.html
[nx-oci]: https://help.sonatype.com/en/oci-repositories.html
[jf-matrix]: https://docs.jfrog.com/installation/docs/feature-comparison-matrix-for-self-mangaged-jpds
[jf-backups]: https://docs.jfrog.com/installation/docs/backups
[jf-oss]: https://releases.jfrog.io/artifactory/bintray-artifactory/org/artifactory/oss/jfrog-artifactory-oss/7.161.26/
[jf-oci]: https://docs.jfrog.com/artifactory/docs/oci-repositories
[jf-evidence]: https://docs.jfrog.com/governance/docs/evidence-management
[jf-sizing]: https://docs.jfrog.com/installation/docs/hardware-sizing-matrix
[hb-backup]: https://goharbor.io/docs/main/administration/backup-restore/
[hb-ha]: https://goharbor.io/docs/main/install-config/harbor-ha-helm/
[hb-retention]: https://goharbor.io/docs/main/working-with-projects/working-with-images/create-tag-retention-rules/
[hb-prereq]: https://goharbor.io/docs/main/install-config/installation-prereqs/
[hb-211]: https://github.com/goharbor/harbor/releases/tag/v2.11.0
[vd-scaling]: https://github.com/verdaccio/verdaccio-aws-s3-storage/blob/master/docs/scaling.md
[pulp-arch]: https://pulpproject.org/pulpcore/docs/admin/learn/architecture/
[pulp-referrers]: https://github.com/pulp/pulp_container/issues/1169
[gitea-proxy]: https://github.com/go-gitea/gitea/issues/21223
[gt-ha]: https://gitea.com/gitea/helm-gitea/raw/branch/main/docs/ha-setup.md
[fj-ha]: https://code.forgejo.org/forgejo-helm/forgejo-helm/raw/branch/main/docs/ha-setup.md
[gl-metadata]: https://docs.gitlab.com/administration/packages/container_registry_metadata_database/
[gl-virtual]: https://docs.gitlab.com/user/packages/virtual_registry/
[gl-container]: https://docs.gitlab.com/user/packages/container_registry/
[gl-pricing]: https://about.gitlab.com/pricing/
[cs-referrers]: https://cloudsmith.com/changelog/store-secure-and-distribute-oci-artifacts-with-cloudsmith-42Z9oBLK
[pg-ha]: https://docs.inedo.com/docs/installation-high-availability-load-balancing
[pg-license]: https://docs.inedo.com/docs/proget/administration/license
[dist-gc]: https://distribution.github.io/distribution/about/garbage-collection/
[dist-referrers]: https://github.com/distribution/distribution/issues/3716
[ecr-quotas]: https://docs.aws.amazon.com/AmazonECR/latest/userguide/service-quotas.html
[ecr-lifecycle]: https://docs.aws.amazon.com/AmazonECR/latest/userguide/LifecyclePolicies.html
[trivy-sbom]: https://trivy.dev/latest/docs/target/sbom/
[grype]: https://github.com/anchore/grype
