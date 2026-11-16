# Compliance: PCI DSS and SOC 2

What Lajstrom provides to an organisation that runs it under PCI DSS v4.0.1 or a SOC 2
examination: for each requirement that concerns an artifact registry, the control in Lajstrom
(the feature, its configuration key, the page that describes it) and the evidence it leaves, or
what the operator does outside Lajstrom, and the gaps, each with its issue. The umbrella issue is
[#1275](https://github.com/lajstrom/lajstrom/issues/1275).

An assessment or an examination is of the organisation, not of a product: no software is "PCI
compliant" or "SOC 2 compliant" by itself. Lajstrom's part is to provide controls and the evidence
that they work. Whether a control meets a requirement in a given environment is the assessor's
or the auditor's judgement, not this page's.

The page describes `main` on 2026-11-16. Features of stacks still open are marked with their
issue: CI token exchange (#937), service accounts (#1020), the web UI's sign-in (#1170), policy
(#101) and encrypted repos (#114).

## Sources

- **PCI DSS:** *Payment Card Industry Data Security Standard: Requirements and Testing
  Procedures*, v4.0.1, June 2024, PCI Security Standards Council. The council's document library
  asks for a license agreement before it serves the file; the text was read from the council's
  PDF (397 pages, sha256 `5e6b9093b84007b973097d20126a3768ea2f0a1d4200255c849b0fb3bf04ebc7`) as
  [Middlebury College publishes it](https://www.middlebury.edu/sites/default/files/2025-01/PCI-DSS-v4_0_1.pdf).
  Requirement numbers and the words in quotation marks are from it; the other summaries of
  requirements are this page's.
- **SOC 2:** AICPA, TSP section 100, *2017 Trust Services Criteria for Security, Availability,
  Processing Integrity, Confidentiality, and Privacy (With Revised Points of Focus — 2022)*. The
  AICPA serves that edition after a sign-in, and it was not read. The 2022 revision changed the
  points of focus and not the criteria ([EY Technical Line, 2 November
  2022](https://ey.com/content/dam/ey-unified-site/ey-com/en-us/technical/accountinglink/documents/ey-ttp17549-221us-11-02-2022.pdf)),
  so the criteria quoted here are from the AICPA's March 2020 edition of TSP section 100, as
  [published by Arpio](https://arpio.io/wp-content/uploads/2020/08/trust-services-criteria.pdf).
  This page cites no point of focus but one, named with its edition.

## Scope

PCI DSS applies to the cardholder data environment (CDE) and to "System components, people, and
processes that could impact the security of cardholder data and/or sensitive authentication
data", where system components include software (PCI DSS, *Scope of PCI DSS Requirements*). Its
Requirement 6 overview says that "Code repositories that store application code, system
configurations, or other configuration data that can impact the security of cardholder data
and/or sensitive authentication data are in scope". A registry that holds what runs in a CDE, or
that CDE systems pull from, is in the same position; the organisation's scoping decides.

Lajstrom is one of several components of a registry. The OCI registry behind it holds every byte
([ADR 0001](adr/0001-oci-registry-as-sole-storage.md)), the index database holds the control
plane and the audit log, the edge terminates TLS, and the identity provider (IdP) logs people in.
Each is a system component of its own, and this page covers them only for what Lajstrom expects
of them ([threat model](security/threat-model.md) §1 and §4).

This page assumes that no artifact contains account data (card numbers, sensitive
authentication data). An artifact that does is stored account data under Requirement 3, in the
registry and in its backups.

## What the operator brings in every case

- **TLS at the edge**, with `Strict-Transport-Security`, and `server.externalURL` set to the
  public URL ([reverse-proxy.md](reverse-proxy.md)). The hop from the edge to Lajstrom is plain
  HTTP unless the edge re-encrypts to `server.tls` or a service mesh encrypts it; passwords and
  tokens cross it.
- **The registry out of clients' reach**: a client that reaches the registry directly bypasses
  RBAC, the audit log and every other check ([threat model](security/threat-model.md) §1, P-8).
- **NTP** on every host of `serve`, the CLI and the database ([Clocks](operations.md#clocks)).
- **Log collection and retention**: Lajstrom writes its access log and its application log to
  standard error (`log.format: json` for a collector, [configuration.md](configuration.md#log)).
- **Backups** of the index database, and the custody of the master key, kept apart from them
  ([Backup and restore](operations.md#backup-and-restore), [The master
  key](operations.md#the-master-key)).
- **An IdP** with the organisation's MFA, lockout and offboarding, for OIDC login
  ([auth.md](auth.md#oidc-login-and-groups)).
- **Change management of the configuration file**: it is read at start and validated
  ([configuration.md](configuration.md)); keep it in git.
- **Updates of Lajstrom itself**: releases are signed and come with SBOMs
  ([release.md](release.md#verifying-a-release)); security fixes land in patch releases of the
  latest minor version ([SECURITY.md](../SECURITY.md)).

## PCI DSS v4.0.1

Each table has the requirement, what Lajstrom does with the evidence it leaves, and what is left
to the operator or missing. A gap names its issue.

### Requirement 2: secure configuration

| Requirement | Lajstrom: the control and its evidence | Outside Lajstrom, or the gap |
| --- | --- | --- |
| **2.2.1** Configuration standards cover all system components and follow hardening standards. | Every key with its default ([configuration.md](configuration.md)); the Helm chart and the Kubernetes manifests run under the `restricted` Pod Security Standard: a non-root user, no capabilities, a read-only root filesystem, no service-account token ([deploy/kubernetes.md](deploy/kubernetes.md#security-context)); what the edge must do ([threat model](security/threat-model.md) §4). An unknown key or an invalid value stops every command that reads the file. *Evidence:* the configuration file; the startup log. | The standard itself: the keys and values the organisation requires, the ones on this page among them. |
| **2.2.2** Vendor default accounts are removed, disabled or get a new password. | There is no default account and no admin password in the configuration or the environment. The first admin is made with a one-time token that the operator makes, valid for `auth.bootstrap.ttl` (24 hours) and once ([auth.md](auth.md#bootstrap-the-first-admin)); `serve` warns while the token's file still exists. *Evidence:* the audit row `user.create` with `source: bootstrap`. | Remove the bootstrap secret after the exchange. The registry and the database may have default accounts of their own. |
| **2.2.4** Only necessary services, protocols and functions are enabled. | `/metrics` is on a listener of its own, on loopback by default (`server.metricsListen`), and `/debug/pprof/` is off (`server.pprof: false`). Anonymous reads are off (`auth.anonymousRead: false`). OIDC, client certificates, Kubernetes tokens and webhooks do nothing until configured. | Never route the metrics listener; keep `/-/version` inside if the build should not be public (threat model B-4). |
| **2.2.5** Insecure services are justified and have additional security features. | An `http://` `server.externalURL` stops the start unless `server.insecureHTTP`, which allows a loopback host only, for the dev stack, and warns when the listener is not on loopback. `server.tls` speaks TLS 1.2 and later. | The plain HTTP hop from the edge, above. |
| **2.2.6** Security parameters are configured to prevent misuse. | Secrets never go into the file: a `password` key is refused, and the keys that end in `Env` or `File` name where the secret is ([configuration.md](configuration.md#secrets)). A master-key file that others may read is used with a warning. Browsers get `Content-Security-Policy: default-src 'none'`, `nosniff` and `no-referrer`, and a cross-site change is refused before its credentials are checked ([operations.md](operations.md#what-lajstrom-answers-browsers)). | |
| **2.2.7** Non-console administrative access is encrypted, browser interfaces and APIs included. | The management API, the web UI's sign-in (#1170) and `lajstrom admin bootstrap` are HTTP routes, behind the edge's TLS or `server.tls`. The other `lajstrom admin` commands talk to the database. | TLS at the edge, TLS to the database (`sslmode` of the DSN) and to the registries, the hop from the edge. |

### Requirements 3 and 4: keys and artifacts

Requirements 3.6 and 3.7 are about "cryptographic keys used to protect stored account data", and
4.2.1 about account data "during transmission over open, public networks". Lajstrom's keys protect
signatures, bearer tokens, webhook secrets and, with encrypted repos (#114), artifacts; they are
under these requirements only where the organisation puts them under its key-management
procedures. The table says what Lajstrom does for each point all the same.

| Requirement | Lajstrom: the control and its evidence | Outside Lajstrom, or the gap |
| --- | --- | --- |
| **3.6.1** Keys: the fewest custodians, key-encrypting keys at least as strong as the keys they protect and stored apart from them, the fewest locations. | One master key, 32 random bytes in `keys.masterKeyEnv` or `keys.masterKeyFile`, never in the configuration file or the database, seals with AES-256-GCM every private key of the index (apt and rpm signing keys, the bearer-token key, the policy key of #101, the encryption keys of #114), the webhooks' secrets and sealed snapshots ([The master key](operations.md#the-master-key)). A repo's signing key can be a key of AWS KMS or GCP Cloud KMS that never leaves it (`keys.kms`, [Keys in a KMS](operations.md#keys-in-a-kms), [ADR 0020](adr/0020-repo-signing-keys-in-a-cloud-kms.md)); so can an encrypted repo's key (#114). | Custody of the master key: who reads its Secret or file, its copy apart from the backups. |
| **3.7.4**, **3.7.5** Key changes at the end of a cryptoperiod; retirement or replacement when a key is weakened or compromised. | `lajstrom admin key rotate --repo` (a retired key signs beside the new one for `keys.retireGrace`; `0s` for a leaked key, [Rotation](operations.md#rotation)); `--token` (the old key verifies for 21 minutes; `--drop` ends every token of it within a second, [The bearer-token key](operations.md#the-bearer-token-key)). *Evidence:* audit rows `key.rotate` and `key.drop` with the fingerprints. | The cryptoperiods. **Gap:** the master key cannot be replaced without making every key it seals again: [#1288](https://github.com/lajstrom/lajstrom/issues/1288). |
| **3.7.6** Manual cleartext key operations under split knowledge and dual control. | The master key is one value; nothing in Lajstrom splits it. A KMS key is never in clear outside the KMS. | Where these keys are in scope: load the master key through the secret store under dual control, or keep the signing keys in a KMS. |
| **4.2.1** Strong cryptography for account data over open, public networks. | No account data is assumed. All of Lajstrom's connections can be TLS: clients (the edge, or `server.tls`), the registries (`registries[].url`), OIDC and Kubernetes issuers (`https` only, but with `server.insecureHTTP`), webhooks (to public addresses over `https` only; `webhooks.allowedHosts` for the others). | Certificates and protocol versions at the edge. |

### Requirement 6: software, SBOMs, signatures and changes

| Requirement | Lajstrom: the control and its evidence | Outside Lajstrom, or the gap |
| --- | --- | --- |
| **6.3.1** Security vulnerabilities are identified and ranked, for bespoke and third-party software. | Scan reports are signed attachments of a version, with the scanner and its database read from the report, and `GET …/versions?without=scan` lists the versions without one (#89). `deploy/jobs/scan/` is a reference job with syft, trivy and cosign ([guides/sbom-and-scans.md](guides/sbom-and-scans.md)). Lajstrom does not scan. | The scanning, the ranking and the sources. A version scanned with an older vulnerability database is not listed again (the guide's "Not there yet"). |
| **6.3.2** An inventory of bespoke and custom software and its third-party components. | An SBOM per version, attached with `cosign attest`, listed with its label: `verified` when a key of `attachments.trust` or an identity of `attachments.keyless` signed it ([configuration.md](configuration.md#attachments)); `?without=sbom` lists the versions without one. *Evidence:* `GET …/versions/<version>/attachments`. | Making the SBOMs in CI. **Gap:** nothing keeps a version without one from being served: [#1287](https://github.com/lajstrom/lajstrom/issues/1287). |
| **6.3.3** Patches are installed in time. | For third-party packages through proxy repos: deny and allow rules, a quarantine of new upstream versions, and signed verdicts keep a known-bad version from being served (#101, in progress). For Lajstrom itself: signed releases with SBOMs. | Patching what runs, Lajstrom included. |
| **6.4.1**, **6.4.2** Public-facing web applications are protected against attacks. | The headers and the cross-site refusal of 2.2.6; the threats and their mitigations in the [threat model](security/threat-model.md). | If Lajstrom is reachable from the internet, the automated technical solution of 6.4.2 sits at the edge. |
| **6.5.1** Changes in production follow procedures: reason, security impact, "Documented change approval by authorized parties", testing, a way back. | A published version never changes: a tag is never moved, and other bytes under it are `409` ([ADR 0005](adr/0005-manifest-profile-and-immutable-tags.md)), so a deployment names exactly what was approved and the way back is the version before. Every publish and delete is an audit row with the principal, the client address and the digest; the manifest of a publish through a frontend carries the principal in `lajstrom.uploaded-by`. Provenance attestations and signatures attach to the version (#89). A CI job publishes with a token whose trust policy names the repository and a branch or an environment; pull-request tokens never write (#937). | The procedure, the approval and the tests are the CI/CD system's. A GitHub environment with required reviewers, named in a write policy of #937, makes the approval a condition of the publish. |
| **6.5.3** Pre-production and production are separated, enforced by access control. | Separate repos, or separate servers, with separate bindings: a `writer` binding on `*/staging` gives nothing on `*/prod`, and a deploy token reaches its own repo only, `404` elsewhere ([auth.md](auth.md#what-a-token-may-do)). | Name the repos so that masks separate the environments ([Bindings](auth.md#bindings)). |
| **6.5.4** Roles are separated between production and pre-production, "such that only reviewed and approved changes are deployed". | Production repos written only by the release pipeline: a deploy token of the repo with `write`, or a CI token whose policy needs the protected branch or environment (#937); people get `reader` there. A write policy of #937 created through the API needs a fresh MFA sign-in and a second admin's approval (`auth.ci.apiWriteApproval: second-admin`, decided on #937). | Who approves the pipeline's environment. **Gap:** other privileged changes need no second person: [#1285](https://github.com/lajstrom/lajstrom/issues/1285). |

### Requirement 7: access

| Requirement | Lajstrom: the control and its evidence | Outside Lajstrom, or the gap |
| --- | --- | --- |
| **7.2.1**, **7.2.2** An access model by job function and least privilege. | Five roles, `reader`, `attacher`, `writer`, `maintainer` and `admin`, bound to principals on repo masks; a principal's rights are the union of its bindings, and a token's scopes narrow them ([Roles](auth.md#roles), [rbac.md](spec/rbac.md) §3). `attacher` signs and attaches without publishing. A `maintainer` on `*/*` still does not reach users or bindings: that is `admin`. The IdP's groups are principals, `group:<name>`. *Evidence:* `GET /api/v1/bindings`, `lajstrom admin binding list`. | Which role and mask each job function gets. A mask matches the repos created later too: a binding on `raw/*` reaches the next raw repo. |
| **7.2.3** "Required privileges are approved by authorized personnel." | Only `admin` creates bindings, users, service accounts (#1020) and deploy tokens over a mask, the last with its password only (#911); a repo's `maintainer` creates its deploy tokens. *Evidence:* `binding.create`, `user.create`, `token.create` with `createdBy`, each with who made it. | The approval. **Gap:** no approval step and no place for its reference: [#1285](https://github.com/lajstrom/lajstrom/issues/1285). |
| **7.2.4** User accounts and their privileges are reviewed at least once every six months, and "Management acknowledges that access remains appropriate". | What a reviewer reads, as admin: `GET /api/v1/users`, `GET /api/v1/bindings`, each user's tokens, `GET /api/v1/deploy-tokens`, every token at once (`GET /api/v1/tokens`, #959, open), with each token's `lastUsedAt`; the audit log of the changes since the last review (`GET /api/v1/audit?since=`). | Running the review and recording the acknowledgement. **Gap:** no report joins these per principal, and nothing records the review: [#1279](https://github.com/lajstrom/lajstrom/issues/1279). |
| **7.2.5** Application and system accounts get the least privileges they need. | Credentials of their own for systems: deploy tokens (one repo or a mask; `read`, `write`, `attach`; never `delete` or `manage`), service accounts with an owner (#1020), client certificates and Kubernetes service accounts bound by name or by a pattern, never `admin` ([Node and workload identity](auth.md#node-and-workload-identity)), CI tokens whose rights are their policy's `grants` (#937). | Bind the narrowest: `cert:spiffe://cluster-a/*` reaches the trust domain's workloads as well as its nodes. |
| **7.2.5.1** Access by application and system accounts is reviewed periodically. | As 7.2.4, and a service account's owner and whether the owner is disabled (#1020). | **Gap:** [#1279](https://github.com/lajstrom/lajstrom/issues/1279). |
| **7.2.6** Access to query repositories of stored cardholder data. | Not applicable while no artifact holds cardholder data. | |
| **7.3.1**, **7.3.2** An access control system covers all system components and enforces the permissions of individuals, applications and systems. | Every route is decided by RBAC: the frontends, `/v2`, `/api/v1`; no configuration turns it off ([Authorization](auth.md#authorization)). | Keep clients away from the registry (above): its own credentials are Lajstrom's alone. |
| **7.3.3** The access control system is set to "deny all" by default. | What no binding allows is denied ([rbac.md](spec/rbac.md) §3, [How a request is decided](auth.md#how-a-request-is-decided), step 6). `auth.anonymousRead` is off by default, and a repo's `anonymousRead` counts only with it. A push never creates a repo. A denial does not tell whether a repo exists (the 401/404/403 rule). | |

### Requirement 8: identification and authentication

| Requirement | Lajstrom: the control and its evidence | Outside Lajstrom, or the gap |
| --- | --- | --- |
| **8.2.1** A unique ID for every user. | A user is its uid, a UUID v7 that never changes and is never reused; the name is an attribute ([auth.md](auth.md#the-uid-and-the-name)). Every audit row and access log line names the principal: `user:<uid>`, `token:<id>`, `cert:…`, `k8s:…`. A deleted user keeps its row as `~<uid>`. | |
| **8.2.2** Shared or generic IDs only by exception. | People authenticate as themselves: a password, a personal access token, an OIDC login. A deploy token is a principal of its own, `token:<id>`, for a pipeline or a fleet of nodes. | Never give a person a deploy token: a person uses a personal access token. |
| **8.2.4** Additions, deletions and changes of user IDs and authentication factors are approved. | *Evidence:* `user.create`, `user.rename`, `user.disable`, `user.enable`, `user.password`, `user.delete`, `token.create`, `token.revoke`, `binding.create`, `binding.delete` ([Audit](operations.md#audit)). | The approval: [#1285](https://github.com/lajstrom/lajstrom/issues/1285). |
| **8.2.5** "Access for terminated users is immediately revoked." | `lajstrom admin user disable` refuses the user's passwords and tokens from the next request on, on every replica; a `/v2` bearer issued before lasts 20 minutes at most, and `lajstrom admin key rotate --token --drop` ends every bearer within a second ([What a token does not say](auth.md#what-a-token-does-not-say), [Disabling a user](operations.md#disabling-a-user)). *Evidence:* `user.disable`, `token.revoke`. | Offboarding disables the user in Lajstrom too, and ends by hand what disabling does not: [Offboarding a person](#offboarding-a-person), with a short `auth.oidc.tokenLifetime`. **Gap:** nothing tells Lajstrom of a removal at the IdP; asking the IdP again is planned: [#1282](https://github.com/lajstrom/lajstrom/issues/1282). |
| **8.2.6** Inactive user accounts are removed or disabled within 90 days. | Tokens have `lastUsedAt`; OIDC logins are audited (`user.login`). Decided on #1283, being built: each user's last authentication, `lastSeenAt` in the users API, and `auth.disableInactiveUsersAfter`, off by default. With it set, no credential of a user inactive that long is taken from that moment on, on any replica, and the first one tried disables the user (a `user.disable` row with `reason` `inactive`); until then the users API marks the user `inactive`. A service account's activity is the use of its tokens; the last enabled admin is never disabled ([rbac.md](spec/rbac.md) §1). | Set `auth.disableInactiveUsersAfter: 2160h`. A user who never tries again stays enabled, `inactive` and unable to authenticate: the review (7.2.4) disables or deletes it. **Gap** until it is built: [#1283](https://github.com/lajstrom/lajstrom/issues/1283). |
| **8.2.7** Third-party remote access is enabled only when needed and monitored. | A vendor is a user like any other. | Enable its user for the time needed (`lajstrom admin user enable`, then `disable`, both audited). |
| **8.2.8** A session idle for more than 15 minutes needs re-authentication. | The web UI's sign-in token (#1170) lives `auth.login.tokenLifetime`, 12 hours by default and at most 24, is bound to the browser's DPoP key and is revoked at sign-out. Package clients authenticate every request and hold no session. Decided on #1277, being built: `auth.login.idleTimeout`, off by default, refuses and revokes a sign-in token unused for that long (`401`, `error_description="idle"`, a `token.revoke` row with `source` `idle`), the last use recorded to the minute ([rbac.md](spec/rbac.md) §10); the UI warns before. | Set `auth.login.idleTimeout: 15m`; the setting is the evidence the testing procedure examines. **Gap** until it is built: [#1277](https://github.com/lajstrom/lajstrom/issues/1277). |
| **8.3.1** Every access is authenticated by at least one factor. | A password, a personal access token or a deploy token (256 random bits), a client certificate, a Kubernetes service-account token, an OIDC login. Without credentials a request gets the system endpoints (`/-/healthz`, `/-/readyz`, `/-/version`) and nothing else, unless both `anonymousRead` switches are on for a repo ([HTTP Basic on every route](auth.md#http-basic-on-every-route)). | |
| **8.3.2** Authentication factors are unreadable in transmission and storage. | Passwords are stored as argon2id with OWASP's parameters, token secrets and the bootstrap token as sha256, private keys sealed with the master key; the OIDC client secret and the registries' passwords come from files or variables ([auth.md](auth.md#local-users-and-passwords), [The secret](auth.md#the-secret)). No password, token, `Authorization` header or OIDC code is logged (threat model L-1 to L-4). | In transmission: TLS at the edge, the hop from it, TLS to the database and the registries. |
| **8.3.3** User identity is verified before an authentication factor is modified. | Only the admin role sets a password (`POST /api/v1/users/<user>/password`, `lajstrom admin user set-password`); a personal access token is created with a password and never with a token (#249); a deploy token over a mask with an admin's password only (#911). | Checking who asks before an admin resets a password. |
| **8.3.4** Lockout after at most 10 attempts, for at least 30 minutes or until the user's identity is confirmed. | A throttle: 10 failed passwords in a minute per account and replica, then `429` for the rest of that minute, and 30 a minute per client address ([Failed logins](auth.md#failed-logins)). Tokens are not throttled. | **Gap:** a throttle, not a lockout (decided on #282): [#1280](https://github.com/lajstrom/lajstrom/issues/1280). Through OIDC the IdP's lockout applies. |
| **8.3.5** A unique first password, changed after first use. | An admin sets a user's password; users cannot change their own. | **Gap:** [#1281](https://github.com/lajstrom/lajstrom/issues/1281). |
| **8.3.6** At least 12 characters, letters and digits. | 8 to 1024 bytes of UTF-8, any characters. | **Gap:** [#1281](https://github.com/lajstrom/lajstrom/issues/1281). |
| **8.3.7** Not one of the last four passwords. | No history is kept. | **Gap:** [#1281](https://github.com/lajstrom/lajstrom/issues/1281). |
| **8.3.9** Single-factor passwords changed every 90 days, or the accounts' posture analysed. | No password age. | **Gap:** [#1281](https://github.com/lajstrom/lajstrom/issues/1281); it "does not apply to in-scope system components where MFA is used". |
| **8.3.11** Tokens, smart cards and certificates belong to one user. | A personal access token belongs to one user; a client certificate names one node or workload (its SPIFFE ID or CN). | One certificate per node; no shared personal access tokens. |
| **8.4.1**, **8.4.2** MFA for all non-console administrative access, and for all non-console access into the CDE. | Through OIDC the IdP's MFA applies to the login, but Lajstrom does not check that it was used (`amr`, `acr`), and admins also authenticate with passwords and personal access tokens. #937 decided an MFA check (`amr`, `auth_time`) for write policies created through the API. | **Gap:** [#1276](https://github.com/lajstrom/lajstrom/issues/1276). 8.4.2's notes exempt "Application or system accounts performing automated functions": deploy tokens, service accounts, CI tokens, nodes. |
| **8.5.1** MFA is not replayable and cannot be bypassed. | The UI's token works only with a proof of the browser's DPoP key (#1170). | **Gap:** with [#1276](https://github.com/lajstrom/lajstrom/issues/1276). |
| **8.6.1** System accounts used for interactive login. | A service account has no password and cannot log in (#1020); deploy tokens, CI tokens and certificates are not interactive logins. | A deploy token in a person's hands is interactive use (8.2.2). |
| **8.6.2** No system account passwords in scripts, configuration files or source code. | Lajstrom's own secrets (the registries' passwords, the master key, the OIDC client secret, the bootstrap token) come from files or variables that the configuration names ([configuration.md](configuration.md#secrets)). CI reads a deploy token from its secret store ([In CI](auth.md#in-ci)), or needs none with token exchange (#937). | The secret store. |
| **8.6.3** System account passwords are changed periodically and when compromised. | A deploy token rotates by create, switch, revoke ([A credential for CI](operations.md#a-credential-for-ci)), and is revoked at once when compromised. `auth.deployTokens.maxTTL` bounds its lifetime, a repo's token and one over a mask alike, none by default (#1286): a token created without an expiry expires at the bound, one asked to expire later is refused, and a live token from before the bound and beyond it is marked `beyondMaxTTL` ([rbac.md](spec/rbac.md) §1). Personal access tokens expire within 365 days; a service account's tokens within `auth.serviceAccounts.maxTokenTTL`, 365 days by default (#1020); CI tokens live 15 minutes (#937); a client certificate as long as its CA makes it. *Evidence:* `token.create` with `expiresAt` and `maxTTL`; `GET /api/v1/tokens?live=true` without `beyondMaxTTL`, and no warning of `serve` at start about tokens beyond the bound. | Set `auth.deployTokens.maxTTL` to the period of the targeted risk analysis, and replace the tokens marked `beyondMaxTTL`. Better, credentials that need no rotation: client certificates for nodes and Kubernetes service-account tokens for the images of pods ([rbac.md](spec/rbac.md) §9), token exchange for CI ([rbac.md](spec/rbac.md) §11); a deploy token only where neither works. A node fleet on a deploy token over a mask replaces its secret on every node within the bound. |

#### Offboarding a person

Nothing tells Lajstrom that a person left: a login asks the IdP once, and nothing asks it again
([rbac.md](spec/rbac.md) §8). Until Lajstrom asks the IdP again with each login's sealed refresh
token ([#1282](https://github.com/lajstrom/lajstrom/issues/1282)), what it offers for 8.2.5 and
CC6.2 is this procedure, which an admin runs when a person leaves, and a short lifetime of the
logins' tokens, which bounds what a late step leaves open. It is a procedure, not a control:
"immediately" depends on people.

1. **At the IdP**, disable or remove the person. No login works from then on; the tokens of their
   earlier logins still do, and so do those logins' groups (the window, below).
2. **In Lajstrom, disable the user**: `lajstrom admin user disable alice`, or
   `PATCH /api/v1/users/alice` with `{"disabled": true}`. From the next request on, on every
   replica, the user's password, its logins and every personal access token of the user are
   refused: those it made, those of its OIDC logins and of `lajstrom login`, a web UI's, and those
   an admin made for it ([Disabling a user](operations.md#disabling-a-user)). Disable, do not
   delete: deleting an OIDC user lets its identity go, and a login the IdP still lets through
   makes a new user, with its groups' bindings. The last enabled user with an admin binding is
   not disabled (`409`): make another admin first.
3. **A bearer of `/v2`** issued before step 2 lasts 20 minutes at most, and so does the archive
   URL of a terraform module. `lajstrom admin key rotate --token --drop` ends them within a
   second, with the bearers and CI tokens of every other principal
   ([What a token does not say](auth.md#what-a-token-does-not-say)).
4. **What disabling does not end**: what the person made that is not the user's.
   - *Tokens made for others.* A deploy token, of a repo or over a mask, belongs to no user, and a
     token the person made for a service account or another user belongs to that one: each keeps
     working. No token records who made it, but its `token.create` row does:
     `GET /api/v1/audit?principal=user:alice&action=token.create` lists them, and those whose
     `subject` is not alice's `user:<uid>` are others', with the token's id in `details.id`. Give
     each system a new token, then `lajstrom admin token revoke <id>`
     ([A credential for CI](operations.md#a-credential-for-ci)); the same for the deploy tokens of
     a secret store the person could read. The rows of `lajstrom admin` name no person
     ([#1284](https://github.com/lajstrom/lajstrom/issues/1284)).
   - *Service accounts the person owns* (#1020). With
     `auth.serviceAccounts.onOwnerDisabled: flag`, the default, they keep working, and
     `lajstrom admin user list --kind service` shows their owner as `user:alice (disabled)`: give
     each another owner (`lajstrom admin user set-owner ci-release user:bob`) and new tokens, and
     revoke those the person held. With `disable`, step 2 disabled them; enable each again by
     itself ([Service accounts](auth.md#service-accounts)).
   - *CI trust policies* the person made or changed through the API (#937), `createdBy` or
     `updatedBy` in `GET /api/v1/ci/policies`, keep granting, and a pending one can still be
     approved. Review them, and delete those nobody else answers for
     (`lajstrom admin ci-policy delete <name>`). A policy whose claims name the person at the CI
     platform (`actor`, `actor_id`, `user_login`, `user_id`) matches their jobs as long as the
     platform runs them.
   - *Webhooks* the person made, `createdBy` in `GET /api/v1/webhooks`, keep sending events to
     their URLs.
   - *The operator's secrets.* Whoever holds the database's credentials runs every
     `lajstrom admin` command without a password: change them, and the other secrets the person
     read (the OIDC client secret, the registries' credentials). The master key cannot be
     replaced: [#1288](https://github.com/lajstrom/lajstrom/issues/1288).
5. **Bindings** need nothing: the user's own grant nothing while it is disabled, and its groups'
   reach it only through its logins. Later, once the IdP no longer logs the person in,
   `lajstrom admin user delete alice` deletes them and revokes its tokens for good; it is `409`
   while the user owns a live service account.
6. **Check**: `GET /api/v1/users/alice` answers `"disabled": true`;
   `lajstrom admin token list --user alice` shows no `LAST USED` after the disabling;
   `GET /api/v1/audit?principal=user:alice&since=<t>`, `t` the `at` of the `user.disable` row, is
   empty; after the 20 minutes of step 3, or the `--drop`, no access log line names alice's
   `user:<uid>`. *Evidence:* the IdP's record of the removal, the `user.disable` row, the
   `token.revoke` rows of step 4.

**The window.** Between steps 1 and 2, a person removed at the IdP keeps the tokens of their
logins, the browser's and `lajstrom login`'s, for `auth.oidc.tokenLifetime` (24 hours by
default), and their groups as long; a web UI's token for the shorter of
`auth.login.tokenLifetime` (12 hours) and `auth.oidc.tokenLifetime`; and a bearer of `/v2`
20 minutes beyond either. A token an admin made for the user, and a password an admin set for
it, do not depend on the IdP: only step 2 ends them.

Set `auth.oidc.tokenLifetime` to the longest window the organisation accepts for a late step 2:
`8h`, one working day, or less ([configuration.md](configuration.md#auth)).
`auth.login.tokenLifetime` needs no change: a web UI's OIDC sign-in gets the shorter of the two.
The cost: package clients never refresh a token, so at each expiry the person logs in again and
puts the new token where `docker login`, `pip.conf`, `.npmrc` or `settings.xml` keeps it.
Systems use deploy tokens, service accounts and CI tokens, which the key does not touch. A new
value counts for the logins after `serve` restarts with it; tokens minted before keep their
expiry.

### Requirement 10: logging and monitoring

| Requirement | Lajstrom: the control and its evidence | Outside Lajstrom, or the gap |
| --- | --- | --- |
| **10.2.1** Audit logs are enabled and active. | The audit log is not a setting: every write and every change of the control plane writes its row in the transaction of the change; with the database down the change fails, and nothing is buffered or dropped ([Audit](operations.md#audit)). Every request is an access log line. | Collect standard error. |
| **10.2.1.1** Individual user access to cardholder data. | Not applicable while no artifact holds cardholder data. Reads are not audit rows; the access log has each with its principal. | |
| **10.2.1.2** All actions of individuals with administrative access. | Each change through the API is an audit row with the principal, `source: api` and the request id; an admin's reads (users, bindings, the audit log) are access log lines with the principal. | **Gap:** `lajstrom admin`, against the database, writes its rows as `anonymous` with `source: cli` and the host, naming no person: [#1284](https://github.com/lajstrom/lajstrom/issues/1284). Decided, being built: every such row gets the OS user (`osUser`), and `--actor` and `--reason` when given ([rbac.md](spec/rbac.md) §6); the person behind a container's command is in the operator's records (the Kubernetes audit log, the bastion's). |
| **10.2.1.3** All access to audit logs. | `GET /api/v1/audit` is for the admin role, and each call is an access log line with its principal. | Reads of the `audit` table in the database are the database's to log. |
| **10.2.1.4** All invalid logical access attempts. | A refused credential is an access log line with `401`, the path, the client address and `principal=anonymous`; a refused action `403` or `404` with the principal; an account under throttle a `throttled` line at info with its uid. | **Gap:** the account a refused password was for is logged at debug only: [#1284](https://github.com/lajstrom/lajstrom/issues/1284). Decided, being built: an `authentication refused` line at info with the kind of credential, the account's uid (never the name typed), the reason, the client address and the request id, and a `login.block` audit row when a throttle trips ([rbac.md](spec/rbac.md) §1, §5). |
| **10.2.1.5** Changes to identification and authentication credentials: new accounts, elevation of privileges, changes to administrative accounts. | `user.*`, `binding.*`, `token.*`, `key.rotate`, `key.drop`; each OIDC login with its groups (`user.login`); the first admin (`user.create`, `source: bootstrap`). | A change of `auth.*` in the configuration file leaves no row: [#1284](https://github.com/lajstrom/lajstrom/issues/1284). Decided, being built: `config.change` at the start that runs a changed configuration, with the keys and their values, secrets excluded ([Changes of the configuration](configuration.md#changes-of-the-configuration)). |
| **10.2.1.6** Initialization, starting, stopping or pausing of audit logs. | The audit log cannot be stopped or paused. `serve` logs `listening`, `shutting down` and `stopped` at info. | |
| **10.2.1.7** Creation and deletion of system-level objects. | `repo.create`, `repo.delete`, `webhook.*`, `key.rotate`, `snapshot.create`, `snapshot.restore`. | |
| **10.2.2** Each event records the user, the type, the date and time, success or failure, the origin and the resource. | `principal` and `principalName` (the name at the time), `action`, `at` (UTC), `state` (`ok`; `pending` while a registry write runs; `failed` with its `error`; `unknown`), `ip` (the client behind `server.trustedProxies`), `repo`, `subject`, `digest`, and `details` with the source and the request id ([api.md](api.md#audit)). | The user of a command against the database: the OS user, `--actor` and `--reason` ([#1284](https://github.com/lajstrom/lajstrom/issues/1284), decided, being built). |
| **10.3.1** Read access to audit logs is limited to a job-related need. | The audit log is a system resource that only the admin role reads ([rbac.md](spec/rbac.md) §2). | Access to the database and its backups. |
| **10.3.2** Audit logs are protected from modification. | No API or command changes or deletes a row; a registry write's row moves once, from `pending` to its outcome. | **Gap:** whoever holds the database's credentials can change rows without trace: [#1278](https://github.com/lajstrom/lajstrom/issues/1278). |
| **10.3.3** Audit logs are promptly backed up to a central log server or to media that is difficult to modify. | The rows are in every backup of the database. `lajstrom admin snapshot create --audit 90d` (or `all`) copies them into a sealed snapshot in the registry ([Snapshots of the control plane](operations.md#snapshots-of-the-control-plane)). The access log goes where standard error is collected. | **Gap:** no export to a SIEM or to a write-once store: [#1278](https://github.com/lajstrom/lajstrom/issues/1278). Until then, a collector polls `GET /api/v1/audit?since=`. |
| **10.3.4** Change detection on audit logs. | | **Gap:** [#1278](https://github.com/lajstrom/lajstrom/issues/1278). |
| **10.4.1**, **10.4.1.1** Security events reviewed daily, by automated means. | What to review: audit rows; `throttled` lines and `lajstrom_auth_throttled_total`; `lajstrom_download_digest_mismatches_total` (anything but 0 is a registry that serves other bytes than it stored); audit rows with `state=unknown`; `proxy.reject`; `policy.refuse` (#101) ([Metrics](operations.md#metrics)). | The SIEM and the review. |
| **10.5.1** Audit log history kept for at least 12 months, the last three immediately available. | The audit table only grows: no row is deleted. | Keep 12 months of the access log in the log store, and the database backups (or the export of [#1278](https://github.com/lajstrom/lajstrom/issues/1278)) for the audit rows. |
| **10.6.1** to **10.6.3** Clocks synchronised from designated time servers; time settings protected. | The replicas, the CLI and the database must agree within 60 seconds. `serve` compares its clock with the database's at start and every 30 seconds, warns past 10 seconds and is not ready past 60; `lajstrom reindex` and the `admin` commands write nothing past 60; `lajstrom_clock_skew_seconds` ([Clocks](operations.md#clocks)). An audit row's time is the clock of the replica that wrote it. | NTP on every host, the designated time servers of 10.6.2, the protection of time settings of 10.6.3. |
| **10.7.2** Failures of critical security controls are detected and alerted. | A failed audit write fails its change. `/-/readyz` answers `503` while the database does not answer, a registry has not answered for a minute or the clock is off ([Readiness](operations.md#readiness)). | Alerts on readiness and on the log pipeline. |

### Requirements 11.5 and 12.10: change detection and incident response

| Requirement | Lajstrom: the control and its evidence | Outside Lajstrom, or the gap |
| --- | --- | --- |
| **11.5.2** Change detection on critical files, compared at least weekly. | For artifacts: a tag never moves; a whole blob or a manifest streamed to a client is hashed and the answer aborted on a mismatch ([guarantees.md](spec/guarantees.md) INV-40); `lajstrom reindex` and the reconcile loop compare the registry with the index, report a tag re-pointed in the registry as drift and never follow it, list what is not Lajstrom's as foreign, and the loop writes a `reconcile` audit row for each repo it changed ([What a run does](operations.md#what-a-run-does)); apt and rpm metadata are signed. | Lajstrom's binary, image and configuration are files of the host: the organisation's change detection, image digests pinned. |
| **12.10.1**, **12.10.5** An incident response plan, and responses to alerts. | What contains an incident: disable a user, revoke a token, delete a binding; `lajstrom admin key rotate --token --drop` (every `/v2` bearer); `lajstrom admin key rotate --repo` with `keys.retireGrace: 0s` (a leaked signing key); a deny rule or a deny verdict, and `enforcePull` (#101); delete a version. Webhooks announce publishes and deletes ([Webhooks](operations.md#webhooks)). | The plan, the runbooks, the alerting. |

## SOC 2 Trust Services Criteria

### CC6: logical and physical access

| Criterion | Lajstrom: the control and its evidence | Outside Lajstrom, or the gap |
| --- | --- | --- |
| **CC6.1** "The entity implements logical access security software, infrastructure, and architectures over protected information assets to protect them from security events to meet the entity's objectives." | RBAC on every route (requirement 7 above); authentication (requirement 8); keys sealed with the master key or held in a KMS (requirements 3 and 4); the backend registry trusts Lajstrom's credentials alone. | Network segmentation, the IdP. **Gaps:** MFA for administrators ([#1276](https://github.com/lajstrom/lajstrom/issues/1276)), the master key's replacement ([#1288](https://github.com/lajstrom/lajstrom/issues/1288)). |
| **CC6.2** "Prior to issuing system credentials and granting system access, the entity registers and authorizes new internal and external users whose access is administered by the entity. For those users whose access is administered by the entity, user system credentials are removed when user access is no longer authorized." | An admin creates local users and service accounts. An OIDC user is created by its first login and gets only what its groups' bindings give, so the IdP's assignment of the person is the registration ([OIDC login and groups](auth.md#oidc-login-and-groups)). Disabling or deleting a user revokes its tokens. *Evidence:* `user.create`, `user.login`, `user.disable`, `user.delete`. | **Gaps:** removal at the IdP ([#1282](https://github.com/lajstrom/lajstrom/issues/1282)), inactive users ([#1283](https://github.com/lajstrom/lajstrom/issues/1283)), the review ([#1279](https://github.com/lajstrom/lajstrom/issues/1279)). |
| **CC6.3** "The entity authorizes, modifies, or removes access to data, software, functions, and other protected information assets based on roles, responsibilities, or the system design and changes, giving consideration to the concepts of least privilege and segregation of duties, to meet the entity's objectives." | Roles on masks; `attacher` separates signing and scanning from publishing; deploy tokens never delete or manage; `maintainer` does not reach the system. | **Gaps:** approvals ([#1285](https://github.com/lajstrom/lajstrom/issues/1285)), the review ([#1279](https://github.com/lajstrom/lajstrom/issues/1279)). |
| **CC6.4**, **CC6.5** Physical access; discontinuing protections over physical assets. | Lajstrom keeps no artifact bytes; with encrypted repos (#114) the registry's copy is unreadable without Lajstrom's keys. | The registry's storage, the database's disks, their disposal. |
| **CC6.6** "The entity implements logical access security measures to protect against threats from sources outside its system boundaries." | The throttles of failed logins and of `/v2/token`; the browser headers and the cross-site refusal; a webhook reaches only public addresses over `https` unless `webhooks.allowedHosts` names its host ([guarantees.md](spec/guarantees.md) INV-44), and a proxy repo follows no URL or redirect of its upstream to a private address (threat model O-2); an upstream's credentials go to its own origin only (O-1); forwarded headers and client-certificate headers count only from `server.trustedProxies` (N-1, W-1). | The edge's TLS and rate limits ([threat model](security/threat-model.md) §4). |
| **CC6.7** "The entity restricts the transmission, movement, and removal of information to authorized internal and external users and processes, and protects it during transmission, movement, or removal to meet the entity's objectives." | Reading a repo needs `read` and is logged; deleting needs `delete` and is audited. A presigned blob redirect goes only to a reader and is off with `policy.blobRedirect: never`; an encrypted repo never redirects (#114). Webhooks carry no credential. | TLS, and the edge's hop. |
| **CC6.8** "The entity implements controls to prevent or detect and act upon the introduction of unauthorized or malicious software to meet the entity's objectives." | Immutable versions; digests checked on upload (threat model P-9) and on download (INV-40); a proxy repo checks its upstream's checksums and refuses what does not match, as `proxy.reject` (O-3); deny rules, quarantine and verdicts (#101); SBOMs and scan reports with verification labels (#89). | Scanning. **Gap:** serving only versions with a verified attachment: [#1287](https://github.com/lajstrom/lajstrom/issues/1287). |

### CC7: system operations

| Criterion | Lajstrom: the control and its evidence | Outside Lajstrom, or the gap |
| --- | --- | --- |
| **CC7.1** "To meet its objectives, the entity uses detection and monitoring procedures to identify (1) changes to configurations that result in the introduction of new vulnerabilities, and (2) susceptibilities to newly discovered vulnerabilities." | The configuration is validated at start, and risky settings are logged at start (an ephemeral token key, `insecureHTTP` off loopback, per-address throttles off). Scan reports and `?without=scan` (#89). | **Gap:** a configuration change leaves no record in Lajstrom: [#1284](https://github.com/lajstrom/lajstrom/issues/1284). |
| **CC7.2** "The entity monitors system components and the operation of those components for anomalies that are indicative of malicious acts, natural disasters, and errors affecting the entity's ability to meet its objectives; anomalies are analyzed to determine whether they represent security events." | The metrics ([Metrics](operations.md#metrics)), the access log, the audit log, readiness. | The monitoring and its analysis. **Gap:** the export of the audit log: [#1278](https://github.com/lajstrom/lajstrom/issues/1278). |
| **CC7.3** "The entity evaluates security events to determine whether they could or have resulted in a failure of the entity to meet its objectives (security incidents) and, if so, takes actions to prevent or address such failures." | Rows that call for an evaluation: `state=unknown`, `proxy.reject`, `policy.refuse` (#101). | The evaluation. |
| **CC7.4** "The entity responds to identified security incidents by executing a defined incident-response program to understand, contain, remediate, and communicate security incidents, as appropriate." | The containment of 12.10 above. | The program. |

### CC8.1: change management

| Criterion | Lajstrom: the control and its evidence | Outside Lajstrom, or the gap |
| --- | --- | --- |
| **CC8.1** "The entity authorizes, designs, develops or acquires, configures, documents, tests, approves, and implements changes to infrastructure, data, software, and procedures to meet its objectives." | Artifacts: as 6.5.1 above (immutable versions, the audit of publishes, provenance, CI identities of #937). The control plane: every change audited; repos and bindings declared in git and reviewed there are decided ([ADR 0015](adr/0015-declarative-management-one-owner-per-object.md)) and not built. Lajstrom itself: migrations are expand or contract, the release notes name a contract one, and after an expand migration the release before still runs, so a rolling update and a rollback work ([Upgrades](operations.md#upgrades)); signed releases. | The change process. **Gap:** approvals of privileged changes: [#1285](https://github.com/lajstrom/lajstrom/issues/1285). |

### A1: availability

| Criterion | Lajstrom: the control and its evidence | Outside Lajstrom, or the gap |
| --- | --- | --- |
| **A1.1** "The entity maintains, monitors, and evaluates current processing capacity and use of system components (infrastructure, data, and software) to manage capacity demand and to enable the implementation of additional capacity to help meet its objectives." | Request, registry and database durations, upload sessions and spool in the metrics; bounds that answer `503` or `429` instead of failing (`server.maxConcurrentUploads`, `server.uploadSessions`, `server.spoolBudget`, [Limits](operations.md#limits)); the chart's autoscaling. | Capacity of the registry, the database and the edge. |
| **A1.2** "The entity authorizes, designs, develops or acquires, implements, operates, approves, maintains, and monitors environmental protections, software, data backup processes, and recovery infrastructure to meet its objectives." | Replicas on one Postgres, one per node, a disruption budget ([deploy/helm.md](deploy/helm.md#high-availability)); readiness that takes a replica out ([Readiness](operations.md#readiness)); database backups, snapshots of the control plane in the registry, a disaster-recovery runbook ([Disaster recovery](operations.md#disaster-recovery)). | The registry's own backup, the database's replication, the master key's copy. |
| **A1.3** "The entity tests recovery plan procedures supporting system recovery to meet its objectives." | `test/e2e/snapshot.sh` restores a snapshot into SQLite and into Postgres at every end-to-end run; operations.md records a rehearsal of a backup's restore; `test/e2e/ha.sh` runs nightly against two replicas behind nginx ([The ha stack](testing.md#the-ha-stack)). | The organisation's own rehearsals. |

### C1: confidentiality

| Criterion | Lajstrom: the control and its evidence | Outside Lajstrom, or the gap |
| --- | --- | --- |
| **C1.1** "The entity identifies and maintains confidential information to meet the entity's objectives related to confidentiality." | Repos are private unless both `anonymousRead` switches are on; reads are per repo. Encrypted repos keep the bytes from the registry's operator (#114). Snapshots of the control plane are sealed by default. | The classification: name repos by it, so that masks follow it. |
| **C1.2** "The entity disposes of confidential information to meet the entity's objectives related to confidentiality." | Deleting a version deletes its attachments and its manifests in the registry, on a registry that deletes manifests (`registries[].capabilities.deleteManifest`), audited; `lajstrom admin retention` deletes by rule ([Retention](operations.md#retention)). | The registry's garbage collection removes the blobs. Database backups and snapshots keep the names and versions of what was deleted. A rotation of an encrypted repo's key does not re-encrypt what is stored (#114). |

## Where the evidence is

| Record | Where | Who reads it | How long | How it leaves Lajstrom |
| --- | --- | --- | --- | --- |
| The audit log | the `audit` table of the index | the admin role, `GET /api/v1/audit`; SQL | no row is deleted | database backups; `lajstrom admin snapshot create --audit`; [#1278](https://github.com/lajstrom/lajstrom/issues/1278) |
| The access log and the application log | standard error | the log store's readers | the log store's | the log pipeline |
| Metrics | `server.metricsListen` | Prometheus | Prometheus's | |
| A webhook's delivery log | the `webhook_deliveries` table | the webhook's manager | `webhooks.retention`, 7 days | the receiver |
| Attachments: SBOMs, scan reports, provenance, signatures | the registry, referrers of each version | readers of the repo | as long as the version | `oras pull`, `cosign verify-attestation` |
| Snapshots of the control plane | the registry, `lajstrom/control-plane` | the operator, with the master key | the registry's retention rules | they are in the registry |
| The configuration | the file; the startup log | the operator | git | |
| Lajstrom's release signatures and SBOMs | the GitHub release, `ghcr.io`, `docker.io` | anyone | | `cosign verify` ([release.md](release.md#verifying-a-release)) |

## Gaps

| Gap | Requirement | Issue |
| --- | --- | --- |
| MFA for administrative access: an admin mode for OIDC logins with `amr`/`acr` checks | PCI DSS 8.4.1, 8.4.2, 8.5.1; CC6.1 | [#1276](https://github.com/lajstrom/lajstrom/issues/1276) |
| An idle timeout for the web UI's sign-in token: `auth.login.idleTimeout` (decided, being built) | PCI DSS 8.2.8 | [#1277](https://github.com/lajstrom/lajstrom/issues/1277) |
| Export of the audit log, tamper evidence, retention | PCI DSS 10.3.2 to 10.3.4, 10.5.1; CC7.2 | [#1278](https://github.com/lajstrom/lajstrom/issues/1278) |
| An access report for the periodic review, and its acknowledgement | PCI DSS 7.2.4, 7.2.5.1; CC6.2, CC6.3 | [#1279](https://github.com/lajstrom/lajstrom/issues/1279) |
| A lockout of 30 minutes, or no local passwords | PCI DSS 8.3.4 | [#1280](https://github.com/lajstrom/lajstrom/issues/1280) |
| A password policy for local users | PCI DSS 8.3.5 to 8.3.7, 8.3.9 | [#1281](https://github.com/lajstrom/lajstrom/issues/1281) |
| Deprovisioning from the IdP: asking it again with each login's sealed refresh token; until then, the [offboarding procedure](#offboarding-a-person) | PCI DSS 8.2.5; CC6.2 | [#1282](https://github.com/lajstrom/lajstrom/issues/1282) |
| A user's last activity; disabling inactive users: `auth.disableInactiveUsersAfter` (decided, being built) | PCI DSS 8.2.6 | [#1283](https://github.com/lajstrom/lajstrom/issues/1283) |
| The person behind `lajstrom admin`, refused passwords at info, configuration changes (decided 2026-11-16: the OS user and `--actor`/`--reason`; refusals at info and `login.block`; `config.change`) | PCI DSS 10.2.1.2, 10.2.1.4, 10.2.2; CC7.1 | [#1284](https://github.com/lajstrom/lajstrom/issues/1284) |
| Approval of privileged changes | PCI DSS 7.2.3, 8.2.4, 6.5.4; CC6.3, CC8.1 | [#1285](https://github.com/lajstrom/lajstrom/issues/1285) |
| Serving only versions with a verified SBOM, provenance or signature | PCI DSS 6.3.2, 6.5.4; CC6.8 | [#1287](https://github.com/lajstrom/lajstrom/issues/1287) |
| Replacing the master key | PCI DSS 3.7.5 where the keys are in scope; CC6.1 | [#1288](https://github.com/lajstrom/lajstrom/issues/1288) |

Not gaps, though they are asked about: the synchronisation of clocks ([Clocks](operations.md#clocks)),
"deny all" by default (7.3.3 above), vendor default accounts (2.2.2 above), an audit log that
cannot be turned off (10.2.1 above), and MFA and a second admin for CI write policies (decided on #937, being built).
