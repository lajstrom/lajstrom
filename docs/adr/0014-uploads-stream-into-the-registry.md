# 0014. Uploads stream into the registry: no request spool

- Status: accepted
- Date: 2026-11-10
- Deciders: gecube

## Context

The OCI registry is the only storage (ADR 0001). A frontend upload today, raw `PUT`, apt
upload, pypi upload, maven deploy, npm publish, and a proxy fetch (ADR 0013), is first copied to
a temporary file for the request: `format.UploadedFile` is re-openable (layout.md §2, Decisions),
`Build` reads it to parse the package and compute its digests, and ingest reads it again to push
it. CLAUDE.md allows that spool for one request. Only `/v2` streams: the facade passes a blob
upload's body to the registry and hashes it on the way (#368).

Lajstrom is meant to run on ephemeral disks (a Kubernetes `emptyDir`), decided on 2026-11-10.
There the spool is a cost the operator must size: in the worst case
`server.maxConcurrentUploads × maxFileSize` bytes, nothing bounds it but the disk (#427), and a
full disk fails an upload half way or evicts the pod. Memory is no alternative: packages of a
gigabyte and more exist, and eight of them at once would be gigabytes of RSS.

The sizes, measured on 2026-11-10 from public indexes
([package-sizes-2026-11.md](../design/package-sizes-2026-11.md)): the median package is small
everywhere (8 KiB npm to 670 KiB wheels), p99 is 6–90 MiB, but every ecosystem has a tail 10 to
100 times its p99, and the tail sets the disk. Debian and Ubuntu reach 3.0 GiB (no public .deb
above 4 GiB; the lead builds .debs of 5 GiB); pypi.org stops near 1 GiB, PyTorch's own index
serves 2.1 GiB CUDA wheels; Maven Central's largest jar is 2.3 GiB (2024), nothing above 1 GiB in
2026; npm's largest is 152 MiB; release assets reach GitHub's 2 GiB cap. A spool for eight
concurrent uploads therefore needs one huge file plus seven typical ones, 3–5 GiB for debs, and
a static budget that no client can exceed is `maxConcurrentUploads × maxFileSize`.

What the spool buys is the second read: parsing and hashing before anything reaches the
registry. Every format can do both in one pass over the stream instead:

| Format | What is parsed | Where it is in the stream |
| --- | --- | --- |
| raw | nothing: the digest only | |
| npm | `package/package.json` in the tarball, the tarball inside the publish JSON | anywhere in the tar; npm writes it last (#350): read as the tar streams |
| apt (.deb) | the control file | `control.tar.*`, the 2nd member of the ar archive: at byte 132, under 1 MiB even in the 3.0 GiB package |
| pypi sdist | `PKG-INFO` | anywhere in the tar.gz (setuptools: early; flit, hatchling: last); read as it streams |
| pypi wheel | `*.dist-info/METADATA`, `WHEEL` | a zip: METADATA is at 99.6–99.999 % of the largest wheels and among the last 7 entries in 70 of 80 random ones (tensorflow puts it first); none of 93 inspected uses data descriptors; the central directory, at the end, is up to 2.9 MiB and is the reference |
| maven | the POM is a file of its own, small; jars and others are not parsed | |

And the registry's blob upload already separates the bytes from the commit: bytes go in with
`PATCH`, and nothing is stored until the closing `PUT ?digest=`. A session that is cancelled
before its close leaves nothing.

## Considered options

1. **Keep the spool**, bound it with a budget per replica (#427) and size the `emptyDir` for it.
2. **Hold uploads in memory.** Ruled out by the sizes above.
3. **Small in memory, large on disk.** Two paths to test, and the disk is still needed.
4. **Stream into a registry blob upload while hashing and parsing in the same pass; validate;
   close the upload only when everything passed; cancel it otherwise.**

## Decision

Option 4. A frontend upload, and a proxy fetch, is one pass over its body:

1. The body is read once. A blob upload session belongs to an OCI repository, which for apt
   follows from the package name in the control file: the first bytes are held in memory up to
   the control archive's bound (`MaxControl`, 4 MiB, #243) before the session opens; every other
   format knows its repository from the request (path, file name, form fields). Each byte goes
   to a blob upload session in the registry (`PATCH`, in
   the chunk sizes the provider and its vendor entry choose, #415), to the hashes the format
   needs (sha256 always; md5, sha1, sha512 for apt's and maven's indexes), and to the format's
   streaming inspector, which extracts the metadata it needs as the bytes pass and ignores the
   rest. `maxFileSize` cuts the stream (413).
2. When the body ends, the digests and the metadata are known; nothing is committed yet. The
   plugin builds the manifest and ingest runs every check it runs today: the profile, the
   name and version rules, the claims and conflicts (409), the policy (ADR 0008), the
   manifest-confusion check of npm (ADR 0012).
3. Only if every check passed is the blob upload closed with its digest (`PUT ?digest=`, no
   body after a `PATCH`, #415), and then the manifest pushed and confirmed as today.
4. Any failure before step 3 cancels the session (`DELETE`): nothing is stored, as with a spool
   that is removed. The facade's hold-back of the last byte (#368) is not needed here: the
   close carries the digest Lajstrom computed, so a registry that does not check digests
   stores what was hashed.
5. A file whose metadata cannot be found in one pass (a wheel whose `.dist-info` precedes its
   data with sizes only in data descriptors, or an archive the inspector cannot follow) is
   read back from the registry after the close, by `Range` where the vendor allows it, before
   the manifest is pushed. If that check fails, the manifest is not pushed and the blob is left
   to the registry's garbage collection, as an orphan with no manifest is today.
6. The temporary file remains only where the operator turns it on: `/v2` chunks of unknown
   length for registries that need a length (#425, off by default). `server.spoolBudget`
   (#427) bounds that, and the old paths while formats move.

The order of the move: raw (nothing to parse), npm, apt, pypi sdist, pypi wheel, maven, the
proxy fetch. Each format's move is one issue; the exact Go shapes (a streaming inspector per
plugin, `UploadedFile` without `Open` or with it only for the read-back) are a `contract` PR of
their own before the first format moves.

Accepted by the lead on 2026-11-11.

## Consequences

- **The disk is out of the upload path.** An ephemeral volume needs no room for uploads, and
  the size of a package no longer depends on the local disk: a 5 GiB .deb streams like a 5 KiB
  one. Memory per upload stays bounded by the inspector's buffers (the control archive, the
  metadata files, a tar header), which already have bounds (#243).
- **Rejected uploads cost upload bandwidth to the registry**, which a spool spent on local disk
  instead: the bytes travel before the check that refuses them. The session is cancelled, so
  nothing is stored, but a writer can make Lajstrom send a refused body to the registry; the
  upload quotas of #246 bound it.
- **The idempotent repeat uploads its bytes again** before ingest sees that the version exists
  with the same digest; the registry dedups the blob. A client that sends the digest up front
  (none of ours does today) could skip it later.
- **Two failure modes get rarer, one gets new:** no `ENOSPC` half way and no spool left behind;
  a session the registry expires while a slow client uploads (sessions are kept alive by the
  `PATCH`es) and a cancel that fails (the registry expires the session itself).
- **A push that fails half way cannot be retried from local bytes**: the client retries the
  upload, as it would after any 5xx. And a slow client keeps a registry upload session open for
  its whole transfer.
- **The wheel inspector is the one new parser of substance.** It captures METADATA from the local
  headers as they pass and checks it against the central directory held from the tail at the
  end; the read-back of step 5 covers the rest.
- **Each format's inspector is new code that parses hostile input on a stream.** The bounds of
  #243 apply; each inspector gets a fuzz target.
- **Registries without chunked uploads**, or with small chunk limits (GHCR: 4 MiB a `PATCH`),
  are served by the provider's chunking, measured per vendor (docs/registries/).
- **The read-back of step 5** needs `Range` on blob reads; where a vendor lacks it (Nexus `oci`,
  #352), the whole blob is read again for that rare case.
- CLAUDE.md's code rule changes from "an upload may be spooled to a temporary file for the
  duration of one request" to "an upload streams into the registry (ADR 0014); a temporary file
  only where the operator turns it on (#425)", with the first format that moves.
