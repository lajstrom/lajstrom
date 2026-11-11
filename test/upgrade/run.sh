#!/usr/bin/env bash
# The upgrade test (#485, docs/testing.md): the end-to-end suite of the previous release, run by
# that release's binary, on a Postgres schema that this checkout's binary migrated. That is what
# a rolling update does while the replicas of the release before still serve, and what a
# rollback does (docs/spec/index-schema.md, Upgrades; INV-41 of docs/spec/guarantees.md). The
# Upgrade workflow (.github/workflows/upgrade.yml) runs it.
#
#   test/upgrade/run.sh [<scenario>...]     e.g. test/upgrade/run.sh raw oras pypi
#
# The release to upgrade from is $LAJSTROM_UPGRADE_FROM, any ref (a tag, a branch, a commit), else
# the previous release: the highest tag vX.Y.Z from v0.1.0 on that is an ancestor of HEAD and not
# HEAD itself. v0.0.1 reserved the name before there was any code, and does not count. Without
# such a tag the script says so and exits 0: there is nothing to upgrade from. Then:
#
#   1. git worktree add of that ref in a temporary directory, and make build there and here;
#   2. a schema of its own in the Postgres database of $LAJSTROM_UPGRADE_POSTGRES_DSN, a URL whose
#      user may create schemas there (the dev stack's:
#      postgres://lajstrom:lajstrom@localhost:5432/lajstrom?sslmode=disable);
#   3. lajstrom migrate of the previous release on it: the schema as that release leaves it;
#   4. lajstrom migrate of this checkout: the migrations of the next release, as the Job of
#      docs/operations.md (Upgrades) applies them before a rolling update; a migration that
#      breaks the release before is refused here, without --allow-breaking;
#   5. lajstrom migrate of the previous release again: whether it runs on the schema it finds;
#   6. the test/e2e/run.sh of that ref with LAJSTROM_E2E_INDEX_DSN naming the schema: it builds
#      and serves the previous release's binary on it, and runs the scenarios of the arguments,
#      else every scenario of that release. The rest of the environment reaches it as it is
#      (LAJSTROM_E2E_REGISTRY and the others of its header).
#
# The run.sh of a ref before #485 does not know LAJSTROM_E2E_INDEX_DSN and cannot run here. The
# exit status is run.sh's: 1 when a scenario failed, 2 when it could not start; 2 also when a step
# before it failed. On exit the schema is dropped and the worktree removed. It needs git, make,
# go and psql, and what run.sh needs: curl, jq, and Docker for the clients.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
say() { printf 'upgrade: %s\n' "$*"; }
die() {
	printf 'upgrade: %s\n' "$*" >&2
	exit 2
}

from="${LAJSTROM_UPGRADE_FROM:-}"
if [ -z "$from" ]; then
	# Stable tags only, newest first; a v0.0.x is below the floor.
	from="$(git -C "$root" tag --merged HEAD --no-contains HEAD --list 'v*' --sort=-v:refname |
		grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | grep -v '^v0\.0\.' | head -n 1)" || true
	if [ -z "$from" ]; then
		msg="no release from v0.1.0 on before HEAD: nothing to upgrade from, skipped"
		[ "${GITHUB_ACTIONS:-}" != true ] || echo "::notice title=upgrade skipped::$msg"
		say "$msg"
		exit 0
	fi
fi

for tool in git make go psql; do
	command -v "$tool" >/dev/null || die "$tool is required"
done
pg="${LAJSTROM_UPGRADE_POSTGRES_DSN:-}"
[ -n "$pg" ] || die "LAJSTROM_UPGRADE_POSTGRES_DSN is required: a Postgres URL whose user may create a schema"
commit="$(git -C "$root" rev-parse --verify --quiet "$from^{commit}")" || die "$from is not a commit of this repository"
unset LAJSTROM_CONFIG LAJSTROM_SERVER_LISTEN LAJSTROM_SERVER_EXTERNAL_URL LAJSTROM_SERVER_METRICS_LISTEN \
	LAJSTROM_LOG_LEVEL LAJSTROM_LOG_FORMAT LAJSTROM_DATABASE_DRIVER LAJSTROM_DATABASE_DSN

tmp="${TMPDIR:-/tmp}"
tmp="$(mktemp -d "${tmp%/}/lajstrom-upgrade.XXXXXX")"
worktree="" schema=""
finish() {
	if [ -n "$schema" ]; then
		psql -q "$pg" -c "DROP SCHEMA $schema CASCADE" >/dev/null 2>&1 || say "could not drop the schema $schema" >&2
	fi
	[ -z "$worktree" ] || git -C "$root" worktree remove --force "$worktree" >/dev/null 2>&1 || true
	rm -rf "$tmp"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# 1. The previous release in a worktree, and both binaries.
worktree="$tmp/previous"
git -C "$root" worktree add --quiet --detach "$worktree" "$commit" || die "git worktree add of $from failed"
grep -q LAJSTROM_E2E_INDEX_DSN "$worktree/test/e2e/run.sh" 2>/dev/null ||
	die "the test/e2e/run.sh of $from cannot run on Postgres: it is older than LAJSTROM_E2E_INDEX_DSN (#485)"
for dir in "$worktree" "$root"; do
	(cd "$dir" && make --no-print-directory build) >"$tmp/build.log" 2>&1 ||
		{ cat "$tmp/build.log" >&2; die "make build failed in $dir"; }
done
previous="$worktree/bin/lajstrom" next="$root/bin/lajstrom"
say "from $from ($("$previous" version | head -n 1)) to this checkout ($("$next" version | head -n 1))"

# 2. A schema of this run.
schema="lajstrom_upgrade_$(date +%Y%m%d%H%M%S)_$$"
psql -q -v ON_ERROR_STOP=1 "$pg" -c "CREATE SCHEMA $schema" >/dev/null ||
	{ schema=""; die "psql could not create a schema with LAJSTROM_UPGRADE_POSTGRES_DSN"; }
case "$pg" in *\?*) dsn="$pg&search_path=$schema" ;; *) dsn="$pg?search_path=$schema" ;; esac
cat >"$tmp/migrate.yaml" <<EOF
# Written by test/upgrade/run.sh for lajstrom migrate.
server:
  listen: "127.0.0.1:8080"
  insecureHTTP: true
database:
  driver: postgres
  dsn: "$dsn"
EOF

# migrate WHO BINARY runs lajstrom migrate of BINARY on the schema and prints what it says: the
# first and the last migration it applied, or what it found.
migrate() {
	local out what
	out="$("$2" migrate --config "$tmp/migrate.yaml" 2>&1)" ||
		{ printf '%s\n' "$out" >&2; die "lajstrom migrate of $1 failed on the schema $schema"; }
	what="$(printf '%s\n' "$out" | sed -n 's/^applied //p' | sed -n '1p;$p' | uniq | paste -sd ' ' -)"
	if [ -n "$what" ]; then
		what="applied ${what/ / to }"
	else
		what="$(printf '%s\n' "$out" | grep '^schema ' || true)"
	fi
	say "lajstrom migrate of $1: $what"
}

# 3 to 5.
migrate "$from" "$previous"
migrate "this checkout" "$next"
migrate "$from" "$previous"

# 6. The suite of the previous release, with its binary, on the schema.
say "the end-to-end suite of $from on the schema $schema"
rc=0
LAJSTROM_E2E_INDEX_DSN="$dsn" "$worktree/test/e2e/run.sh" "$@" || rc=$?
if [ "$rc" = 0 ]; then
	say "PASS: the binary and the end-to-end suite of $from on the schema this checkout migrated"
else
	say "FAIL: the end-to-end suite of $from exited $rc on the schema this checkout migrated" >&2
fi
exit "$rc"
