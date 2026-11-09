# The Lajstrom image (#57): the static binary on distroless, as the non-root user. goreleaser
# builds it with buildx from the binaries it built before (.goreleaser.yaml, dockers_v2): the
# context holds linux/amd64/lajstrom and linux/arm64/lajstrom, nothing is compiled here.
# docs/release.md shows a build by hand.

# distroless static, Debian 13, the nonroot tag: CA certificates, tzdata, /etc/passwd, no shell.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329

ARG TARGETPLATFORM

# The labels that do not change between releases; goreleaser adds version, revision, created.
LABEL org.opencontainers.image.title="Lajstrom" \
      org.opencontainers.image.description="Artifact registry on top of any OCI registry" \
      org.opencontainers.image.url="https://lajstrom.dev" \
      org.opencontainers.image.source="https://github.com/lajstrom/lajstrom" \
      org.opencontainers.image.documentation="https://github.com/lajstrom/lajstrom/tree/main/docs" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --chmod=0555 ${TARGETPLATFORM}/lajstrom /lajstrom

# nonroot (65532) of the base image. Its home is the working directory, where the defaults
# point (--config lajstrom.yaml, the SQLite file lajstrom.db): mount the configuration there or
# set LAJSTROM_CONFIG (docs/configuration.md).
USER 65532:65532
WORKDIR /home/nonroot
EXPOSE 8080

ENTRYPOINT ["/lajstrom"]
CMD ["serve"]
