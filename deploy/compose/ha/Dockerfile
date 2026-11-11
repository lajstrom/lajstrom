# The Lajstrom of the ha profile (docs/dev-stack.md): bin/lajstrom built from the working tree
# in a Go container, on the base image of the release image (the Dockerfile at the root, which
# copies binaries goreleaser built and compiles nothing). make dev-up PROFILES=ha rebuilds it on
# every start (pull_policy: build); the module and build caches make that a few seconds when
# nothing changed. Dockerfile.dockerignore keeps the context to the Go sources.

# Go 1.27.4 on Alpine, the version of go.mod, pinned by the digest of its index.
FROM golang:1.27.4-alpine@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS build
# The context is the Go sources only (Dockerfile.dockerignore); go build fetches the modules into
# the cache mount, so there is no step of its own for them.
COPY . /src
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    cd /src && CGO_ENABLED=0 go build -trimpath -o /out/lajstrom \
    -ldflags '-X github.com/lajstrom/lajstrom/internal/version.Version=dev-ha' ./cmd/lajstrom

# The base of the release image: distroless static, Debian 13, nonroot.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build --chmod=0555 /out/lajstrom /lajstrom
USER 65532:65532
WORKDIR /home/nonroot
EXPOSE 8080
ENTRYPOINT ["/lajstrom"]
CMD ["serve"]
