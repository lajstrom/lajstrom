# 0026. Token exchange for CI: a CI platform's OIDC token for a short-lived Lajstrom token

- Status: proposed
- Date: 2026-11-16
- Deciders: gecube

## Context

A CI job that publishes to Lajstrom holds a deploy token or a personal access token in a secret
of the CI system (docs/auth.md, In CI): a long-lived secret, copied into every project that
publishes, which a leaked log or a malicious workflow change can read. Both CI platforms that
the users of Lajstrom run already prove who a job is:

- **GitHub Actions** issues a job with `permissions: id-token: write` an OIDC token on request
  (`ACTIONS_ID_TOKEN_REQUEST_URL` and `ACTIONS_ID_TOKEN_REQUEST_TOKEN`, with `&audience=`), signed
  by `https://token.actions.githubusercontent.com` (`https://<host>/_services/token` on GitHub
  Enterprise Server). Its claims name the repository (`repository`, `repository_id`,
  `repository_owner`, `repository_owner_id`), the ref (`ref`, `ref_type`, `ref_protected`), the
  `environment` of the job, the event (`event_name`), the workflow (`workflow_ref`,
  `job_workflow_ref`), the run (`run_id`, `run_attempt`, `sha`) and who started it (`actor`,
  `actor_id`). A workflow of a pull request from a fork gets no token; `pull_request_target`
  runs the base repository's workflow with the base's identity and its `ref`, and may check out
  the fork's code.
- **GitLab CI** puts the tokens a job declares in `id_tokens` (each with its `aud`) into
  variables, signed by the GitLab instance (`https://gitlab.com`). Its claims name the project
  (`project_id`, `project_path`, `namespace_id`, `namespace_path`), the ref (`ref`, the short
  name, `ref_type`, `ref_path`, `ref_protected`), the `environment` and whether it is protected,
  the pipeline (`pipeline_source`, `pipeline_id`, `job_id`, `sha`, `ci_config_ref_uri`) and the
  user (`user_id`, `user_login`, `user_email`).

PyPI's trusted publishers and npm's trusted publishing take these tokens: the registry keeps,
per project, the repository, the workflow and the environment that may publish, and trades a
token that matches for an upload token of minutes (PyPI's `POST /_/oidc/mint-token`, JSON;
npm's `POST /-/npm/v1/oidc/token/exchange/package/<name>`). Neither stores a secret in CI.

What Lajstrom has: the device flow's OAuth token endpoint `POST /-/oidc/token` (rbac.md §8,
RFC 8628), served while `auth.oidc` is on; `oidc.Verifier` (#1223), which verifies the JWTs of an
issuer for an audience by its discovery document and a cached JWKS, for the Kubernetes
service-account tokens of rbac.md §9; the bearer tokens of `/v2`, signed with the token key
(`keys`, ADR 0010) and verified for 20 minutes, a retired key for 21 (`keys.TokenGrace`); deploy
tokens over a mask (#911), whose rights are on the credential rather than in bindings; and the
principals `cert:` and `k8s:` of ADR 0025, which exist through their credential and have no row.

#937, split out of #69 (decision D6), asks for the exchange and leaves four points open: the
endpoint, where the trust policies live, the token's lifetime and form, and which claims a policy
matches and how a wildcard is written. D6 sketched `POST /-/oidc/token` with RFC 8693, trust
under `auth.oidc.trust[]`, exact claims, a token of at most an hour acting as the groups of the
matching rules, and nothing written per run but the audit row. The maintainer decided on
2026-11-16 to do it now, with defaults for the four points to be proposed in the contract.

## Considered options

### 1. The endpoint

- **A. RFC 8693 token exchange at `POST /-/oidc/token`**, the token endpoint the device flow has
  already: `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`, `subject_token`,
  `subject_token_type`, a form, OAuth's errors. One token endpoint for every grant, as OAuth has
  it; a one-line `curl` in a job; and the clients of OAuth libraries that know RFC 8693.
- **B. `POST /api/v1/tokens/exchange`**, JSON in the envelope of the management API, like PyPI's
  `mint-token`. The management API's conventions, and an unauthenticated mint among routes that
  all take a credential.
- **C. No exchange: the CI token itself as the password**, of `/v2/token` and of Basic on every
  route, as a Kubernetes token is at `/v2/token` (ADR 0025). No endpoint; but every request
  verifies a foreign JWT and matches the policies again, the platform's token (minutes, not
  renewable on GitLab within a job) is the credential written into `~/.docker/config.json` and
  `.pypirc`, and D1 of #69 refused the IdP's token as a credential for people for the same
  reasons.
- **D. The registries' own routes**: PyPI's `/_/oidc/audience` and `/_/oidc/mint-token` at the
  root of the host, npm's exchange under `/npm/<repo>/-/npm/v1/oidc/…`, so that
  `pypa/gh-action-pypi-publish` and `npm publish` exchange by themselves. Two shapes per
  format, each tied to how one client finds its registry; they can sit on the same verification
  and policies later.

Every client that publishes takes a password or a token as it is today: `docker login`, `oras
login`, `helm registry login` (`--password-stdin`), twine (`TWINE_USERNAME=__token__`,
`TWINE_PASSWORD`), Maven's `settings.xml`, npm's `_authToken`, curl. With A, B or D a job runs one
step before them that turns the platform's token into Lajstrom's.

### 2. Where trust policies live

- **A. The configuration only**, `auth.ci`: declarative, reviewed in git with the deployment, a
  rolling restart to change. Lajstrom writes no audit row for a policy change: the history of the
  configuration is its record, and `serve` logs each policy at start. A stolen admin credential
  cannot add one.
- **B. The API too**, a table with its audit rows. An admin's stolen token, or a mistake in the
  UI, can add a policy that trusts an attacker's own repository: a credential without a secret,
  which outlives every password and token revoked afterwards, and which the audit log shows only
  if someone reads it.
- **C. A declarable kind of ADR 0015**, in a source watched from git. Reviewed in git without a
  restart, and the engine's audit rows; it waits for the kinds of ADR 0015 beyond `Repo` and
  `RoleBinding`, and can come later without changing the policy's schema.

### 3. The token

**Its form.**

- **A. A JWT signed with the token key, with the prefix `ljs_ci_`**, minted for every exchange
  and stored nowhere: the prefix tells it to the authenticator and to a secret scanner, as
  `ljs_pat_` and `ljs_dep_` do; a key rotation with `--drop` ends it with every bearer.
- **B. A deploy token with an expiry**, a row per exchange (migration 0033). Revocable one by
  one, but a write per CI run, and rows that expire to sweep.

**Its lifetime.** Minutes: 15 by default, configurable from 1 to 20. Twenty is the bearer's
lifetime, so that a rotated token key (`keys.TokenGrace`, 21 minutes) never cuts one short; D6's
hour would need a longer grace for every bearer. A job that runs longer exchanges again.

**What it works on.** Wherever a deploy token works: the password of Basic on every route, user
name free (`__token__`, `ci`); `Authorization: Bearer` outside `/v2` (npm's `_authToken`); the
password of `/v2/token` (`GET` and the `POST` form), whose bearer then names the `ci:` principal.
On `/api/v1` it reaches what its rights reach, a repo's routes, and never `system` nor `audit`.
It creates no token of any kind: a CI job must not turn minutes into a secret that lasts.

**Its principal.**

- **A. `ci:<issuer>/<project>`**, `<issuer>` the name the configuration gives the issuer and
  `<project>` GitHub's `repository` (`acme/app`) or GitLab's `project_path`
  (`acme/platform/app`): readable in the access log, the audit log and `lajstrom.uploaded-by`;
  per project for the bounds per principal. The ref, the environment, the run and the policies
  go into the audit row of the exchange.
- **B. `ci:<issuer>/<policy>`**: names what granted, not who ran; two projects under one policy
  share a principal.
- **C. The numeric id in the principal** (`ci:github/123456`): stable across a rename, and
  unreadable everywhere a principal is shown. The ids are in the audit row.

**Its rights, and bindings.**

- **A. The policy grants them** (`grants: [{role, mask}]`), and a `ci:` principal has no
  bindings: the conditions and the rights are read together, in one reviewed file. The RBAC
  evaluates a CI token as a deploy token over a mask, from the credential.
- **B. The policy names groups, and `group:` bindings decide** (D6's sketch). The bindings and
  their API are reused, but a group's name is shared with the IdP's groups: an IdP group of the
  same name gets the CI's rights, and the CI the group's. The trust decision is split between the
  file and the database.
- **C. The `ci:` principal is bound directly** (`ci:github/acme/*`), the policy only admits.
  A read policy for every branch and a write policy for `main` then yield the same principal with
  the same bindings: the ref of a policy no longer limits the rights, unless the policy caps them
  too, in a second place that must agree with the first.

### 4. Claims and wildcards

- **Which claims.** A list per platform, of the claims that say who and what runs; `sub` is not
  one, since its form is configurable per repository on GitHub. An unknown key refuses the
  configuration rather than never matching.
- **Wildcards.** **A. `*` for any run of characters but `/`, `**` for any run, in the claims of a
  ref, an environment and a workflow only**, GitHub's filter syntax that users write in `on:`
  already; elsewhere exact. B. Regular expressions: one unanchored pattern grants every
  repository. C. Exact values only (D6): every release tag a policy of its own.
- **Safe defaults.** Without them, a policy that names a repository and a role publishes from
  every branch and every pull request. Each is either a refusal at start or a warning; a warning
  at every start becomes noise, and a refusal is what a reviewer sees in the pull request of the
  configuration.

## Decision

Option A for each question: **RFC 8693 token exchange at `POST /-/oidc/token`, for the OIDC
tokens of the issuers of `auth.ci`, under trust policies in the configuration only, which grant a
role on a mask; the answer is a `ljs_ci_` token of 15 minutes for the principal
`ci:<issuer>/<project>`, which has no bindings and creates no token.**

- **Configuration.** `auth.ci.issuers`: a `name` (the principal's `<issuer>`), a `platform`
  (`github` or `gitlab`, which decides the claims and the project), the `issuer` (exact, as its
  tokens' `iss` and its discovery's `issuer`), the `audience` (default `server.externalURL`) and a
  `caFile`. `auth.ci.policies`: a `name`, an `issuer`, `claims` (a value or a list of values per
  claim, all of which must hold) and `grants` (`role` `reader`, `writer` or `maintainer`, never
  `admin`; a `mask` of rbac.md §3). `auth.ci.tokenLifetime`, 15m. Read at start.
- **The claims a policy may match.**
  - GitHub: `repository_id`, `repository_owner_id`, `actor_id` (ids); `repository`,
    `repository_owner`, `actor` (names); `ref`, `environment`, `workflow_ref`,
    `job_workflow_ref` (patterns); `ref_type`, `ref_protected`, `event_name`,
    `runner_environment`, `repository_visibility` (values).
  - GitLab: `project_id`, `namespace_id`, `user_id` (ids); `project_path`, `namespace_path`,
    `user_login` (names); `ref`, `ref_path`, `environment`, `ci_config_ref_uri` (patterns);
    `ref_type`, `ref_protected`, `environment_protected`, `pipeline_source`,
    `runner_environment`, `deployment_tier`, `project_visibility` (values).
  A claim is compared as a string (a number or a boolean as its JSON text); a claim the token
  lacks fails the condition.
- **Wildcards** in patterns only: `*` any run of characters but `/`, `**` any run; no other
  character is special. A value made only of `*` is refused: it is no condition.
- **Safe defaults, refused at start:**
  1. a policy names its project by a numeric id: `repository_id` or `repository_owner_id` on
     GitHub, `project_id` or `namespace_id` on GitLab. A name alone is refused, since a deleted
     owner's or project's name can be registered again by anyone; a name beside an id is checked
     too;
  2. a policy that grants `writer` or `maintainer` has a condition on `ref` (or `ref_path`) or on
     `environment`; one that grants `reader` only need not;
  3. a policy that grants `writer` or `maintainer` never matches a token of a pull request: on
     GitHub an `event_name` of `pull_request`, `pull_request_target`, `pull_request_review` or
     `pull_request_review_comment`, on GitLab a `pipeline_source` of `merge_request_event` or
     `external_pull_request_event`, whatever its conditions say; one whose `event_name` or
     `pipeline_source` condition names only such events is refused.
- **The exchange.** The form of RFC 8693 §2.1, no client authentication; `subject_token_type`
  `…:token-type:jwt` or `…:token-type:id_token`; `audience` and `resource` not read; `scope` is
  `invalid_scope` (no narrowing in the first cut); `actor_token` is `invalid_request`. The token
  is verified as a Kubernetes token is (rbac.md §9: the issuer's discovery and cached JWKS, an
  asymmetric algorithm, `iss`, `aud`, `exp`, `iat`, `nbf`); every policy of its issuer is
  matched, and the token's grants are the union of those that match. None: `400 invalid_grant`,
  with the token's own claims in the description and nothing of the policies. The answer: RFC
  8693 §2.2.1 with `access_token`, `issued_token_type` `…:token-type:access_token`, `token_type`
  `Bearer`, `expires_in`, and the `principal` and `policies`.
- **The token.** `ljs_ci_` and a JWT signed with the token key: `typ` `ljs-ci+jwt`, `iss` the
  service, `aud` `lajstrom:ci` (never a service, so never a bearer of `/v2`), `sub` the principal,
  `jti`, `iat`, `nbf`, `exp`, and the names of the policies that matched. Each use grants what
  those policies grant in the configuration of the replica that serves it: a policy removed is
  dead there from the restart on; a token none of whose policies the replica has is `401`.
- **The RBAC.** A step beside the deploy token's: a CI token is allowed an action on a repo iff a
  grant of its policies has a mask that matches the repo and a role with the action; never
  `system` nor `audit`; the bindings and anonymous read are not asked. A repo no grant matches is
  `404`, one a grant matches without the action `403`, as for a deploy token over a mask. A
  binding of a `ci:` principal is `400`. Creating a personal access token, a deploy token of a
  repo or one over a mask with a CI token is `403` before the body is read.
- **Audit.** Each exchange is a `token.exchange` row in its own transaction: the principal, the
  token's id (`jti`), the issuer, the policies, its expiry, and the claims of the platform's
  list beside the run's (`run_id`, `run_attempt`, `sha`, `pipeline_id`, `job_id`), never the
  token nor `user_email`. The rows that ingest and the facade write for a CI token (pushes,
  deletes, a proxy's fetches) are rows of its principal with `details.ciToken`, the exchange's
  `jti`, through the bearer of `/v2/token` too, which carries it. A refused exchange writes no row: anyone can get a valid token from a repository of their
  own; it is logged at info with its reason, the issuer and the project when the token verified.
- **Out of scope:** narrowing by `scope`; revoking one CI token; bindings and patterns for
  `ci:`; policies through the API or a source; issuers of other platforms (Buildkite, CircleCI,
  Bitbucket, Jenkins); PyPI's and npm's own exchange routes (option 1D); a single use per OIDC
  token; the `admin` role.

## Consequences

- A project publishes with no Lajstrom secret in CI: a leaked log holds a token of at most 20
  minutes, and a workflow changed on a branch or in a pull request publishes nothing where the
  policy pins a ref or an environment.
- Each job runs one step before its client: a `curl` that trades the platform's token. The
  clients themselves are unchanged. pypa's publishing action and `npm publish --provenance` do not
  find Lajstrom by themselves until option 1D.
- Policies change with the configuration and a restart. The audit log holds the exchanges and
  the writes; the changes of policies are in the history of the configuration, and `lajstrom
  admin binding list` does not show the rights of CI, which the configuration and the start log
  do.
- An operator must look up the numeric id of a repository (`gh api repos/<owner>/<repo> --jq
  .id`) or a project (its page): one step more than PyPI's form, which looks it up itself.
- No CI token can be revoked alone. The levers: the policy removed, at the next restart; `lajstrom
  admin key rotate --token --drop`, which ends every bearer too; and its 20 minutes at most. A
  bearer that `/v2/token` issued on a CI token lives its own 20 minutes, as any.
- One principal kind more, `ci:`, read wherever principals are read: a replica of the release
  before refuses a bearer whose `sub` it does not parse (`401`), and does not know `ljs_ci_` (a
  password that fails). The feature works once every replica has it.
- Without a master key the token key is per replica (`keys.EphemeralTokens`), so a CI token minted
  on one replica fails on another, as a bearer does.
- A pull request's job never writes through a CI token, a preview build of a pull request
  included; it uses a deploy token in a secret that forks do not see, or a policy that reads.

## Amendment (maintainer, 2026-11-16)

The maintainer decided on the defaults (comment on #937): trust policies live in the
configuration **and** the API; rights come from the policy's `grants` and a `ci:` binding stays
refused; a refused exchange writes an audit row, rate-limited per source; the rest as proposed
(RFC 8693 at `POST /-/oidc/token`, a token of 15 minutes, numeric project ids, a ref or an
environment to write, pull and merge requests never write). This replaces option 2A by 2B under
the limits below, and the refusals' "no row".

- **Policies of the API.** An admin creates, replaces and deletes them under
  `/api/v1/ci/policies` and with `lajstrom admin ci-policy`, a resource of `system`; each change
  is a `ci.policy.create`, `ci.policy.update` or `ci.policy.delete` row with the policy's issuer,
  claims and grants (and the previous ones for a change). They are rows of a table of the control
  plane, `ci_policies` (migration 0033), and go into a snapshot. The rules are the
  configuration's, checked by the same code, with `400` naming the field. The policies of the
  configuration are shown through the API as `source: config` and stay read-only there (`409`),
  one owner per object as ADR 0015 has it.
- **Without a restart.** Every change moves `auth_version` in its transaction, as a binding does,
  and a replica reads the policies of the index again when the counter moved, before each
  exchange and each request with a CI token. Chosen over a cache with a lifetime: no window in
  which a deleted policy still grants, and the read of the counter is the one RBAC makes already.
- **Policies of the API read.** A policy of the API grants `reader` only (`403` for one that
  writes), and one that writes in the index (from a snapshot) is left out of the evaluation, with a
  warning. A stolen admin credential then adds a policy that reads at most: it can leak internal
  artifacts, never publish one. The policies that write stay in the configuration, reviewed in
  git (the second amendment, below, lets the API make them under conditions).
- **Refusals.** A `400 invalid_grant` is a `token.exchange` row with `outcome: refused`, its
  reason, the issuer, and for a token that verified its principal and the claims that say who and
  where (ids, project, ref, environment, event); never the token, never a claim of a token that
  did not verify. Rate-limited per replica by `<issuer>/<project id>` for a token that verified and
  by the client address otherwise: one row a minute per key, with `suppressed`, the refusals of
  the key left without a row since the one before; at most 10 000 keys. A project that misses its
  policy writes a row a minute, not one per retry of its job, and a flood from one address one row
  a minute; the info log keeps every refusal.

Consequences of the amendment: a policy can be added at night by whoever holds an admin's
credential, visible in the audit log only; the policies of a server are its configuration's and
its index's, so `lajstrom admin ci-policy list` (or the API) shows them both, and a restore from a
snapshot brings the API's back. A replica of the release before the migration does not read the
table: its CI tokens get the configuration's grants only until it is replaced.

## Second amendment (maintainer, 2026-11-16)

The maintainer decided on the policies of the API that write (comments on #937): an admin makes
them through the API too, and a policy created or widened through the API that grants `writer` or
`maintainer` needs

1. a fresh interactive sign-in with a second factor: a web UI's sign-in through OIDC with
   `prompt=login` and `max_age`, the ID token's `auth_time` within 10 minutes and its `amr`
   showing a second factor. A password's sign-in does not count, nor a personal access token, a
   deploy token, a service account's token or a CI token;
2. the approval of a second, different admin: the policy waits, pending, until then. The approval
   is a setting, `auth.ci.apiWriteApproval: second-admin | none`, `second-admin` by default; an
   install with one admin sets `none`, or declares its policies that write in the configuration,
   where the review in git is the second pair of eyes;
3. a target within `auth.ci.apiWriteRepos`, masks of the configuration: write to other repos only
   through the configuration;
4. an expiry of at most 180 days, renewed by the same fresh sign-in and approval;
5. an event on the webhooks for every create, approval, change, renewal, expiry and delete.

With `none`, steps 1, 3, 4 and 5 still hold. A policy that reads only is made through the API
without them. This replaces "Policies of the API read" of the first amendment.

How it is built (rbac.md §10, §11):

- **The sign-in.** The callback of a web UI's login keeps `auth_time` and `amr` of the ID token
  with the login request, and the token of its code keeps them (`tokens.auth_time`, `tokens.amr`,
  migration 0033), so that a request with the token and a proof of its DPoP key carries when and
  how the person authenticated. `/-/oidc/login` takes `prompt=login` from a web UI and asks the IdP
  for `prompt=login` and `max_age=600`. `amr` shows a second factor with `mfa`, or with methods of
  two kinds of factor of RFC 8176 (knowledge, possession, inherence). Chosen over a separate
  credential for the step-up: the token that a web UI already holds, bound to its key, carries it,
  and nothing else changes for the token. The command line, which writes the database without a
  credential, makes the policies that read and deletes any.
- **Pending, approval and renewal.** `ci_policies` has a `state` (`active`, `pending`,
  `expired`), `expires_at`, the expiry of a renewal that waits, the principal of the latest change
  (`updated_by`, who may not approve it) and of the latest approval, and a `revision`. An approval
  takes `If-Match`, the ETag of the revision read, so that what is approved is what was read; a
  change of a policy that writes waits again, whatever it changed: telling a narrowing from a
  widening of patterns is not attempted. A renewal of a policy that counts waits beside it, which
  keeps its expiry until the approval, so that renewing early stops nothing.
- **The expiry.** A policy grants nothing from its `expiresAt` on, whether marked or not: each
  replica's policies are read again when the earliest expiry among them passes. Each replica also
  looks once a minute for the policies whose expiry passed and marks each `expired` with its
  `ci.policy.expire` row and event, in a transaction that updates the row only at the revision it
  read: of the replicas that look at once, one marks it, and the event is sent once. Chosen over
  marking at the first read of the policy, which writes nothing until someone exchanges a token or
  lists the policies, so that the event of an expiry could come days late; one query a minute per
  replica is the cost.
- **Events.** The six types `ci.policy.created`, `.updated`, `.renewed`, `.approved`, `.expired`
  and `.deleted` (webhooks.md §1), for every policy of the API, one that reads too, to the
  webhooks of the admin (a mask with `*`) that cover a mask of the policy's grants: a webhook of
  one repo belongs to its maintainer, who may not read the policies.

Consequences of the second amendment: making a policy that publishes through the API takes a web
UI that signs in through OIDC with a second factor, and with the default, two admins; an install
without them declares its policies that write in the configuration as before. A policy that
writes stops at its expiry unless renewed, which breaks the CI jobs that rely on it: the
`ci.policy.expired` event, and the expiry in the lists, warn first. The state, the expiry and the
sign-in come with the table, in migration 0033, so no release reads the table without them.

