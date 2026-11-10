# 0016. Go: the language of the network operating system

- Status: accepted
- Date: 2026-11-10
- Deciders: gecube

## Context

Lajstrom has been written in Go since its first commit, and nothing recorded why. The question
comes back with every comparison: two young artifact repositories that started in 2026 are
written in Rust, Artifact Keeper and NORA ([comparison.md](../comparison.md)), and the lead and the
team like Rust better as a language.

For decades systems software was written in C and C++, and not because the languages were the
best ones. They were the de facto standard set by the operating systems that mattered, Windows
and the Unix family with Linux at its head: their interfaces were libc and the C calling
convention, their libraries and drivers were C, and a program that wanted to be a first-class
citizen of those systems spoke their language.

The operating system that a networked service lives in today is Kubernetes. It schedules the
processes, wires their network, holds their secrets and configuration, and is extended through
its own API. Its de facto language is Go: Kubernetes itself, its controllers and
controller-runtime, Helm, Flux, Argo CD, Prometheus, etcd, containerd. For an artifact registry
the ecosystem that matters most is the same: the OCI registries and their tools, CNCF
distribution, Zot, Harbor, ORAS (`oras-go`, Lajstrom's data path to the registry),
go-containerregistry, cosign and sigstore-go, notation-go, and the OCI conformance suite, are
Go. Their behaviour, the quirks that the registry matrix measures
([registries/README.md](../registries/README.md)) included, is defined by that code.

Lajstrom is developed largely by AI coding agents, with the lead reviewing and merging: hundreds
of pull requests in its first days, each written, tested and fixed by an agent in a loop of edit,
build, test and read the failure. The language decides how fast and how cheaply that loop turns.

What Lajstrom does is I/O: it streams bytes between clients and a registry, hashing and parsing
on the way (ADR 0014). It is not CPU-bound. Its deliverable is one binary and one image that
operators run on Kubernetes (ADR 0015, the Helm chart of M5).

## Considered options

1. **Go.**
2. **Rust**, the language the team prefers.
3. **A JVM language** (Java, Kotlin), as Nexus and Artifactory.

## Decision

Go. Lajstrom is a citizen of Kubernetes and of the OCI ecosystem, and their language is Go, as C
was the language of the operating systems before them. Rust would be the better language in
isolation; Go is the better choice for this product:

- **The libraries exist and are the reference.** `oras-go` and distribution's client code,
  sigstore and notation, the Kubernetes client and controller-runtime: Lajstrom reuses the code
  that defines the behaviour it must match, instead of a port that lags it. The Rust OCI crates
  exist, but are behind on referrers, token flows and chunked uploads, the corners where the
  registries differ.
- **Integration with Kubernetes is first class.** The operator of ADR 0015 is a controller on
  controller-runtime; the Helm chart, the metrics for Prometheus and tracing follow the patterns
  of the projects next to it. The platform teams that run Lajstrom read and write Go, so a patch
  from a user is a realistic event.
- **The toolchain.** Modules with a checksum database for dependencies; a compile measured in
  seconds; cross-compilation to every target from one machine; one static binary without cgo
  (CLAUDE.md), so the image is distroless and the release is goreleaser's archives and images
  ([release.md](../release.md)). Built into the toolchain: the race detector, native fuzzing
  (every parser of hostile input has a target, #51), `govulncheck` with reachability, profiling.
- **Agents work better and cheaper in Go.** This is the lead's experience on this project, not a
  measurement against Rust. A small language with one way to write most things, one format
  (`gofmt`), explicit errors and a huge body of cloud-native code to learn from leaves agents
  less to get wrong; a build in seconds and plain compiler errors make every edit-build-test
  turn short; and fewer turns means fewer tokens and less money. Rust's borrow checker, lifetimes
  and long builds cost more turns per change.
- **The profile fits.** `net/http`, composable `io.Reader`s and goroutines are what a streaming
  gateway needs; the measured cost is 66 MiB of RSS for eight concurrent 1 GiB transfers
  ([operations.md](../operations.md), Performance).
- A JVM is ruled out: a heavy runtime and image for an I/O gateway, slow starts for a stateless
  service meant to scale by replicas, and the stack Lajstrom is an alternative to.

## Consequences

- **What Rust would give and Go does not, accepted:**
  - more memory: Lajstrom idles at tens of MiB where a Rust server idles near 15 MiB;
  - data races are found by the race detector and the tests, not ruled out by the compiler;
  - a garbage collector, whose pauses are not visible in a streaming gateway's latencies;
  - a larger binary.
- **How the gap is narrowed:**
  - `go test -race` in CI for every package;
  - a fuzz target for every parser, run weekly (#51);
  - the invariants of [guarantees.md](../spec/guarantees.md), each with its tests;
  - `govulncheck` on every PR;
  - the standard library first, and new dependencies only from the approved list of
    [layout.md](../spec/layout.md) §3, so the code that parses hostile input is Lajstrom's or
    the standard library's.
- **The choice of language is not a selling point.** What sets Lajstrom apart is the registry
  as its only storage and the guarantees backed by tests ([comparison.md](../comparison.md)), not
  the language. Asked "why not Rust", the answer is this ADR.
- **A component may be written in another language** only for a reason this ADR does not cover,
  for example a parser that exists only in Rust, through a new ADR. cgo stays out.
