# 0023. Terraform modules: the module registry protocol, the repo is the namespace

- Status: proposed
- Date: 2026-11-15
- Deciders: gecube

## Context

Issue #66 (P-03) asks for Terraform's module registry protocol, `modules.v1`, with the
namespace of a module address taken by the repo, and OpenTofu's `oci://` sources named as the
alternative. ADR 0004 reserved `.well-known` for it; http-routing.md §1.1 already names
`/terraform/<repo>/.well-known/terraform.json` as a route of the plugin on a repo host.

What the clients do, read from Terraform 1.17 (`internal/registry`, `internal/getmodules`,
`internal/initwd`) and OpenTofu 1.13 (`internal/registry`), main of 2026-11-15:

- **Discovery.** A module address is `<host>/<namespace>/<name>/<system>`, exactly four parts
  (a `//<subdir>` may follow). The client reads `https://<host>/.well-known/terraform.json`,
  always over TLS, and takes `modules.v1`, a URL resolved against the document's, `http` or
  `https`. It sends the host's credentials with it.
- **Grammar.** `<namespace>` and `<name>`: letters, digits, `-` and `_`, 1 to 64, beginning and
  ending with a letter or a digit, case kept; `<system>`: lowercase letters and digits, 1 to 64.
  A repo name (`^[a-z0-9]+(?:[-._][a-z0-9]+)*$`) is a namespace unless it has a `.`.
- **Versions.** `GET <base><namespace>/<name>/<system>/versions` answers
  `{"modules":[{"versions":[{"version":"1.2.0"},…]}]}`; the client parses each version with its
  version library, skips one it cannot parse, and picks the newest that matches the constraint.
  It then asks for the version as that library prints it: a SemVer version without a leading
  zero prints as written; `1.0.0+a` and `1.0.0+b` are one version to it.
- **Download.** `GET <base><namespace>/<name>/<system>/<version>/download` answers `204` (or
  `200`) with `X-Terraform-Get`, a go-getter address; one that begins with `/`, `./` or `../` is
  resolved against the download URL. go-getter fetches it, and takes it as an archive by its
  extension (`.tar.gz`, `.tgz`, `.zip`, …) or an `?archive=` parameter.
- **Credentials.** `credentials "<host>" { token = "…" }` in the CLI configuration, or
  `TF_TOKEN_<host>`, go as `Authorization: Bearer` to the discovery, the versions and the
  download of that host. Not to the archive: Terraform's go-getter HTTP getter sends the
  credentials of `~/.netrc` only. OpenTofu 1.12 and later sends the registry's credentials to
  the archive when the download answers `200` with `{"location", "use_registry_credentials":
  true}`; on a `204` it does what Terraform does.
- **`terraform login`** needs `login.v1`, an OAuth 2.0 authorization code grant with PKCE on a
  loopback port; without it the command says the host does not support login. The password
  grant is for app.terraform.io only.
- **No publish.** The protocol has no write: registry.terraform.io publishes from Git tags,
  GitLab takes a `.tgz` by `PUT`.
- **OpenTofu 1.10 and later** installs a module from an OCI registry itself: `source =
  "oci://<host>/<repository>?tag=1.2.0"`, a manifest of `artifactType`
  `application/vnd.opentofu.modulepkg` with one `archive/zip` layer, with the credentials of its
  `oci_credentials` blocks or of docker's configuration. No registry protocol is involved.

## Considered options

For the namespace on a repo host (FQDN mode, #75), where the host already names the repo and
Terraform still wants four parts:

- **The repo again**: `infra.art.example.com/infra/vpc/aws`, the address of the main host with
  another host. The repo host's discovery points at the root of its tree, so the plugin serves
  a second form of each read with the repo as its first segment.
- **The main host's discovery on every host**: `modules.v1` an absolute URL of the main host. The
  namespace on a repo host could then name any repo, and requests leave the host the module
  address names, with that host's credentials.
- **No discovery on a repo host**: terraform only on the main host, against §1.1.

For the URL of the archive:

- **Signed**: the path of the archive with a short-lived token in its query, as GitLab does, so
  that Terraform fetches it without credentials of its own.
- **Plain**: the path alone. Terraform then needs a `~/.netrc` entry beside its `credentials`
  block for a repo without anonymous read; OpenTofu 1.12 needs `use_registry_credentials`.

For the archive formats: `.tar.gz` only, or `.tar.gz` and `.zip`. A zip's central directory
comes last, so a streamed upload (ADR 0014) can check it only by holding the end of the body
(pypi's `zipstream` does it for wheels).

## Decision

**The namespace is the repo.** A repo of format `terraform` is a Terraform namespace, on the
main host and on a repo host alike:

- **Main host.** `GET /.well-known/terraform.json` answers `{"modules.v1":"/terraform/"}`, so
  `art.example.com/infra/vpc/aws` reads `/terraform/infra/vpc/aws/versions`: the one tree of
  http-routing.md §1, the namespace in the place of `<repo>`. No other service: no
  `providers.v1`, no `login.v1`.
- **Repo host.** `/terraform/<repo>/.well-known/terraform.json` answers `{"modules.v1":"/"}`, the
  root of the repo's tree there, and the address keeps the repo as its namespace,
  `infra.art.example.com/infra/vpc/aws`. The versions and download routes take a second form,
  `/terraform/<repo>/<repo>/<name>/<system>/…`, whose first segment must be the repo's name
  (`404` otherwise). A module moves between the two hosts by its host alone.
- A repo whose name has a `.` is no namespace. Terraform refuses its address before it asks;
  the routes serve it to other clients. It is not refused at creation, as the FQDN mode does
  not refuse a repo that has no host.

**The model** (normative in manifest-profile.md). A module is a package, `lajstrom.name`
`<name>/<system>` as written (`vpc/aws`), package path `PackagePath(EscapeComponent(name),
EscapeComponent(system))`, looked up by it, so that spellings that differ in case are one
module, the first one published holding it. A version is a SemVer 2.0.0 version without build
metadata and without a `v` (`1.2.0`, `2.0.0-rc.1`): the version Terraform asks for is then the
one listed. No variant; the tag is `Escape(version)`. One file layer, the archive, media type
`application/vnd.lajstrom.terraform.module.v1.tar+gzip`, titled
`<name>-<system>-<version>.tar.gz`; no metadata layer, since nothing of the protocol needs one;
no `lajstrom.terraform.*` annotation.

**Archives: `.tar.gz` only.** A gzip-compressed tar, read whole on its way in: relative paths
without a `..` component, regular files, directories and links whose target is such a path, a
regular file among them, at most 2^18 entries and 1 GiB unpacked (npm's bounds). A `.zip` is
`400`; it can come later as a second media type without changing what exists.

**Publish.** `PUT /terraform/<repo>/<name>/<system>/<version>` with the archive as its body,
`write`, as raw's `PUT`: streamed into the registry, `201`, the same bytes again the idempotent
repeat, other bytes under a version that exists `409`. A version is immutable while it exists;
it is deleted as in every format, through the management API or the `DELETE` of its manifest on
`/v2`, and may then be published again. There is no frontend `DELETE`, as for rpm.

**`X-Terraform-Get` is signed.** The download answers `204` with `X-Terraform-Get`, the path of
the archive route, `/terraform/<repo>/<name>/<system>/<version>/<file>` (on a repo host without
`/terraform/<repo>`), and, for a principal that is not anonymous, `?token=<token>`: a bearer
token of rbac.md §4, signed with the token key, issued for the principal of the download, for
20 minutes, whose one access entry is `{"type": "terraform-archive", "name":
"terraform/<repo>/<package path>:<tag>", "actions": ["pull"]}`. The archive route takes it in
place of credentials, for that version only; `/v2` grants nothing for its type. The archive
route takes the credentials of every frontend too, so `~/.netrc` and `curl -u` work, and
anonymous read. The `204` works with every Terraform and OpenTofu; OpenTofu's
`use_registry_credentials` is not used, since an OpenTofu before 1.12 would take the `location`
of a `200` without its token.

**Credentials.** A personal access token or a deploy token of the repo with `read` in a
`credentials` block or `TF_TOKEN_<host>`, sent as `Authorization: Bearer`, which every frontend
takes (rbac.md §1). `terraform login` is not supported: `login.v1` would make Lajstrom an OAuth
authorization server that issues personal access tokens, a feature of its own, after the OIDC
login (#69).

**Out of scope.** The provider registry protocol (`providers.v1`) and its network mirror: a
provider is a set of signed per-platform zips with a `SHA256SUMS` and its signature, a format of
its own; OpenTofu reads providers from `oci` repos already. Proxy repos of registry.terraform.io
and groups of terraform repos. OpenTofu's `oci://` needs nothing of this plugin: the module
package goes to an `oci` repo with `oras push`, and the quickstart says how.

## Consequences

- `terraform init` works with one `credentials` block and no `~/.netrc`.
- The token travels in a URL: a proxy in front of Lajstrom may log it, as may Terraform's debug
  log (`TF_LOG`). It downloads one version for 20 minutes, which its principal could read
  anyway; Lajstrom's access log never logs a query (§1), and
  `lajstrom admin key rotate --token --drop` ends every token at once.
- The plugin issues and verifies tokens, which only the facade did: serve hands it the
  facade's issuer and verifier behind an interface of its own, and `internal/format` does not
  change.
- `/.well-known/terraform.json` is the first route outside `/<format>/` that a plugin provides;
  the server mounts it for the plugin.
- Two route forms for the reads, one per host kind; the second is a few patterns, not a second
  tree.
- A module archive made as a `.zip` is repacked as a `.tar.gz` until a later issue takes zips.
- Repos named with a `.` get no warning from the server; the quickstart says it.
