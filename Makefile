# Lajstrom developer targets. `make check` runs the CI job list of CONTRIBUTING.md
# (build-test, lint, vulncheck, licenses); run it before every push.

.DEFAULT_GOAL := help

GO      ?= go
BIN     := bin/lajstrom
PKG     := github.com/lajstrom/lajstrom

# Tool versions: the same as .github/workflows/ci.yml.
GOLANGCI_LINT_VERSION ?= v2.15.0
GO_LICENSES_VERSION   ?= v1.6.0
VACUUM_VERSION        ?= v0.39.3
ALLOWED_LICENSES      := Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause,ISC,MPL-2.0,Unlicense,CC0-1.0,0BSD
# Modules whose license was verified by hand because go-licenses cannot classify the text
# (CONTRIBUTING.md, dependency license policy): modernc.org/mathutil is BSD-3-Clause.
LICENSE_IGNORES       := modernc.org/mathutil

# Build information for internal/version; override on the command line, e.g. make build VERSION=v0.1.0.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null)
DATE    := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.Date=$(DATE)

# goreleaser for snapshot and release-check: the version of .github/workflows/release.yml, from
# its image, which brings syft, cosign, docker and buildx. The checkout is mounted at its own
# path (with the common git directory of a worktree), buildx builds on the host's Docker through
# its socket, and dist/ is handed back to the caller's user at the end.
GORELEASER_IMAGE ?= goreleaser/goreleaser:v2.20.0@sha256:19ca96ff782e521af4002ca42dd42021d360a04a8d2a06bb79f720a284c3fee1
GIT_COMMON_DIR   := $(shell git rev-parse --path-format=absolute --git-common-dir 2>/dev/null)
GORELEASER_RUN    = docker run --rm -v /var/run/docker.sock:/var/run/docker.sock \
	-v '$(CURDIR):$(CURDIR)' -w '$(CURDIR)' \
	$(if $(filter-out $(CURDIR)/%,$(GIT_COMMON_DIR)),-v '$(GIT_COMMON_DIR):$(GIT_COMMON_DIR)') \
	-e GIT_CONFIG_COUNT=1 -e GIT_CONFIG_KEY_0=safe.directory -e GIT_CONFIG_VALUE_0='*' \
	--entrypoint sh $(GORELEASER_IMAGE) -c

COMPOSE_FILE := deploy/compose/compose.yaml
COMPOSE      := docker compose -f $(COMPOSE_FILE)
# PROFILES adds the services of compose profiles to dev-up, dev-ps and dev-logs:
# make dev-up PROFILES=s3 also starts distribution on SeaweedFS, PROFILES=s3-minio the same on
# MinIO, the fallback, PROFILES=gitlab the GitLab container registry; several with commas, e.g.
# PROFILES=s3,s3-minio (docs/dev-stack.md). dev-down stops the services of every profile.
ifdef PROFILES
export COMPOSE_PROFILES := $(PROFILES)
endif
# make run uses the local configuration when there is one, the example otherwise.
RUN_CONFIG   := $(firstword $(wildcard deploy/compose/lajstrom.yaml) deploy/compose/lajstrom.example.yaml)

.PHONY: help build test vet lint api-lint vulncheck licenses fmt-check check \
	dev-up dev-down dev-ps dev-logs test-integration e2e run snapshot release-check clean

help: ## List the targets
	@echo "Usage: make <target>"
	@awk -F ':.*## ' '/^[a-z0-9-]+:.*## / { printf "  %-17s %s\n", $$1, $$2 }' $(firstword $(MAKEFILE_LIST))

build: ## Compile every package and build bin/lajstrom with version, commit and date
	$(GO) build ./...
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/lajstrom

test: ## Unit tests with the race detector
	$(GO) test -race ./...

vet: ## go vet
	$(GO) vet ./...

lint: ## golangci-lint at the version CI pins (configuration in .golangci.yml)
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

vulncheck: ## Known vulnerabilities in dependencies and the standard library
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

api-lint: ## Lint the OpenAPI document of the management API (internal/api/openapi.yaml) with vacuum, at the version pinned above
	$(GO) run github.com/daveshanley/vacuum@$(VACUUM_VERSION) lint -d internal/api/openapi.yaml

licenses: ## Dependency licenses against the allowlist of CONTRIBUTING.md
	$(GO) run github.com/google/go-licenses@$(GO_LICENSES_VERSION) check ./... --allowed_licenses=$(ALLOWED_LICENSES) $(foreach m,$(LICENSE_IGNORES),--ignore $(m))

fmt-check: ## Fail when gofmt would change a file
	@files="$$(gofmt -l .)"; if [ -n "$$files" ]; then echo "gofmt -l reports:" >&2; echo "$$files" >&2; exit 1; fi

check: fmt-check build vet test lint api-lint vulncheck licenses ## Everything CI runs, in CI's order, and api-lint, which CI does not run yet

dev-up: ## Start the dev stack (Zot, distribution, Postgres; PROFILES=s3 adds distribution on SeaweedFS, PROFILES=s3-minio on MinIO as the fallback, PROFILES=gitlab the GitLab registry) and wait until it is healthy
	$(COMPOSE) up -d --wait

dev-down: ## Stop the dev stack, every profile; RESET=1 removes the volumes
	$(COMPOSE) --profile '*' down $(if $(filter 1,$(RESET)),-v)

dev-ps: ## Show the services of the dev stack and their health
	$(COMPOSE) ps

dev-logs: ## Follow the logs of the dev stack
	$(COMPOSE) logs -f

test-integration: ## Integration tests against the dev stack; needs LAJSTROM_TEST_REGISTRY=host:port
	@test -n "$$LAJSTROM_TEST_REGISTRY" || { echo "set LAJSTROM_TEST_REGISTRY=host:port (after make dev-up: localhost:5000)" >&2; exit 1; }
	$(GO) test -race -tags integration ./...

e2e: ## End-to-end scenarios with real clients against the dev stack's Zot (docs/testing.md); E2E='raw oras' runs those only
	./test/e2e/run.sh $(E2E)

run: build ## Serve with deploy/compose/lajstrom.yaml, or the example when it does not exist
	./$(BIN) serve --config $(RUN_CONFIG)

snapshot: ## The release without publishing or signing: archives, checksums, SBOMs, cask in dist/, images in the local Docker
	$(GORELEASER_RUN) 'goreleaser release --snapshot --clean --skip=publish,sign; rc=$$?; \
		[ ! -d dist ] || chown -R $(shell id -u):$(shell id -g) dist; exit $$rc'

release-check: ## Validate .goreleaser.yaml with the release's goreleaser
	$(GORELEASER_RUN) 'goreleaser check'

clean: ## Remove build output
	rm -rf bin/ dist/
