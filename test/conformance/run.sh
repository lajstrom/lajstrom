#!/usr/bin/env bash
# Runs the OCI distribution-spec conformance suite against the facade of a running Lajstrom, on
# an oci repo (test/conformance/README.md).
#
#   test/conformance/run.sh [ginkgo flags...]
#
# The suite is the module github.com/opencontainers/distribution-spec/conformance at the tag
# $SPEC_VERSION (default v1.1.1): it is cloned into $CONFORMANCE_DIR (default
# ~/.cache/lajstrom/conformance) once and built there with go test -c, outside Lajstrom's
# module, which gains no dependency. The OCI_* variables of the suite default to the facade at
# http://localhost:8080 and the repo oci/conformance, with the pull, push and content discovery
# workflows; any of them set in the environment wins. junit.xml and report.html go to
# $OCI_REPORT_DIR (default $CONFORMANCE_DIR/report). The exit code is the suite's.
set -euo pipefail

version="${SPEC_VERSION:-v1.1.1}"
dir="${CONFORMANCE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/lajstrom/conformance}"
src="$dir/distribution-spec-$version"
for tool in git go; do
	command -v "$tool" >/dev/null || { echo "run.sh: $tool is required" >&2; exit 2; }
done

if [ ! -d "$src/conformance" ]; then
	mkdir -p "$dir"
	git clone --quiet --depth 1 --branch "$version" https://github.com/opencontainers/distribution-spec.git "$src"
fi
if [ ! -x "$src/conformance/conformance.test" ]; then
	go test -C "$src/conformance" -c -o conformance.test
fi

export OCI_ROOT_URL="${OCI_ROOT_URL:-http://localhost:8080}"
export OCI_NAMESPACE="${OCI_NAMESPACE:-oci/conformance/a}"
export OCI_CROSSMOUNT_NAMESPACE="${OCI_CROSSMOUNT_NAMESPACE:-oci/conformance/b}"
export OCI_TEST_PULL="${OCI_TEST_PULL:-1}"
export OCI_TEST_PUSH="${OCI_TEST_PUSH:-1}"
export OCI_TEST_CONTENT_DISCOVERY="${OCI_TEST_CONTENT_DISCOVERY:-1}"
export OCI_TEST_CONTENT_MANAGEMENT="${OCI_TEST_CONTENT_MANAGEMENT:-0}"
export OCI_HIDE_SKIPPED_WORKFLOWS="${OCI_HIDE_SKIPPED_WORKFLOWS:-1}"
export OCI_REPORT_DIR="${OCI_REPORT_DIR:-$dir/report}"
mkdir -p "$OCI_REPORT_DIR"

echo "conformance $version against $OCI_ROOT_URL/v2/$OCI_NAMESPACE (reports in $OCI_REPORT_DIR)"
cd "$OCI_REPORT_DIR"
exec "$src/conformance/conformance.test" "$@"
