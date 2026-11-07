# 0004. Path-first addressing: one host, `/<format>/<repo>/`

- Status: accepted
- Date: 2026-11-06
- Deciders: gecube

## Context

Artifact servers address repositories in one of three ways: a path per repo under one host
(Nexus: `/repository/<name>/`), a host per repo (Artifactory SaaS subdomains, some apt mirrors),
or a mixed scheme per format. Lajstrom must also give every artifact a stable OCI name in the
backend registry, and that name must be derivable from the URL and back, so that `oras push`,
the frontends and `reindex` agree without a lookup table.

Some formats fold several "repositories" into one URL space (apt suites and components, maven
releases vs snapshots), and some clients need the repository at the root of a host (the
terraform registry protocol discovers `/.well-known/terraform.json` on the host).

## Considered options

- One host, path-first: `https://host/<format>/<repo>/…`, OCI name `<format>/<repo>/<pkg>`.
- Host per repo: `https://<repo>.host/…`, format inferred from the repo's configuration.
- Multi-format repos: one repo name serving several formats under format-specific sub-paths.

## Decision

Path-first. The first URL segment is the format (also the protocol frontend), the second is the
repo name, unique within its format. The OCI repository path mirrors the URL:
`<prefix><format>/<repo>/<package path>`. A repo has exactly one format; there is no
multi-format repo entity. Inside a repo, apt suite and component are upload metadata, not
sub-repositories; maven releases and snapshots are different repos.

The host-per-repo form (`<repo>.host`) is an optional edge rewrite to the path form, added after
the MVP for the terraform protocol (namespace = repo, discovery on the host) and for wildcard
certificates; it introduces no new naming.

Reserved first segments: `v2`, `api`, `ui`, `-`, `.well-known` and all format names.

## Consequences

- URL, RBAC resource (`<format>/<repo>`), control-plane key and OCI path are the same string;
  no mapping table, and `reindex` can list a prefix in the backend and know which repo each
  OCI repository belongs to.
- Clients get one base URL per repo and one set of credentials for every protocol.
- A team that wants "one repo for everything" configures several repos with a shared name
  (`apt/payments`, `maven/payments`) and a binding mask `*/payments`.
- Terraform's host-rooted discovery needs the FQDN mode later; it is not blocked by this
  decision.
