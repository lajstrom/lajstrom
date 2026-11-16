# 0022. Web UI sign-in: a short-lived DPoP-bound token from `POST /api/v1/login`, an OIDC code redeemed with PKCE

- Status: proposed; the binding to a key (DPoP) decided by the maintainer on 2026-11-16, its
  details (8) proposed
- Date: 2026-11-15, amended 2026-11-16
- Deciders: gecube

## Context

The web UI ([lajstrom-ui](https://github.com/lajstrom/lajstrom-ui), #70) is a static bundle that
reads `/api/v1` with a bearer and never with cookies (#136). It runs on Lajstrom's origin, at
`/ui/` behind the same edge, or on another origin that `server.corsOrigins` lists (#949).

What it has to sign in with today ([http-routing.md](../spec/http-routing.md) §4):

- **A password.** The UI creates a personal access token with `POST /api/v1/users/<name>/tokens`,
  the password over Basic, `expiresIn: "12h"`, and revokes it at sign-out with
  `DELETE …/tokens/<id>`. The UI chooses the token's name and lifetime; the server has no say in
  how long a browser stays signed in.
- **A pasted personal access token**, which sign-out only forgets.
- **OIDC** (#69, [rbac.md](../spec/rbac.md) §8), which the UI cannot use: the browser login ends
  on Lajstrom's page that shows a new personal access token once, and nothing hands the token to
  the UI (lajstrom-ui#11).

#70 asks for "short-lived login tokens from `/api/v1/login` rather than pasted PATs", for both.
The open questions are what the login answers and for how long, refresh, sign-out, throttles and
audit, how an OIDC login hands its token to a UI on another origin, what `GET /api/v1/info`
tells the UI, and cookies. Each has a recommended default below.

## Considered options

### 1. What `POST /api/v1/login` answers

- **A. A personal access token of the user** (`tokens.kind` `pat`), without scopes, named
  `login <time> <4 hex>`, expiring after `auth.login.tokenLifetime`, answered as
  `POST …/tokens` answers a creation: `201`, `Location`, the `CreatedToken` (`id`, `user`,
  `expiresAt`, `secret`, …). Everything that holds for a personal access token holds: the token
  list, revocation, expiry, a disabled or deleted user, the rule that a token never creates a
  token (#249). The UI keeps exactly what it keeps today (its token, user, token id and expiry).
- **B. A token kind of its own** (`session`) that works on `/api/` only. A token stolen from the
  browser could not push through `/v2` or a frontend. But a maintainer's stolen session could
  still create a deploy token of the repo and push with that, a new kind touches `tokens.kind`,
  the authenticator and the token list, which #1020 changes now, and the browser's token is
  bounded by its expiry and revocation already (threat model B-5). It can come later without
  changing the shape of the answer.

`auth.login.tokenLifetime`: 12h by default, as the UI asks today, and at most 24h: a token
for longer is a personal access token the person makes and pastes.

### 2. Refresh

- **A. None.** The UI signs in again when the token expires or a request is `401`.
- **B. A refresh token.** A second secret in the same storage, living longer than the first, so
  a script on the UI's origin (B-5) gains more, not less.

### 3. Sign-out

- **A. `DELETE /api/v1/users/<name>/tokens/<id>` with the token itself**, as the UI does today:
  a personal access token without scopes revokes its user's tokens (rbac.md §3), and the login
  answers `id` and `user`. No new route.
- **B. `POST /api/v1/logout`**, which revokes the request's own token: a second route for the
  same change.

### 4. Throttles and audit

- **A. As npm's login, the other login that turns a password into a token**: the password is
  checked as Basic is, with the throttles of failed logins per account and per address (`429`
  with `Retry-After`) and argon2id every time; a refusal is `401` and says no more. The audit
  row is the token's `token.create`, `createdBy` `password`, source `api`. An OIDC login through
  the UI writes what the browser login writes: `user.login` and `token.create` with `createdBy`
  `oidc`.
- **B. A `user.login` row for a password login too.** One more row per sign-in that says what
  `token.create` says already; `user.login` carries an OIDC login's identity and groups.

The request's own `Authorization` header is ignored on `POST /api/v1/login`, as on npm's login
and `/-/oidc/`: a UI whose stored token was revoked signs in without being refused first.

### 5. How an OIDC login hands its token to a UI on another origin

- **A. A redirect back to the UI with the token in the URL fragment.** One request less. But the
  bearer itself sits in the address bar and the history until the UI removes it, and any page
  that the redirect reaches holds it.
- **B. A code the UI redeems with PKCE (RFC 7636).** The UI makes a verifier, keeps it in its
  tab, and starts `GET /-/oidc/login?return_to=<a page of the UI>&code_challenge=<S256 of the
  verifier>&code_challenge_method=S256`. The login runs as today; the callback, instead of the
  token page, keeps the sha256 of a new code of 256 random bits on the login request with the
  user, and answers `302` to `<return_to>#code=<code>`. The UI posts `{"code", "codeVerifier"}`
  to `POST /api/v1/login` and gets the answer of a password login. The code is useless alone:
  redeemed once, within the login's 10 minutes, with the verifier only. The fragment reaches no
  server and no `Referer`, the token is never in a URL, and the redemption is a request to
  `/api/v1` that the CORS of #949 serves. `return_to` must be on the origin of
  `server.externalURL` or of `server.corsOrigins`, so the callback is no open redirect.
- **C. The UI runs its own authorization code flow** at the IdP, as a public client, and trades
  the IdP's token at Lajstrom. A second client at the IdP, CORS at the IdP, and an IdP token as a
  credential of Lajstrom, which rbac.md §8 rules out.

A refusal at the callback stays Lajstrom's page, as today: nothing of the IdP's answer goes to
another origin, and the UI offers the sign-in again when the person comes back.

### 6. What `GET /api/v1/info` tells the UI

- **A. `authModes` as specified** (`password`, `token`, and `oidc` while `auth.oidc` is on; main
  does not add `oidc` yet), **and `login`**: `{"tokenLifetime": <seconds>}` on a server that has
  `POST /api/v1/login`. An older server answers no `login`, and the UI keeps creating its token
  with Basic. With `oidc` in `authModes`, `login` also says that `/-/oidc/login` takes
  `return_to` and `code_challenge`, which the OIDC login of #69 alone does not.
- **B. A list of login URLs.** The UI knows the server's base (`apiBaseUrl`) and the paths are
  fixed.

### 7. Cookies

Browsers never get a cookie for `/api/v1`: the login answers its token in the body, and the one
cookie Lajstrom sets, `lajstrom_oidc`, has the path `/-/oidc/` and authenticates nothing.

### 8. The token bound to a key of the browser (DPoP, RFC 9449)

The maintainer decided on 2026-11-16 (#1170, #949) that the token of a UI's sign-in is
DPoP-bound, always: `POST /api/v1/login` takes the browser's proof, the token records the key's
thumbprint, and `/api/v1` takes the token only with a valid proof of that key, the protection
against a replayed proof shared by the replicas. So a token copied off the browser is of no use,
and of none to docker, pip or npm. What that leaves open is below, the recommended option first.

**8.1 Where the binding is kept.**

- **A. A column, `tokens.dpop_jkt`**: the JWK SHA-256 thumbprint (RFC 7638) of the key, the
  `cnf.jkt` of RFC 9449 §6.1. The token stays a personal access token (`kind` `pat`), and all of
  1A holds: the token list, revocation, a disabled user, no token created with it. Its views
  answer `dpopJkt`, so the token list shows which tokens are bound (#959). The authenticator
  refuses a token with `dpop_jkt` everywhere but `Authorization: DPoP` on `/api/`. A replica of a
  release before 0028 knows no such column and takes a bound token for a bearer: a web UI signs
  in once every replica runs this release, and after a rollback to a release before, the bound
  tokens still live work as bearers until they expire or are revoked, 24 hours at most.
- **B. A token kind of its own** (`dpop`). A replica of a release before refuses it ("a token of
  kind dpop"), so a rollout or a rollback fails closed. But every list and check that asks for
  `pat` learns a kind: the token list, revocation, `MayCreatePAT`, the service accounts of #1020,
  #959.
- **C. A secret prefix of its own** (`ljs_pop_…`), which a replica before refuses by its shape,
  failing closed too. A third prefix in the rule on passwords (a password that starts with it
  stops working), in the redaction of logs and in the secret scanners.

**8.2 Signature algorithms.**

- **A. ES256 only**, `algs="ES256"`: an ECDSA P-256 key, which every browser makes
  non-extractable with WebCrypto; one curve to check.
- **B. Also EdDSA, ES384, PS256**: more keys to check for no client that needs them. Additive
  later.

**8.3 `htu`.** `server.externalURL` and the request's path, both normalized as RFC 3986 §6.2.2
and §6.2.3 say (RFC 9449 §4.3). Behind the edge Lajstrom sees neither the browser's scheme nor
its host, and `server.externalURL` is the URL the browser sees; `X-Forwarded-Host` and
`X-Forwarded-Proto` are read nowhere (threat model N-1). No option: the rest would trust the
client.

**8.4 How far a proof's `iat` may be from the server's clock.**

- **A. 5 minutes, before or after**, as Kerberos takes clock skew: the clocks are the people's
  laptops, and without a nonce (8.6) the server corrects none of them. A UI whose `iat` is
  refused corrects its own by the `Date` of an answer, which CORS exposes (http-routing.md §4).
- **B. 60 seconds**: a laptop off by more cannot sign in at all.
- **C. A configuration key**: one more key for a number nobody has a reason to change yet.

**8.5 A proof used once (RFC 9449 §11.1).**

- **A. A table, `dpop_proofs`, in migration 0028**: the sha256 of the key's thumbprint and the
  `jti`, kept until `iat` and 5 minutes; one insert per request, which a second use on any
  replica finds. A row that expired is taken over, and each replica deletes the expired rows at
  most once a minute, at a check. The `jti` is recorded once the token is authenticated, so that
  a request with a made-up token writes nothing.
- **B. A cache per replica**: a proof replays on another replica, which the decision rules out.

**8.6 Server nonces (RFC 9449 §8, §9).**

- **A. None**: no answer has `DPoP-Nonce`, and a `nonce` claim is ignored. A nonce stops the
  proofs that a script on the origin signs now for later (§11.2). But such a script can already,
  while the page is open, do what the token may do and create a deploy token or, as an admin, a
  user, which outlive the session (threat model B-5); and the proofs it signs carry the token's
  hash, so they end with the token, 24 hours at most. The UI needs no retry on `use_dpop_nonce`,
  the server keeps no nonce, and CORS exposes no `DPoP-Nonce`.
- **B. Required**: a nonce of the server's (a MAC of the time with a key the replicas share,
  changing every few minutes), the first request of a UI `401` with `use_dpop_nonce` and
  `DPoP-Nonce`, and the UI signs again; `iat` would then not depend on the browser's clock.
  Worth it once a bound token may no longer create credentials.

**8.7 The OIDC code bound to the key (RFC 9449 §10).**

- **A. `dpop_jkt` with `return_to`, required**: the login keeps the thumbprint, and the code is
  redeemed only with a proof of that key; a code of another key uses it up, as a wrong verifier
  does. A code and a verifier read from the history, where the fragment stays, or from the page
  are of no use on another machine (§11.9). One column of 0028, one parameter.
- **B. PKCE alone**: whoever has the code and the verifier binds the token to a key of their own.

**8.8 The answers.** The login answers the `CreatedToken` with `dpopJkt` and
`tokenType: "DPoP"`, the `token_type` of RFC 9449 §5 in the API's camelCase. A missing or
refused proof on the login is `400 INVALID`, as a body that is no login. On `/api/` a refused
proof is `401` with `WWW-Authenticate: DPoP error="invalid_dpop_proof", error_description,
algs="ES256"`, and a token refused with it `error="invalid_token"` (§7.1); a bound token as a
bearer or in Basic credentials is `401` with `DPoP error="invalid_token"` on `/api/` (§7.2), and
with the route's challenge on the frontends, `/v2` and `/v2/token`; `Authorization: DPoP`
outside `/api/` is `401`. The CORS of #949 allows the `DPoP` request header and exposes
`WWW-Authenticate` (§7.1).

**8.9 JOSE.** The proof is a JWT checked with `github.com/golang-jwt/jwt/v5`, already approved
(layout.md §3) for the bearer of `/v2` and the ID tokens of #69: `WithValidMethods(ES256)`, the
key of the header's `jwk` parsed and its thumbprint computed in `internal/auth/dpop`.
`github.com/go-jose/go-jose/v4` is in the module graph, through sigstore, but not in `go.mod`:
it would be a new direct dependency for what fifty lines do.

## Decision

Proposed, option A of each but 5, where B: `POST /api/v1/login` takes a user name and a password,
or an OIDC code and its PKCE verifier, and answers a personal access token without scopes that
expires after `auth.login.tokenLifetime` (12h, at most 24h), with no refresh; sign-out revokes it
with `DELETE …/tokens/<id>`; the password is throttled and the token audited as npm's login, the
OIDC code as the browser login; `/-/oidc/login` takes `return_to` and an S256 `code_challenge`
and its callback redirects with a code in the fragment; `GET /api/v1/info` adds `login`; no
cookie for `/api/v1`. The token is bound to the browser's DPoP key (decided), with option A of
each of 8: `tokens.dpop_jkt`, ES256, `htu` against `server.externalURL`, `iat` within 5
minutes, `dpop_proofs` shared by the replicas, no server nonce, the OIDC code bound by
`dpop_jkt`. Specified in [http-routing.md](../spec/http-routing.md) §4 and §7,
[rbac.md](../spec/rbac.md) §10 and [index-schema.md](../spec/index-schema.md) (`tokens`,
`oidc_logins`, `dpop_proofs`, migration 0028).

## Consequences

- The UI signs in with one request for a password, and with a redirect and one request for OIDC,
  on its own origin or another; the server sets how long a browser stays signed in.
- Every sign-in is a personal access token in the person's list and the audit log, as today; a
  tab closed without signing out leaves it live until it expires, at most 24 hours.
- A token read out of the browser is of no use without the key, on any route (B-5). A script on
  the UI's origin still acts as the person while the page is open: it signs proofs with the key
  it cannot export, and may create what the token may create, a deploy token, or as an admin a
  user, which are not bound.
- The UI makes a non-extractable P-256 key with WebCrypto, keeps it in IndexedDB, and signs a
  proof for the login and for every request (lajstrom-ui); a request of the UI costs one insert
  into `dpop_proofs`.
- `oidc_logins` gets four columns, `tokens` one, and `dpop_proofs` is new (0028). A replica of the
  release before finishes a UI's login as its own, on the token page, and takes a bound token
  for a bearer, so the UI's sign-in needs every replica on this release (8.1).
- Not here: a CI's token exchange, and joining an existing user to an identity (#69).
