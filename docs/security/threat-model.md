# Threat model

- Status: first pass, 2026-11-09, at `129dc47` (#63). It is for the first public release
  (v0.1.0). Review it again when a surface changes: a new endpoint, a new credential, a proxy
  repo, OIDC.
- Scope: the server of this repository. This means the OCI facade `/v2` and its token
  endpoint, the frontends (raw, apt, pypi, maven), the management API `/api/v1`, the ingest
  pipeline, the provider towards the registries, the index, and the logs. The registries,
  the database server and the edge are in scope only for what Lajstrom expects of them.
- Method: STRIDE over the flows of [architecture.md](../architecture.md) §3. For every threat
  below, the code that stops it was found and read. Where it helped, a throwaway test or a
  probe against the dev stack confirmed it. Where nothing stops it, the threat is **open**, with
  its issue.

Each threat has an ID and one of these statuses:

- **mitigated**: with the function that does it;
- **fixed in this pass**: with the pull request;
- **by the backend**: the registry's job, with how it was checked;
- **accepted**: a decision, with where it is written;
- **open**: with the issue that tracks it.

A function is named by its package and name: `facade.resolve` is `resolve` in
`internal/facade`, `generic.Provider.location` is the method in `internal/provider/generic`,
`raw.checkPath` is in `internal/format/raw`.

## 1. What is protected, from whom

| Asset | Where it lives | What losing it means |
| --- | --- | --- |
| Artifact bytes and their integrity | the registry only ([ADR 0001](../adr/0001-oci-registry-as-sole-storage.md)) | a client installs what an attacker chose |
| Tags: immutable, append-only ([ADR 0005](../adr/0005-manifest-profile-and-immutable-tags.md)) | the registry; claims in the index | a version silently changes under its name |
| The registry's credentials | `registries[].auth` in the configuration, held by the provider | every repository of the instance prefix, read and write, around RBAC |
| The control plane: users (argon2id hashes), tokens (sha256), bindings, audit | the index database | anyone becomes admin; the audit log lies |
| The master key, and the keys it encrypts (apt GPG keys, the bearer-token key) | `keys.masterKeyEnv` or `keys.masterKeyFile`; `keys` rows | forged `Release` signatures; forged bearer tokens for every repo |
| Availability | every replica | a CI fleet stops |

| Actor | Can | Trust |
| --- | --- | --- |
| anonymous client | reach the edge | none; reads repos with `anonymousRead` where `auth.anonymousRead` allows it |
| user (reader, writer, maintainer, admin) | what its bindings allow ([rbac.md](../spec/rbac.md) §3) | its bindings |
| holder of a PAT | its user's rights, narrowed by the scopes | as the user |
| holder of a deploy token | read and/or write on one repo | the token's scopes |
| holder of a stolen bearer JWT | its `access` for up to 20 minutes, from anywhere | the token's claims |
| the edge | sets `X-Forwarded-For` | only when its address is in `server.trustedProxies` ([ADR 0011](../adr/0011-client-address-behind-a-proxy.md)) |
| the registry | stores and answers | trusted to store and serve content. Clients check the digests they are given: OCI clients the descriptor, apt the `Packages` hashes, pip the `#sha256=`. Ingest checks what it reads back. The facade hashes a blob that a client uploads as it streams (P-9), not one it serves. Its redirects and token realm are checked before its credentials follow them (P-2, P-3) |
| the operator: configuration, CLI, database | everything | trusted; out of scope, as [SECURITY.md](../../SECURITY.md) has it |

### What the backend trusts

The registry trusts whoever holds its credentials, and Lajstrom holds them. Every check of
this document happens in Lajstrom. A client that reaches the registry directly bypasses all
of them: RBAC, immutability, the profile and the audit log. So:

- **the network must keep clients away from the registry**, or the registry must accept only
  Lajstrom's credentials (rbac.md §4);
- a presigned URL that a blob `307` hands out is the one deliberate exception. It is
  short-lived, names one blob, and only reaches a client that was allowed to read that blob.
  `policy.blobRedirect: never` turns it off for a repo.

## 2. STRIDE over the flows

The table summarises; the IDs point to §3.

| Flow | Spoofing | Tampering | Repudiation | Information disclosure | Denial of service | Elevation of privilege |
| --- | --- | --- | --- | --- | --- | --- |
| A. `oras push` through `/v2` | T-3, T-5, A-1 | P-1, P-9, M-1, M-3, F-5 | A-3 | R-1, R-2, P-5 | M-2, U-3, U-4, T-7 | T-1, P-7, T-9 |
| B. `mvn deploy`, twine, raw and apt `PUT` | A-1, A-5 | F-1, F-2, F-5, U-5 | A-4 | R-4 | F-4, U-5, U-6 | R-4 |
| C. reads: apt, pip, maven, raw, `/v2` | T-3 | P-2 | none (reads are not audited, rbac.md §5) | R-1, R-2, P-5, B-1 | U-6 | none |
| Token flow `/v2/token` | T-2, T-3, T-5 | none | none (tokens are not audited, rbac.md §5) | T-9 | T-7, A-1 | T-1, T-6, T-8 |
| Management API `/api/v1` | A-1, B-2 | B-2 | A-4 | R-5 | A-1 | B-2, T-8 |
| E. reindex, reconcile | none | F-4, F-5 | none | none | F-4 | none |
| Logs and metrics | none | none | L-1 | L-1 to L-4 | none | none |

## 3. Threats

### Proxying to the registry (`/v2` and the provider)

| ID | Threat | Mitigation | Status |
| --- | --- | --- | --- |
| P-1 | **A name that reaches another repo's OCI repository.** The cases are `..`, `%2F`, `//`, uppercase, a trailing `/`, a `blobs` or `manifests` segment inside the name, or a prefix of one repo that nests in another's. | `facade.resolve` checks `nameGrammar` (no `..` and no empty segments) and `maxNameLen` on the decoded path. `facade.parseRoute` matches the fixed segments from the end, so the name stays below its repo. A repo's prefix is always `<format>/<name>`: `api.Repos.newRow` sets it, and neither part can contain `/`. The provider builds URLs from the checked name only. Two registries on one URL (scheme, host and port) whose instance prefixes nest, the empty prefix included, are refused at load, naming both (`config.validateNesting`, #250; test `TestLoadNestedPrefixes`). | mitigated |
| P-2 | **The backend's credentials sent where they should not go: a redirect.** | oras-go's `auth.Client` (the `follow` client) drops `Authorization` when a redirect leaves the origin. `generic.Provider.location` resolves an upload session's `Location` against the registry and refuses another host or port, or a downgrade from https to http. `generic.Provider.blobRequest` follows the hops of `OpenBlob` and `StatBlob` itself, through the `noFollow` client, whose token cache is keyed by host and port without the scheme: it refuses a hop from https to http, and once a hop leaves the registry's origin it sends the rest of the chain through `clients.anonymous`, which has no credentials and no cache, even back to the origin (#242). Tests: `TestBlobRedirectDowngrade` (one port that speaks TLS and plain HTTP, so the http hop has the cache's key), `TestBlobRedirectLeavesOrigin`. | mitigated |
| P-3 | **The backend's credentials sent to a token realm the backend names.** | `generic.realmTransport` refuses the registry's bearer challenge before the auth client sees it when its realm (`refuseRealm`) is not an absolute http(s) URL, is http for a registry on https, or is on another host name than the registry's that the operator did not allow; another port of that host is allowed, a token server beside the registry (Harbor, GitLab in the dev stack). Another host name is allowed only over https and only when `registries[].tokenRealmHosts` lists it exactly (no wildcards, no public-suffix rule), or the vendor table does for the registry's host name: `gitlab.com` for `registry.gitlab.com`, `auth.docker.io` for Docker Hub (`generic.realmHosts`, #397). So nothing goes to any other realm, and the error names the registry and the realm's host, not a URL (`generic.realmError`, `translate`; #242). oras-go `validateRealm` still applies its own checks after it: a realm on another host name may not be or resolve to a loopback, link-local or cloud metadata address. Tests: `TestTokenRealm`, `TestRefuseRealm`. | mitigated |
| P-4 | **The backend's URLs reach the client.** | `facade.answerSession` answers its own upload id (`uploadLocation`); the backend's session URL stays in the `sessions` row. Backend errors are translated and their text cleaned (`generic.translate` into a `backendError`), and 5xx messages are not answered (`facade.fail`). A cross-origin blob redirect is the one exception, by design (P-6). | mitigated |
| P-5 | **A blob of another repository read by digest.** A reader of repo A asks for a digest that only repo B holds, with `HEAD`/`GET …/blobs/<digest>` or as a layer of a manifest it pushes. | Lajstrom asks the registry in the requested OCI name only (`format.ServeBlob`, `facade.checkLayers` → `Repository.Exists`), and scoping a blob to its repository is the registry's job. Probed on 2026-11-09 against Zot v2.1.23 of the dev stack with `dedupe: true`: a blob pushed to `a` is `404` on `HEAD` and `GET` in `b` and `c`. distribution scopes blobs by repository links. | by the backend; measured on Zot; check it again for each new registry ([registries/README.md](../registries/README.md)) |
| P-6 | **A presigned URL leaks.** | It names one blob, expires as the backend sets it, and only goes to a principal allowed to read the blob. `policy.blobRedirect: never` and `capabilities.blobRedirect: false` keep blobs on Lajstrom's connection. | accepted ([http-routing.md](../spec/http-routing.md) §2) |
| P-7 | **Confused deputy: a cross-repo mount from a repo the client may not read.** | `facade.mountSource` passes `from` to the backend only when the bearer names it with `pull` (or the principal may read it, without a bearer) and the source is on the same registry; otherwise the answer is a plain upload session. This pass makes the bearer check not depend on `token.ValidName` and `resolve` accepting the same names. | mitigated; hardened in #260 |
| P-8 | **A client reaches the registry directly.** | Outside Lajstrom: the network, or credentials only Lajstrom holds (§1). | the deployment's job ([reverse-proxy.md](../reverse-proxy.md), [operations.md](../operations.md)) |
| P-9 | **A blob stored under a digest it does not have.** A registry that checks no digest (JFrog JCR 7.161, [registries/jfrog-jcr.md](../registries/jfrog-jcr.md#uploads-and-digests)) stores what a `/v2` upload sends under the digest the client names, and serves it so; integrity, signatures and dedup rest on that digest. | `facade.verifier` hashes the bytes as they stream to the backend and holds the last byte back until the hash is known: a closing `PUT` or `POST ?digest=` whose bytes do not have the digest is `400 DIGEST_INVALID`, the backend's request breaks off one byte short, and the session ends. Between chunks the sha256 state is in the session row (`uploadState`), so any replica continues it. A chunk sets `Recheck` in the row before it streams; after one that failed, `facade.closingHash` asks the backend's offset before the close, and a session whose backend holds bytes the hash did not see ends. Chunks are checked in sha256 only: a chunked upload closed with another algorithm is `400`. Mounts and blobs the client skips stream nothing and rely on the backend (P-5). Residue: two requests of one session at once, which only its owner can send (U-1), on a backend that checks neither a chunk's offset nor the digest; JCR checks offsets. Tests: `TestUploadVerifiesDigest`, `TestUploadVerifiedAcrossReplicas`, `TestUploadBrokenBody`. | fixed in #368; residue accepted |

### The token flow (`/v2/token` and the bearer on `/v2`)

| ID | Threat | Mitigation | Status |
| --- | --- | --- | --- |
| T-1 | **A token wider than the credential.** This could come from a scope with uppercase, `..` or a trailing slash, from `*`, from duplicates, or from a name that resolves on `/v2` to another repo than the one authorised. | `token.ParseScopes` drops a name that fails `token.ValidName` (the lowercase grammar, at least `<format>/<repo>/<path>`); `*` means pull, push and delete; duplicates are unioned. `token.Reduce` applies `Credential.Limit` (the PAT's scopes; a deploy token's repo and scopes; never `delete` or `manage`) and then the Authorizer, per action. `facade.unknownScopes` empties the scope of a repo that does not exist. On `/v2`, `facade.namedByToken` matches the exact name before `resolve`, and every endpoint calls `facade.authorize` → `allowedByToken`. | mitigated |
| T-2 | **Audience and issuer.** | `Verifier.Verify` requires `iss` and `aud` to be the service. The service is `server.externalURL`'s host, or, without it, the request's `Host`. With it empty, the client chose the audience: a staging server restored from production's database with the same master key shares the token key, and its tokens for `Host: <production>` verified there. Since #248 `Server.validateExternalURL` (`internal/config`) refuses an empty `server.externalURL` unless `server.insecureHTTP` is on, the dev stack, so a server's service is its own; tests: `TestExternalURL`. A copy restored on another server with the same `externalURL` still shares the key: [operations.md](../operations.md#restoring) has it run `lajstrom admin key rotate --token --drop` there. | mitigated (#248) |
| T-3 | **`kid` and algorithm confusion** (`none`, HMAC with the public key, an unknown `kid`). | `token.kidOf` reads the `kid`. `Verifier.key` takes the key from `keys.Tokens.TokenVerifiers`: active, or retired within the grace, decrypted and checked against its fingerprint. `WithValidMethods` pins the algorithm of that key (ES256 or EdDSA), and the keyfunc returns an EC or Ed25519 key, so HS256 and `none` fail. An unknown `kid` reloads the keys at most once a second, then `401`. A replica that cannot reach the database keeps its last keys, so a dropped key verifies there until it can. | mitigated |
| T-4 | **Expiry, replay and revocation.** | `exp` is required; `nbf` and `iat` are checked with 60 s of skew. A token is a bearer: nothing binds it to a client, and a revoked credential, binding or user still has its tokens for up to 20 minutes. `lajstrom admin key rotate --token --drop` ends every token within a second. The same window applies to a repo deleted and created again under the same name. | accepted ([rbac.md](../spec/rbac.md) §4, [ADR 0010](../adr/0010-bearer-token-signing-key.md)) |
| T-5 | **The uid in `sub` (#182).** | `Verifier.Verify` refuses a `sub` of kind user that is not a uid (`index.IsUID`), which a token from before #182 has. `sub` is trusted beyond `access` in four places: the owner of an upload session (`facade.continueSession`); the ingest principal, which becomes the audit principal and `lajstrom.uploaded-by`; the anonymous check of `facade.repoUnknown`; and `_catalog` (T-9). A disabled or deleted user's token pushes until `exp` (T-4), and its audit principal resolves to the kept row. | mitigated |
| T-6 | **A stolen deploy token.** | `Credential.Limit`: its own repo only, with `read` and/or `write` as its scopes; another repo is `404`, and `delete` and `manage` are `403`. The API's token routes (`allowTokens`, `allowDeployTokens`) refuse it, and `whoami` answers `token:<id>` only. Through `/v2/token` it gets the same, as `token.Reduce` applies the same `Limit`. What remains: it lives until it is revoked or expires, which is optional; its JWTs live 20 minutes past revocation (T-4); and a write-only token can probe whether a blob exists in its own repo through a same-repo mount. | mitigated; residue accepted |
| T-7 | **Amplification through the token endpoint.** A request without credentials and with 20 000 scopes made 20 000 `GetRepo` calls and a 3 MB answer. | The per-address throttle of `/v2/token` (100 a second, `Authenticator.AllowTokenRequest`, with `trustedProxies` or `server.tls`). This pass bounds a request to 100 repositories: `400` beyond. | fixed in #262 |
| T-8 | **A stolen PAT mints PATs** that outlive its revocation. `api.allowTokens` let an unscoped PAT create its user's PATs. | Decided on #249: a PAT is created with a password only. `auth.MayCreatePAT` refuses a request whose credential is a PAT, with scopes or without, or a deploy token, with `ErrScope`: `api.createToken` asks it before anything else (`403` before the body is read), and `auth.Tokens.CreatePAT` asks it again for every caller. The CLI has no credential and is the operator's. The `token.create` row names the creating credential (`createdBy`: `password` or `token:<id>`), so a deploy token made with a PAT is tied to it. Tests: `TestTokensAPINoMint`, `TestTokensAPIAuthorization`, `TestTokensCreatedBy`, e2e `tokens.sh`. A stolen unscoped PAT still creates deploy tokens where its user has `manage`; they are confined to one repo and audited with `createdBy`. | mitigated |
| T-9 | **`_catalog` ignores a PAT's scopes.** A write-only PAT got `registry:catalog:*` and listed every repo its user can read. | `facade.catalog` filters by `format.Authorize` without the credential, which a bearer request no longer has. This pass makes `token.Reduce` keep the catalog scope only for a credential that may read. | fixed in #262 |

### RBAC: the 401/404/403 rule ([rbac.md](../spec/rbac.md) §3)

| ID | Threat | Mitigation | Status |
| --- | --- | --- | --- |
| R-1 | **`/v2` with a bearer tells whether a repo exists.** | `facade.repository` asks the token (`namedByToken`) before `resolve`. A repository the token does not name is `401 insufficient_scope`, whether it exists or not. One it names with no action answers as a repo that does not exist. The token endpoint names every repository asked for, with `actions: []` for a repo that does not exist or may not be read (`unknownScopes`, `Reduce`). | mitigated |
| R-2 | **`/v2` without a bearer** (Basic, or anonymous) **tells whether a repo exists** by checking the input before the permission. On an existing repo a malformed digest or tag answered `400 DIGEST_INVALID`/`TAG_INVALID` or `404 MANIFEST_UNKNOWN`; on a missing one it answered `404 NAME_UNKNOWN`, or `401` to anonymous. The routes were `getBlob`, `getManifest`, `putManifest` and `putUpload`. The messages of the two `NAME_UNKNOWN` also differed. | This pass authorises before parsing in the four handlers. `facade.authorize` answers a denial with the answer of a repo that does not exist, word for word (`nameUnknown`, `repoUnknown`). | fixed in #260 |
| R-3 | **Lists tell whether a repo exists.** | `GET /api/v1/repos` and `/v2/_catalog` show only what the principal may read (`facade.catalog`, `api`). Creating a repo, bindings, users and audit are admin only, and are checked before the body is read. | mitigated |
| R-4 | **A PAT's scope tells whether a repo exists.** A read-only PAT got `403` for a write on any existing repo, even one its user cannot read, and `404` on a missing one; this held on the frontends and on `/v2`. | `format.Authorize` applies the credential (`limit`) before the bindings. This pass answers `404` when the credential refuses and the user may not read the repo. | fixed in #260 |
| R-5 | **Timing.** A denial on a repo that exists may take a database read longer. | none | accepted (rbac.md §3) |

### Uploads and sessions

| ID | Threat | Mitigation | Status |
| --- | --- | --- | --- |
| U-1 | **Hijacking another principal's upload session.** | The id is 128 random bits (`facade.newUploadID`), and its form is checked (`validUploadID`). `facade.continueSession` authorises `write` on every request, loads the row of this repo, and refuses an expired row, another principal (`s.Principal`) or another name (`st.Name`) with `BLOB_UPLOAD_UNKNOWN`. The row is deleted at the end (`dropSession`). | mitigated |
| U-2 | **A spooled upload outlives its request, or is served.** | `format.Spool` unlinks the file at creation where the system allows and removes it on `Close`. The generic provider's spool of a `/v2` chunk of unknown length (`registries[].uploads.spoolUnknownLength`, #425) unlinks its file at creation, or does not spool, and closes it by `defer`. `pypi.readerAt` removes its own by `defer`, which covers errors, panics and a client that goes away. Files are created by `os.CreateTemp` with a constant pattern and mode 0600, so no client name reaches a path. Nothing serves from them. A `SIGKILL` leaves the not-yet-unlinked ones in `$TMPDIR`. | mitigated |
| U-3 | **A `/v2` blob upload without a size bound.** Neither `policy.maxFileSize` (#60 puts it in ingest, which a blob upload never reaches) nor `capabilities.maxBlobSize` was read. | `facade.limitBlob` bounds the monolithic `POST ?digest=`, `PATCH` and `PUT ?digest=` of a session by `facade.blobLimit`: the repo's `maxFileSize`, by default its format's (`format.MaxFileSize`), an `oci` repo's too, or the registry's `maxBlobSize` when smaller (GHCR's 10 GB from the vendor table). A declared size past it is `413 SIZE_INVALID` naming the bound before anything streams; a body of unknown length is cut where the session's bytes, counted across its `PATCH`es and the closing `PUT`, pass it (`tooLargeBody`); the session ends. Tests: `TestUploadMaxFileSize`, `TestUploadMaxBlobSize`, `TestUploadDefaultMaxFileSize`. | mitigated (#246) |
| U-4 | **Open upload sessions without a bound.** Each holds a backend session and a row for an hour. | `facade.openSession` counts a new session of a principal before the backend's session is opened (`admitUpload`, `liveUploads`): beyond `server.uploadSessions.perRepo` live sessions in the repo, 256 by default, or `perPrincipal` in all repos, 1024, it is `429 TOOMANYREQUESTS` with `Retry-After` and nothing opens; a mount that mounts holds none. A session is live while a request on it runs and for 5 minutes after (`liveWindow`): clients leave the sessions of a failed push, and an idle one takes no slot. `DELETE`, the closing `PUT` and an error that ends a session free its slot (`dropSession`). The count is per replica, as the throttles are (A-1): a session ended on another replica is forgotten at the bound, once the index says it is gone. At most 10 000 principals are counted; beyond, a new one passes uncounted. Tests: `TestUploadLiveSessions`, `TestUploadSessionsOfAnotherReplica`, `TestLiveUploads`, `TestLiveUploadsPrincipals`. | mitigated (#246); per replica |
| U-5 | **Maven sessions:** no bound on their files, and a writer who opens a version's session first holds it (`409` for the real deploy). | `ingest.Pipeline.Add` refuses a file past the repo's `maven.maxSessionFiles` in a session, 256 by default and at most 999 (`maxFilesOf`, `sessionState.room`), before it is stored and again under the session's lock: `400` naming the bound and the key; so every `Add` writes at most that many entries. Checksum files and `maven-metadata.xml` never reach a session. `maven.sessionTimeout` (#60) ends an idle session. Tests: `TestDeploySessionFiles`, `TestDeploySessionFilesCounted`, `TestSessionFilesPolicy`. The hold stays: a writer of the repo can keep the session of a version open, but can as well publish the version first, which a tag makes final; a hold takes nothing a writer does not have. | mitigated (#246); the hold accepted |
| U-6 | **`$TMPDIR` filled by frontend uploads.** `maxFileSize` was unlimited and `maxConcurrentUploads` off by default; `stallTimeout` does not stop a body that keeps moving. | Both have a default now: `policy.maxFileSize` by format, npm 512 MiB, pypi and maven 4 GiB, apt and raw 8 GiB, oci 16 GiB (`format.MaxFileSize`), and `server.maxConcurrentUploads` 32 (`config.DefaultMaxConcurrentUploads`), so a replica spools at most 256 GiB at once, a twine upload twice its file; [operations.md](../operations.md#limits) has every bound and how to size `$TMPDIR`. Tests: `TestMaxFileSize`, `TestMaxConcurrentUploads`. | mitigated (#246) |
| U-7 | **A repo's policy set to something that breaks it.** `checkPolicy` accepts any object, and `PATCH` needs `manage`: a non-numeric `maxFileSize` makes every upload a `500`, and a manager can lift the cap. | none yet | open: #60 (typed, validated policy) |

### Manifests

| ID | Threat | Mitigation | Status |
| --- | --- | --- | --- |
| M-1 | **An oversized or malformed manifest.** | `facade.readManifest` runs after authorisation. Without `Content-Length` it answers `411` (no chunked body); above `server.maxManifestSize` (4 MiB) `413`. It reads through `io.LimitReader`, and a short body is `400`. It checks that the body is JSON and that its `mediaType` is its `Content-Type`. `encoding/json` bounds nesting. | mitigated |
| M-2 | **A manifest with tens of thousands of layers.** `facade.checkLayers` sent one `HEAD` per layer to the registry before the profile was checked, and ingest checks them again: one PUT of 4 MiB with 25 000 layers made 50 000 requests to the registry. | `facade.readManifest` refuses more than 1 000 layers (`maxLayers`) with `400 MANIFEST_INVALID` naming the bound, on managed and `oci` repos alike, before anything asks the registry. On a managed repo, `facade.putManaged` checks the profile (`profile.Validate`, no backend) before `checkLayers`, so a manifest the profile refuses costs no request either (#247). Test: `TestManifestLayersBound` counts the requests to the registry. What remains: a managed manifest's layers are still asked twice, by `checkLayers` and by ingest, so at most 2 000 `HEAD`s per PUT; asking once needs ingest to report a missing layer as its own error, a change of its contract. The annotations are bounded by `server.maxManifestSize` only. | mitigated |
| M-3 | **Header injection through `OCI-Subject`.** The subject's digest came from the client's JSON unchecked into a response header; only net/http stopped CR/LF. | This pass: `facade.readManifest` refuses a subject whose digest does not parse, `400 MANIFEST_INVALID`. The other headers built from input (`Location`, `Link`, the challenge) take checked names, digests, tags and ids, `url.Values.Encode` and `strconv.Quote`. | fixed in #260 |

### Referrers (#28)

| ID | Threat | Mitigation | Status |
| --- | --- | --- | --- |
| X-1 | **A signature attached to a subject of another repo, or to nothing.** | On a managed repo, `ingest.Pipeline.checkReferrer` resolves the subject in the same OCI name, checks the config and layers with `Exists` there, and refuses any tag but cosign's legacy one. The facade checks `write` and the blobs first (`facade.putReferrer`). | mitigated |
| X-2 | **Two pushes of referrers of one subject lose one**, on a registry without the referrers API, where the provider rewrites the `sha256-<hex>` index. | The referrers lock of the subject in the index ([index-schema.md](../spec/index-schema.md), Coordination); tested across two replicas over Postgres. | mitigated |
| X-3 | **An `oci` repo takes any referrer.** | By design ([http-routing.md](../spec/http-routing.md) §2): no subject check, the registry's. | accepted |
| X-4 | **Reading the referrers of a repo one may not read.** | `facade.referrers` authorises `read` before it parses the digest. A subject without referrers is an empty index, never `404`. | mitigated |

### Frontends and parsers

| ID | Threat | Mitigation | Status |
| --- | --- | --- | --- |
| F-1 | **Path traversal in raw paths:** `..`, `%2e%2e`, `%2F`, NUL, CR/LF, very long paths. | ServeMux decodes the path. `raw.checkPath` refuses empty, `.` and `..` segments, bytes below 0x20 and 0x7f, invalid UTF-8, and more than 4096 bytes. `raw.escapePath` and `profile.EscapeComponent` cannot collide, and the OCI name is capped at 255. ASCII case variants share an OCI repository; `raw.checkSpelling`, and on `/v2` ingest's claim on the package path (F-5), refuse the second spelling. The listing compares a prefix, with no `LIKE`. | mitigated |
| F-2 | **Path traversal in maven paths;** a file that escapes its version; a commit of another version. | `maven.ParsePath` and `groupOf` refuse a group with a `.` segment; `artifactIDPattern` and `tokenPattern` refuse a leading `.`. `maven.ParseFilename` requires the `<artifactId>-<version>` prefix, and the file is the last path element. `deployer.metadata` commits only the document's version, and only the caller's session. | mitigated |
| F-3 | **pypi and apt names into paths.** | pypi: `ValidName`, `ParseWheelFilename`, `ParseSdistFilename`, and `uploadForm.spool` refuses `/` and `\`; downloads are index lookups (`FindFiles`). apt: `checkDistName` for suite and component, the control file's grammars, and pool paths resolved through the index (`poolFile`). | mitigated |
| F-4 | **Archive bombs.** | deb: the stored control size is checked against `MaxControl`; `capReader` caps the unpacked archive at 4 MiB; zstd runs with `WithDecoderMaxWindow`; `data.tar` is never unpacked. An xz control archive has at most 16 blocks (`deb.xzReader.startBlock`), each dictionary at most 64 MiB: a parse allocates at most a GiB, in about 20 ms. POM ≤ 4 MiB, `maven-metadata.xml` ≤ 1 MiB, checksum files ≤ 1 KiB; `encoding/xml` expands no entities. Wheel `METADATA` ≤ 4 MiB. `pypi.checkZipEntries` refuses a wheel or a zip sdist with more than 2^18 central directory headers before `zip.NewReader`, which would hold them (70 MiB). It counts the headers `zip.NewReader` reads, not the count of the end record, which `zip.NewReader` compares modulo 65 536 only. `pypi.readTarGzFile` reads a .tar.gz sdist up to its PKG-INFO for at most 1 GiB unpacked (0.2 s) and 2^18 entries (#243). `npm.ReadTarball` reads an npm tarball whole, as it must to find every `package.json` an install could extract, for at most 1 GiB unpacked and 2^18 entries, its `package.json` ≤ 2 MiB (#304). Tests: `TestXZBlocks`, `TestSdistBombs`, `TestZipBombs`, `TestTarballBombs`, `FuzzZipEntries`, `FuzzPackageJSON`. | mitigated |
| F-5 | **A case variant of a package name squats another spelling's tag.** Names that differ only in case share an OCI repository. The claim was keyed by `lajstrom.name`, and `name_oci` uniqueness was checked after the manifest was pushed: `Com.Example:lib:2.0` landed in `com.example/lib:2.0`, and the real deploy got `409`. | Since #244 the claims are keyed by the package path, and a publish claims its package path for its spelling before its tag (`ingest.Pipeline.claimPath`). Under the claims, a package of another spelling (`ingest.packageOf`), or a fresh claim of another spelling, is `409` naming it, before the manifest is pushed: through the frontends and `/v2` alike, and for two publishes at once. `raw.checkSpelling` and `maven.deployer.checkSpelling` refuse earlier, before a file is stored. Tests: `TestIngestSpelling`, `TestIngestSpellingRace`, `TestManifestHookSpelling`, `TestDeploySpelling`, `TestFirstSpellingWins`. | mitigated |
| F-6 | **Header injection in `Location` and `Content-Disposition`.** | raw uses `r.URL.EscapedPath()`; apt the checked component and `PoolPath`; pypi's `Content-Disposition` is built with `mime.FormatMediaType`. net/http turns CR/LF into spaces on HTTP/1.1 and drops the field on HTTP/2. | mitigated |

### Browsers

| ID | Threat | Mitigation | Status |
| --- | --- | --- | --- |
| B-1 | **Stored XSS.** A raw file uploaded as `text/html` or `image/svg+xml`, or a POM with XHTML (`application/xml`), is served from the origin of `/api/v1`, where a browser holds cached Basic credentials. | This pass: every answer has `X-Content-Type-Options: nosniff` and `Content-Security-Policy: default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'` (`server.securityHeaders`, again after a panic in `writeInternalError`), so such a file runs no script and loads nothing. PyPI simple pages escape what they render (`html.EscapeString`, `url.PathEscape`). Defence in depth not taken: `Content-Disposition: attachment` for active types. | fixed in #259 |
| B-2 | **CSRF.** A cross-site `<form enctype=text/plain>` POSTs a body that `api.decodeBody` reads as JSON (it checks no `Content-Type`); with an admin's cached Basic credentials it creates an admin user. The twine upload is a form too. | This pass: `server.crossOrigin` (net/http's `CrossOriginProtection`) refuses a non-safe request whose `Sec-Fetch-Site` or `Origin` says another site, with `403`, before authentication. Package clients send neither header. | fixed in #259 |
| B-3 | **Framing, referrer leaks.** | `frame-ancestors 'none'`, `Referrer-Policy: no-referrer` | fixed in #259 |
| B-4 | **Version disclosure.** | No `Server` header (net/http writes none; a test reads the wire). `/-/version` answers the build to anyone, as [http-routing.md](../spec/http-routing.md) §5 has it; the edge may keep it inside. | accepted |

### Authentication and audit

| ID | Threat | Mitigation | Status |
| --- | --- | --- | --- |
| A-1 | **Password guessing.** | argon2id ([auth.md](../auth.md)). Per account, `Authenticator.authenticatePassword` and `verifyPassword` allow 10 failures a minute, checked before the cache and argon2id. Per address, with `trustedProxies` or `server.tls` (N-2), they allow 30 failures a minute, checked before the user is read. `/v2/token` takes 100 requests a second per address (`AllowTokenRequest`, checked before Basic). Each throttle and the cache hold at most 10 000 keys. Tests: `TestLoginThrottle`, `TestLoginThrottleAlike`, `TestLoginThrottleParallel`, `TestAddressThrottle`, `TestAllowTokenRequest`, `TestThrottled`, `TestTokenThrottle`, and e2e `auth.sh` (X-04). Tokens are 256 random bits and are not throttled. The budgets are per replica; across replicas the bound is the edge's. | mitigated |
| A-2 | **The throttle's memory bound evicts a throttled account.** Failures of 10 000 other names pushed out its full window, which `throttle.wait` reads without refreshing. | `throttle.makeRoom` gives a new key the place of a window that has closed, else of the open window opened first that is under its budget, and never of a full open window. When all 10 000 are open and full, the new key goes untracked (fail open), which takes 100 000 failed logins a minute on one replica; the choice is in [auth.md](../auth.md#failed-logins), Per replica (#251). Tests: `TestThrottleEviction`, `TestLoginThrottleFlood` (alice stays throttled through a failure of each of 10 000 unknown names). | mitigated |
| A-3 | **A push to an `oci` repo leaves no audit row** (`facade.passThrough`), against rbac.md §5. | `facade.recordPassThrough` writes a `push` row in the transaction that records `oci_repos`: the principal, the repo, the OCI name as the subject, the digest, the client address, the tag and the request id. Every PUT writes one, the repeat of a manifest too (#245). Referrers (`ingest.Pipeline.auditReferrer`) and managed repos are audited by ingest, with the client address (`ingest.withOrigin`, #283). Tests: `TestOCIPushAudit`, `TestPushAuditOrigin`. | mitigated |
| A-4 | **An audit row lost or forged.** | The row is written in the transaction of its change; a write to the registry, which no transaction spans, has its row before the write and completes it after, or the row is `unknown` (#473, [operations.md](../operations.md#audit)). The address is the client behind a trusted proxy (`server.ClientAddr`, 129dc47). The audit API is admin only. | mitigated |
| A-5 | **Basic parsing and timing.** | `r.BasicAuth` splits at the first colon, and `CheckName` forbids `:` and the name `deploy`. The secret's prefix picks the kind of credential. `VerifyPassword` compares in constant time. An unknown, disabled or password-less user costs `dummyVerify`. The verification cache key is a sha256 of name and secret, valid while the stored hash is the same (`keyOf`). | mitigated |
| A-6 | **Claiming the first admin.** Someone reaches a new installation before its operator and makes the first admin, or reads a token from the logs. | There is no admin password in the environment or the configuration, and no token in a log: the bootstrap token is the operator's, from the file of `auth.bootstrap.tokenFile`; the index keeps its sha256 (`auth.Bootstrap.Arm`). `POST /api/v1/bootstrap` compares it in constant time, allows 10 wrong tokens a minute per address, uses the token in the transaction that makes the admin, and is `404` once an enabled admin exists (`auth.Bootstrap.Exchange`); the token expires after `auth.bootstrap.ttl`. Tests: `TestBootstrapTokenFile`, `TestBootstrapReplicas`, `TestBootstrapRefusals`, `TestBootstrapExpiry`, the conformance `BootstrapTokenRace`, and e2e `first-admin.sh`. | mitigated |

### The client address ([ADR 0011](../adr/0011-client-address-behind-a-proxy.md))

| ID | Threat | Mitigation | Status |
| --- | --- | --- | --- |
| N-1 | **A spoofed `X-Forwarded-For`.** | `server.resolveClient` reads the header only when the peer is in `server.trustedProxies`. It walks every `X-Forwarded-For` line right to left, skipping trusted hops; a malformed hop makes the peer the client. IPv6 counts as its /64 (`throttleAddress`). `X-Forwarded-Proto` and `X-Forwarded-Host` are read nowhere. The access log has the client and the edge (`proxy`). | mitigated |
| N-2 | **Per-address throttles off when Lajstrom terminates TLS itself** (`server.tls`, so the peer is the client) and `trustedProxies` is empty. | `server.New` sets `byAddress` with `server.tls` too, and `server.clientAddress` then puts the peer into the throttles (`auth.WithClientAddress`); the startup line says so (`logTrust`). Tests: `TestAddressThrottleTLS`, `TestTokenThrottle`, `TestClientAddress`. An L4 balancer that hides the client in front of `server.tls` makes one bucket of everyone (ADR 0011, amendment). | mitigated (#248) |
| N-3 | **The realm over `http://` with `server.tls` and no `externalURL`:** docker sends Basic in clear text. | `server.externalURL` is required outside the dev stack (T-2); without it, `token.Realm` answers `https://` for a request that came over TLS. Test: `TestRealm`. | mitigated (#248) |

### Secrets in logs

| ID | Threat | Mitigation | Status |
| --- | --- | --- | --- |
| L-1 | **The access log.** | `server.accessLog` writes the method, path, status, principal and addresses: never the query, a header or a body. | mitigated |
| L-2 | **Credentials and tokens in values that are logged.** | `token.Token`, `keys.TokenKey` and `config.RegistryAuth` print redacted. `Verifier.Verify` logs the `kid` and the reason, never the token. The throttle logs the account's uid, never the typed name. The provider logs paths only (`logTransport`); `withoutURL` strips URLs from errors, and the query of error responses is cleared. `postgres.Open` leaves the DSN out of its errors. `recovery` logs the panic value, the path and the stack. | mitigated |
| L-3 | **A typed user name that may be a mistyped password**, logged at debug for an unknown user (`Authenticator.refuse`) and at error when the index fails (`Server.authenticate`). The `kid`, which an attacker controls, is logged at debug without a length bound. | Debug is off by default. | open, low; recorded here, no issue |
| L-4 | **The master key and the configuration.** | The key is read from `keys.masterKeyEnv` or `keys.masterKeyFile` and never logged; the startup log names the variable, not its value. | mitigated |

### SSRF through configuration

| ID | Threat | Mitigation | Status |
| --- | --- | --- | --- |
| S-1 | **A URL that a non-admin controls.** | None exists. The API names registries by name only (`Repos.checkRegistry`); `PATCH` refuses `registry`; the prefix is fixed; there are no proxy repos, so no upstream URL. `registries[].url` is the operator's. The backend's own redirects and realm: P-2, P-3. | mitigated |

## 4. What the edge must do

Lajstrom assumes an edge in front of it. The edge terminates TLS, routes the whole host, keeps
clients away from the registry and the metrics listener, and limits request rates where a count
is cheap to send and dear to answer. [reverse-proxy.md](../reverse-proxy.md) has the
configuration, part 1 and part 2. In terms of this document:

- **TLS and `Strict-Transport-Security`** at the edge, and `server.externalURL` set to the public
  URL, which the configuration requires: T-2, N-3.
- **`X-Forwarded-For` from the edge only**, and `server.trustedProxies` naming exactly the peers
  Lajstrom sees: N-1. With it empty, the per-address throttles are off (A-1), unless Lajstrom
  terminates TLS itself (`server.tls`, N-2).
- **A rate limit on `/v2/token`** per address, and per-connection bandwidth if needed: A-1, T-7.
  Not a request count on the rest: a `docker pull` is tens of requests a second.
- **Never route `server.metricsListen`**: `/metrics` and `/debug/pprof/` stay inside.
- **Keep `/-/version`** inside if the build should not be public: B-4.
- **The registry unreachable from clients**: P-8.

## 5. Open items

| Issue | Threats | Priority |
| --- | --- | --- |
| [#60](https://github.com/lajstrom/lajstrom/issues/60) typed, validated repo policy | U-7 | (its own) |

None is critical: they need a writer,
an operator's mistake or a hostile registry. Two findings would let a writer's upload, or any
web page, act with an admin's browser credentials. Both are fixed in this pass: stored XSS
(B-1) and CSRF (B-2).

## 6. What this pass did not do

- **No fuzzing.** #51 adds fuzz targets for the parsers; F-4 was found by reading and by
  measuring.
- **The dependencies.** These were not reviewed beyond `govulncheck`, which runs in
  `make check`. At `129dc47` it found no vulnerability that the code calls, and one in a
  required module that the code does not call.
- **ECR, GHCR, Harbor and the other registries of the matrix.** Their behaviour for P-2, P-3
  and P-5 was not probed.
- **The `ha` stack (#226).** Replicas were reasoned about: the throttles and caches per replica,
  sessions and claims in the index. They were not attacked.
- **Timing side channels** beyond R-5.
