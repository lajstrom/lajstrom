# Security policy

## Reporting a vulnerability

Please report vulnerabilities **privately** through GitHub private vulnerability reporting:

1. Open <https://github.com/lajstrom/lajstrom/security/advisories/new>
   (or the **Security** tab of this repository, then **Report a vulnerability**).
2. Describe the issue, the affected version or commit, and how to reproduce it. A proof of concept
   helps, but is not required.

Do not open a public issue, pull request or discussion for a suspected vulnerability.

## What to expect

- **Acknowledgement** within 3 business days.
- An initial assessment, and a decision whether the report is accepted, as soon as we have
  reproduced it. We keep you informed while we work on a fix.
- **Coordinated disclosure.** We agree on a disclosure date with you, publish a GitHub security
  advisory together with the fix, and request a CVE where appropriate. We aim to disclose within
  90 days of the report, or earlier once a fix is available. Please do not disclose the issue
  publicly before then.
- Reporters are credited in the advisory unless they prefer otherwise.

## Scope

In scope:

- The code in this repository: the server, protocol emulation for the supported package formats,
  pass-through of container images, the index and control plane, RBAC and audit logging.
- Issues that let a client bypass Lajstrom's authorization or audit, read or modify artifacts it
  should not have access to, or corrupt the index or the backing registry.
- The build, CI and release configuration in this repository (supply-chain issues).

Out of scope:

- Vulnerabilities in the OCI registries that Lajstrom uses as storage, or in third-party
  dependencies, unless Lajstrom's use of them makes it exploitable; please report those to the
  respective upstream project. Tell us as well if Lajstrom is affected.
- Findings that require an already compromised host, registry or administrator account.
- Volumetric denial of service, social engineering and physical attacks.

## Supported versions

Lajstrom is pre-release and in the design phase. **No version is supported yet**; the published
`v0.0.1` only reserves the name. Until the first release, fixes land on `main` only. This section
will list supported versions once releases exist.
