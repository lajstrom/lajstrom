# 0025. Node and workload identity: client certificates and Kubernetes service-account tokens at `/v2/token`

- Status: proposed
- Date: 2026-11-15
- Deciders: gecube

## Context

A fleet of nodes pulls from Lajstrom with one deploy token over a mask (#911): one long-lived
secret, copied into every node's configuration. #1019 asks for the step after it, where no
secret of Lajstrom's is stored on the node at all:

- **A client certificate.** containerd presents one to a registry from `hosts.toml`
  (`client = [["client.crt", "client.key"]]`), and Talos from its machine configuration
  (`machine.registries.config.<host>.tls.clientIdentity`, or a `RegistryTLSConfig` document since
  1.12). The key never crosses the wire, a leaked log holds nothing to replay, and the operator's
  CA, not Lajstrom, decides who gets one. A SPIFFE ID in the URI SAN (`spiffe://cluster-a/node/w1`)
  names the node; a plain CA names it in the CN or a DNS SAN.
- **A Kubernetes service-account token.** KEP-4412 (beta and on by default since Kubernetes 1.34,
  `KubeletServiceAccountTokenForCredentialProviders`) lets the kubelet hand a credential-provider
  plugin a short-lived token of the pod's service account, for an audience the plugin's
  configuration names. The Kubernetes docs name the "pass-through mode": the plugin returns the
  token as it is, as the password of the registry. This is ECR with IRSA or GAR with workload
  identity: no static secret for workloads. It does not cover the images the node pulls for
  itself (kubelet, etcd, the Talos installer): containerd pulls those with the machine's
  credentials, a certificate or #911.

What Lajstrom has ([rbac.md](../spec/rbac.md)): the principals `user:<uid>` (local, OIDC and,
with #1020, service accounts), `token:<id>` (deploy tokens), `group:<name>` (OIDC claims, a
principal of bindings only) and `anonymous`. Bindings are `(principal, role, mask)`, the mask a
`<format>/<repo>` with `*` as a whole segment. The token endpoint (§4) takes Basic on `GET` and
the OAuth2 password form on `POST` (#908). TLS ends at the edge, or in Lajstrom with
`server.tls`; behind an edge Lajstrom believes `X-Forwarded-For` from `server.trustedProxies`
only, and no other forwarded header ([ADR 0011](0011-client-address-behind-a-proxy.md)). The OIDC
login (#69, §8) already reads a discovery document and caches a JWKS.

[ADR 0021](0021-service-accounts.md) (#1020, proposed) makes a service account a user of kind
`service` with an owner and personal access tokens. It is a principal for a system that holds a
secret of Lajstrom's.

The issue leaves open: the principal strings and where they sit among the principals, how the
certificate reaches Lajstrom, the trust bundles, which name of the certificate counts, rotation
and revocation, the audit, and for KEP-4412 the issuer's discovery, the JWKS cache, the audience
and the mapping of issuer, namespace and service account to a principal.

## Considered options

### 1. The principal model

- **A. Principals of their own, bound directly.** The certificate's identity and the service
  account are the principal of the request: `cert:spiffe://cluster-a/node/w1`,
  `k8s:prod/ci/builder`. A binding names one, or a pattern that ends in `/*` for a fleet
  (`cert:spiffe://cluster-a/*` with `reader` on `oci/*`). Nothing is stored per node: the
  principal exists through its credential, as a group exists through the identities that name it.
  The bearer's `sub`, the audit row, `lajstrom.uploaded-by` and the bound of upload sessions per
  principal see each node and each service account by itself.
- **B. Mapped onto a service account** (ADR 0021). A rule maps a pattern of identities to one
  service account, whose bindings apply; the identity goes into the audit details. The fleet gets
  an owner, and disabling the account stops it. But a thousand nodes share one uid in the bearer,
  the access log and the per-principal bounds; the account's tokens, its token bound and its
  owner's rules describe a secret that nobody holds here; the rules need a table, an API and a
  CLI; and the feature waits for #1020.
- **C. Mapped onto a deploy token over a mask.** The same as B with a token instead of an account:
  no owner, no bindings beyond the mask and the scopes, and the identity gone from the principal.

### 2. The kind's name

- **A. `cert:`**, the credential's name, as `token:` is a deploy token's. Behind Istio the
  certificate of an in-mesh workload arrives as well as a node's (`spiffe://…/ns/ci/sa/builder`),
  and `cert:` names both right.
- **B. `node:`**, the issue's example. It misnames every certificate that is not a node's.

### 3. How the certificate reaches Lajstrom

- **A. Lajstrom's own TLS** (`server.tls`): the listener asks for a client certificate.
- **B. The edge's header**: Envoy and Istio `X-Forwarded-Client-Cert`, nginx
  `$ssl_client_escaped_cert` (ingress-nginx's `ssl-client-cert`), Traefik's
  `X-Forwarded-Tls-Client-Cert`, read only from `server.trustedProxies`.
- **C. Both**, each where it applies: TLS terminates in one place or the other (http-routing.md
  §1), and the Kubernetes default is the edge.

### 4. Trust bundles

- **A. Static PEM files**, one bundle per CA or SPIFFE trust domain, read at start like
  `server.tls`'s files.
- **B. SPIFFE federation**: the bundle endpoint of the trust domain, or the Workload API of a
  SPIRE agent beside Lajstrom. Rotates the bundle without a restart, at the cost of a SPIFFE
  client in Lajstrom.

### 5. Revocation

- **A. Short lifetimes, no CRL or OCSP.** A certificate counts until its `notAfter`. The levers are
  the bindings (from the next `/v2/token` on, 20 minutes at most for a bearer issued before), the
  bundle, and the CA's lifetimes.
- **B. A CRL file per bundle.** A file to keep fresh, read again on a timer, and a stale one either
  fails open or stops every node.
- **C. OCSP.** A request to the responder on the token path, or stapling, which containerd does
  not do for client certificates.

### 6. A service-account token at `/v2/token`

- **A. A pass-through plugin.** The plugin returns the token as the password; containerd sends it
  to `/v2/token` as Basic or in the form, like any password. Lajstrom ships the plugin as
  `lajstrom credential-provider`: it reads the kubelet's request and writes the answer, with no
  network.
- **B. An exchange plugin.** The plugin trades the token at a Lajstrom endpoint for a credential
  of Lajstrom's: an endpoint more, and a secret on the node again, in the kubelet's cache.
- **C. TokenReview.** Lajstrom asks the API server whether a token is valid: it needs credentials
  for every cluster, and the API server on the path of every pull.

### 7. The principal of a service account

- **A. `k8s:<cluster>/<namespace>/<service account>`**, `<cluster>` the name the configuration
  gives the issuer: short, the same across a change of the issuer's URL, and readable in the
  audit log.
- **B. The issuer's URL in the principal.** No name to configure, but an EKS issuer is a URL of 70
  characters, and a new cluster URL changes every principal.
- **C. Kubernetes' user name `system:serviceaccount:<ns>:<sa>`**: the same in every cluster.

## Decision

Option A for each question, with C for 3: **a client certificate and a Kubernetes
service-account token are credentials of `/v2/token`, whose principals `cert:` and `k8s:` are
bound directly, a fleet by a pattern; neither is a user nor maps onto a service account.**

- **Principals.** `cert:spiffe://<trust domain>/<path>` for a certificate of a SPIFFE bundle, its
  SPIFFE ID; `cert:<bundle>/<name>` for another bundle, `<name>` the certificate's CN or first DNS
  SAN as the bundle says; `k8s:<cluster>/<namespace>/<service account>`. They sit in rbac.md as a
  section of their own (§9): no row, no password, no token of Lajstrom, no owner.
- **Service accounts.** A `cert:` or `k8s:` principal is not a user and is never mapped onto a
  service account. A service account is the principal of a system that holds a secret of
  Lajstrom's (a CI job's personal access token); `cert:` and `k8s:` are those of a system whose
  platform proves who it is. They share the bindings of §3 and nothing else. An owner for a
  fleet, and mapping a pattern onto a service account (option 1B), can come later without
  changing these kinds.
- **Bindings.** The roles `reader`, `writer` and `maintainer`; `admin` is `400`. A binding's
  principal of these kinds may end in `/*`, which matches every principal of its kind below the
  part before the `*`, at any depth: `cert:spiffe://cluster-a/*`, `cert:spiffe://cluster-a/node/*`,
  `cert:corp/*`, `k8s:prod/*`, `k8s:prod/ci/*`. A `*` anywhere else is `400`, and no identity of a
  request has one.
- **Where the certificate comes from.** With `server.tls` and a bundle, the listener asks for a
  client certificate (`VerifyClientCertIfGiven`, the bundles' CAs as the acceptable ones): a client
  without one goes on as before, and one whose certificate no bundle's CA signed fails the
  handshake. It counts when the peer is not in `server.trustedProxies`, so that an edge's own
  client certificate to Lajstrom is never taken for its client's. Behind an edge,
  `auth.clientCertificates.forwardedHeader` names the header, read only when the peer is in
  `server.trustedProxies`: `X-Forwarded-Client-Cert` is read as Envoy writes it, the `Cert` (and
  `Chain`) of its last element; any other name holds a URL-escaped PEM (nginx, ingress-nginx,
  Traefik) or base64 of the DER (HAProxy). The edge must set the header itself and drop the
  client's; Lajstrom verifies the certificate again against its bundles, so the edge only needs
  to ask for it and forward it.
- **Trust bundles.** `auth.clientCertificates.bundles`: a `name`, a `caFile` of PEM certificates,
  an `identity` (`spiffe`, the default; `cn`; `dns`) and, for `spiffe`, the `trustDomain`, one
  bundle per trust domain. Read at start; a new bundle takes a restart.
- **Verification and the name.** The bundles are tried in their order; the first whose CAs verify
  the chain (the leaf, with the intermediates the client or the header sent) at the server's clock,
  for client authentication (an extended key usage of `clientAuth`, or none), decides. Under
  `spiffe` the leaf has exactly one URI SAN, a SPIFFE ID of the bundle's trust domain with a path
  (X509-SVID); under `cn` the subject's CN; under `dns` the first DNS SAN. A name is 1 to 255
  bytes of printable ASCII without `*`. A certificate that verifies and has no name is refused,
  not passed to the next bundle.
- **On `/v2/token`.** A `GET` without an `Authorization` header and with a verified certificate is
  issued the token of the certificate's principal; Basic credentials, when sent, decide instead. A
  certificate from the header that does not verify or has no name is `401` with the Basic
  challenge. Other routes do not read the certificate.
- **Revocation:** none in the first cut. A certificate counts until its `notAfter`; the operator
  removes a binding, rotates a bundle, or keeps lifetimes short: a day or less where something
  renews them (SPIRE, cert-manager's CSI driver). On Talos the certificate lives in the machine
  configuration and lasts until it is patched (`talosctl patch machineconfig`); bind such nodes
  one by one, not by a pattern, where one must be cut off before its certificate ends.
- **Kubernetes clusters.** `auth.kubernetes.clusters`: a `name`, the `issuer`, the `audience`
  (default `server.externalURL`) and a `caFile` for an issuer behind a private CA. The issuer's
  discovery document (`<issuer>/.well-known/openid-configuration`) names it exactly and gives the
  `jwks_uri`; the JWKS is cached as the OIDC login's (§8): a `kid` it lacks fetches it again at
  most every 30 s, it is fetched again hourly, and the last set that loaded stays while the issuer
  does not answer. Nothing is read at start.
- **The token at `/v2/token`.** A password of Basic (`GET`) or of the form (`POST`) that is a JWT
  whose `iss` is a configured cluster's issuer is that cluster's token, whatever the user name;
  any other password is a password. It must verify: an asymmetric algorithm of §8, `iss` exact,
  `aud` containing the audience, `exp` present (the legacy tokens of Secrets have none), `iat` and
  `nbf` within the leeway of §4, and `sub` equal to `system:serviceaccount:<ns>:<sa>` of its
  `kubernetes.io` claim's `namespace` and `serviceaccount.name`. The principal is
  `k8s:<cluster>/<ns>/<sa>`. A token that does not verify is `401` and counts as no failed login,
  as a token secret does not. Lajstrom does not ask whether the pod or the service account still
  exists: the token's `exp` bounds it.
- **The plugin.** `lajstrom credential-provider` reads the kubelet's `CredentialProviderRequest`
  (`credentialprovider.kubelet.k8s.io/v1`) and answers the token as the password of the registry,
  the user name `k8s`, and a cache duration that ends a minute before the token does. The
  kubelet's `tokenAttributes` name the audience, `requireServiceAccount: true` and `cacheType:
  Token`.
- **Audit.** The bearer tokens of `/v2/token` stay unaudited (§5); the writes they allow are audit
  rows with the `cert:` or `k8s:` principal, which `lajstrom.uploaded-by` carries too. Each token
  issued on a certificate or a service-account token is logged at debug level with its principal
  and its bundle or cluster. A binding of these kinds is a `binding.create` and `binding.delete`
  row as any.
- **Out of scope:** the certificate and the service-account token on any route but `/v2/token`
  (the frontends, `/api/v1`, `/v2` without the token flow); CRL and OCSP; SPIFFE federation and the
  Workload API; a static JWKS file; TokenReview; the `admin` role for these principals; a `*` in
  the middle of a pattern; patterns for users and groups; mapping onto service accounts.

## Consequences

- A node pulls with a key that never leaves it, and a pod with its service account, and nothing
  on either is a secret of Lajstrom's. The audit log and the access log name each node and each
  service account.
- Two principal kinds more: the evaluation of §3 matches patterns for them, and every place that
  reads a principal takes `cert:` and `k8s:`. A replica of the release before skips their bindings
  (logged at warn as bindings that grant nothing) and refuses a bearer whose `sub` it does not
  parse (`401`): the client gets another token, which may come from a replica that cannot issue
  it. The feature works once every replica has it.
- A certificate cannot be revoked before its end. Talos keeps its certificate in the machine
  configuration, so a node's certificate there is as long-lived as the operator makes it; the gain
  over a deploy token is proof of possession, not a shorter life.
- With `server.tls` and a bundle, a client whose certificate no bundle's CA signed cannot connect
  at all, anonymous reads included: a misconfigured node fails at the handshake, not at a `404`.
- One header more is believed from `server.trustedProxies`, which ADR 0011 kept to
  `X-Forwarded-For`; the edge's duty to set it and drop the client's goes to the threat model's
  list of what the edge must do.
- The pass-through plugin puts no Lajstrom code in the kubelet but the plugin itself, a mode of the
  `lajstrom` binary that must be on the node; on Talos that is a system extension or a mount
  (To verify).

## To verify (the spike of #1019)

On a throwaway Talos cluster, when the heavy container slot of the machine is free. Everything
under the worktree; never the user's `~/.kube` or `~/.talos` contexts, whose shasums are taken
before and after.

1. **Setup**, in OrbStack, as the boots of [the Talos guide](../guides/talos.md) §3:
   - a scratch directory `spike/` in the worktree, with `export TALOSCONFIG=$PWD/spike/talosconfig
     KUBECONFIG=$PWD/spike/kubeconfig`;
   - a test CA and a SPIFFE-style CA made with `openssl` in `spike/pki/`, a client certificate per
     node (URI SAN `spiffe://spike.test/node/<n>`, and one with CN `node-<n>` for the plain CA),
     lifetimes of a day;
   - `lajstrom serve` on the host with `server.tls` (a server certificate for
     `host.docker.internal`), a SQLite index, the dev registry, the bundles of both CAs, an `oci`
     proxy repo of registry.k8s.io and ghcr.io, no anonymous read, and the bindings
     `cert:spiffe://spike.test/node/*` and `cert:plain/*` with `reader` on `oci/*`;
   - `talosctl cluster create docker --name ljs-1019 --state $PWD/spike/state
     --talosconfig-destination $PWD/spike/talosconfig --host-ip 127.0.0.1 --subnet
     10.219.0.0/24 --config-patch @spike/patch-registries.yaml`, the patch with the mirrors of the
     guide and `machine.registries.config.host.docker.internal:<port>.tls` with `ca` and
     `clientIdentity` (`crt`, `key`, base64); a second run with the `RegistryTLSConfig` document
     (`cert`, `key`, PEM).
2. **Lajstrom terminating TLS:** both nodes `Ready` with every image through Lajstrom; the access
   log's principal of the pulls is `cert:spiffe://spike.test/node/<n>`; `/v2/token` is a `GET`
   without `Authorization` over the same TLS client (containerd's token fetch uses the host's TLS
   configuration); a node without `clientIdentity` gets the `401` challenge only.
3. **Behind an edge:** an nginx container in OrbStack terminating TLS with
   `ssl_verify_client optional_no_ca` and `proxy_set_header X-SSL-Client-Cert
   $ssl_client_escaped_cert`, Lajstrom on plain HTTP behind it with `server.trustedProxies` the
   container's address and `forwardedHeader: X-SSL-Client-Cert`: the same principals; a request
   that sets the header itself from outside the list is anonymous. If an Envoy image is at hand,
   the same with `forward_client_cert_details: SANITIZE_SET` and `X-Forwarded-Client-Cert`.
4. **KEP-4412:** the kubelet of the cluster (Kubernetes 1.34 or later) with
   `machine.kubelet.extraArgs` `image-credential-provider-config` and
   `image-credential-provider-bin-dir` under `/var/lib/lajstrom-cp`, mounted into the kubelet with
   `machine.kubelet.extraMounts`, the configuration written by `machine.files`, the `lajstrom`
   binary (linux, the nodes' architecture) copied there by a privileged DaemonSet with a
   `hostPath`; the issuer `https://<control plane>:6443` with the cluster CA as `caFile`, after
   `kubectl create clusterrolebinding oidc-reviewer --clusterrole=system:service-account-issuer-discovery
   --group=system:unauthenticated`. A pod with an image of a repo bound only to
   `k8s:spike/default/puller` starts with that service account and fails with `default`. If Talos
   refuses the mount, the same on a kind cluster in OrbStack with its own kubeconfig under
   `spike/`.
5. **Afterwards:** `talosctl cluster destroy --name ljs-1019 --state $PWD/spike/state`, the nginx
   container removed, the shasums of `~/.kube/config` and `~/.talos/config` unchanged. The
   measured results go into the guide, and this section says which held.

## Verified (the spike of #1019, 2026-11-15)

Run as planned above, at #1224 (e16fe920, with the fix 7c02b23a), on Talos v1.14.5 (Kubernetes
v1.37.1) with the docker provisioner in OrbStack: one control plane and no worker, two clusters
in turn. The measurements are in [the Talos guide](../guides/talos.md) §3.1 and §3.2.

- **`clientIdentity` reaches Lajstrom both ways.** With Lajstrom terminating TLS, through
  `machine.registries.config…tls.clientIdentity`: the boot completed, and every image came through
  as `cert:spiffe://spike.test/node/cp1`. containerd asked `/v2/token` with a `GET` and no
  credential over the TLS configuration of the host, as the issue expected. Behind nginx, through
  a `RegistryTLSConfig` document and `$ssl_client_escaped_cert`, every image came through as
  `cert:plain/node-cp1`. A header a client sent through nginx was replaced, and nginx outside
  `server.trustedProxies` was not believed.
- **The principal strings** are those of the decision: `cert:spiffe://<trust domain>/<path>` and
  `cert:<bundle>/<CN>`, one per node in the access log.
- **KEP-4412 works on Talos** with four settings the decision did not name, now in the guide and
  in docs/auth.md:
  - a `KubeAuthenticationConfig` document that adds the discovery paths to the anonymous
    conditions, since Talos's API server takes anonymous requests on `/livez`, `/readyz` and
    `/healthz` only;
  - the RBAC that lets anonymous readers read the discovery;
  - the RBAC verb `request-serviceaccounts-token-audience` on Lajstrom's audience for
    `system:nodes`. The kubelet passes no credential without it
    (`ServiceAccountNodeAudienceRestriction`);
  - Talos's `KubeCredentialProviderConfig` document, with the binary in
    `/usr/local/lib/kubelet/credentialproviders`. The node's root filesystem is read-only, so a
    real node needs a system extension; the spike bind-mounted the directory into the node.

  The pod of the bound service account pulled as `k8s:spike/default/puller`, and one of another
  service account was refused.
- **Found and fixed:** the API server answers its JWKS as `application/jwk-set+json`, and `406` to
  a request that accepts `application/json` only, so every token was `503` (7c02b23a, on #1223).
- **Not verified:** a provider binary installed by a system extension on a real node; Envoy's
  `X-Forwarded-Client-Cert` (no Envoy run, unit tests only); a fleet of more than one node.
