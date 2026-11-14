#!/usr/bin/env bash
# End-to-end test of npm repos with npm, pnpm and yarn against a running Lajstrom (#312; ADR 0012,
# ADR 0013; docs/spec/http-routing.md §3 npm and §6; docs/quickstart/npm.md):
#
#   - a hosted repo npm-<run> through POST /api/v1/repos, a user of its own with the writer role
#     on it (and reader on the proxy repo), and a deploy token with read and write;
#   - npm (the Node LTS image): npm login with the user's password (the prompts answered in a
#     pseudo-terminal, as npm needs one), the personal access token it got listed for the user,
#     npm whoami; npm publish of an unscoped package (1.0.0, 1.1.0, and 2.0.0-beta.1 with --tag
#     beta) and of a scoped one; npm install of both into an empty project with an empty cache,
#     and require() of what it installed; npm dist-tag add, ls and rm, and npm install
#     <name>@<tag>; npm deprecate and its message in the packument, and the message cleared;
#   - pnpm 10, the deploy token as _authToken in .npmrc: pnpm whoami (token:<id>), pnpm publish
#     of an unscoped package and of a scoped one that depends on npm's, pnpm add of them into an
#     empty project with an empty store, the dependency resolved from the repo;
#   - yarn 4 (npmMinimalAgeGate: 0, npmAlwaysAuth: true in the registry's entry of npmRegistries,
#     where yarn npm login puts the token, the node-modules linker): yarn npm login, yarn npm
#     whoami, yarn npm publish of an unscoped and a scoped package, yarn npm tag add, list and
#     remove, yarn add of the packages of all three into an empty project with an empty cache;
#   - a proxy repo npmjs-<run> of the upstream $NPM_E2E_UPSTREAM of the server's configuration
#     (npmjs; run.sh configures it as https://registry.npmjs.org/): npm install of
#     $NPM_E2E_PUBLIC (chalk@4.1.2) and its tree into an empty project with an empty cache, every
#     tarball fetched once (an audit row proxy.fetch each) and in the registry; again from an
#     empty cache, every tarball from the registry and none fetched;
#   - the M4 demo (docs/roadmap.md, #311): a group all-<run> of the hosted repo and the proxy,
#     the user a reader on it, and its deploy token, which reads only (write is 400). npm, pnpm
#     and yarn each publish a scoped package that depends on $NPM_E2E_PUBLIC through the group,
#     which hands the write to the hosted repo (details.via in the audit row), with
#     registry=<group> the one registry line and the token of npm login or yarn npm login on the
#     group (pnpm: npm's); each installs from an empty cache through the group, the deploy
#     token pnpm's; every dist.tarball names the group, and nothing is fetched again. A public
#     name ($NPM_E2E_HIDDEN, is-number) published to the hosted repo hides the upstream's
#     versions in the group's packument, and group.merge brings them back;
#   - npm audit through the proxy of a project of $NPM_E2E_VULNERABLE (minimist@1.2.0) and a
#     scoped package of the hosted repo: the upstream's advisories come back; with deny on the
#     proxy of the scope and of that name, none do, the names kept on the server (#514);
#   - npm unpublish refused to the writer (E403: delete is the maintainer's), then, as maintainer,
#     of one version (its dist-tag goes with it) and of the scoped package;
#   - reindex restores the journal: npm dist-tag and npm deprecate through the group, then the
#     hosted repo deleted (409 while it is a member of the group, so the group first) and made
#     again, empty, and POST /api/v1/repos/npm/<repo>/reindex: every packument of the run,
#     dist-tags, deprecations and integrity, is what it was, the unpublished package still 404;
#   - npm logout, which revokes npm's token (401) and leaves yarn's.
#
#   test/e2e/npm.sh [<base url>]     default: http://localhost:8080
#
# A step that checks a row of docs/acceptance.md §3.11 begins with its id, J-01 to J-13 (#515).
#
# It runs as $LAJSTROM_E2E_USER, a user with the admin binding (lib.sh, e2e_credentials; run.sh
# makes one), creates the repos on the registry local ($NPM_E2E_REGISTRY names another), and on
# exit revokes the deploy token, deletes the group and both repos (force: the versions leave the
# index; what was not unpublished stays in the registry) and disables its user. NPM_E2E_UPSTREAM=
# (empty) leaves the proxy out, for a server without an npm upstream (the example configuration,
# or no way to registry.npmjs.org from the server): the group then has the hosted repo only, and
# its packages no public dependency. The proxy and the group's public dependency run by default.
#
# The clients run in node:26.13.0-slim ($NPM_E2E_IMAGE; npm 11.20.0) with --network host, as the
# caller (-u), with the run's directory as /w and a home of its own per client. pnpm
# ($NPM_E2E_PNPM, 10.34.9) and yarn ($NPM_E2E_YARN, 4.18.1, @yarnpkg/cli-dist) are installed from
# registry.npmjs.org into the run's directory first, as pypi.sh installs its clients from PyPI.
# The password goes to the clients through a file and a pipe, the tokens into .npmrc and
# .yarnrc.yml in the run's directory, mode 600, and to curl in config files, never on a command
# line; the script prints no secret and checks that the clients' logs hold none. It needs curl,
# jq and docker, prints one line per step and exits non-zero at the first failure, naming the
# assertion or the command (lib.sh).
#
# Where the registry deletes no manifests, LAJSTROM_E2E_DELETE_MANIFEST=false (run.sh reads it
# from the server's log, #800), the maintainer's npm unpublish of the version and of the scoped
# package are refused with E501 and change nothing: the reindex then finds the scoped package
# again, and what the run published stays in the registry.
set -euo pipefail
. "$(dirname "$0")/lib.sh"
umask 077

base="${1:-http://localhost:8080}"
base="${base%/}"
for tool in curl jq docker; do
	command -v "$tool" >/dev/null || { echo "npm.sh: $tool is required" >&2; exit 2; }
done
image="${NPM_E2E_IMAGE:-node:26.13.0-slim}"
pnpm_version="${NPM_E2E_PNPM:-10.34.9}"
yarn_version="${NPM_E2E_YARN:-4.18.1}"
upstream="${NPM_E2E_UPSTREAM-npmjs}"
public="${NPM_E2E_PUBLIC:-chalk@4.1.2}"
hidden="${NPM_E2E_HIDDEN:-is-number}" # a name of the upstream that the hosted repo takes over
vulnerable="${NPM_E2E_VULNERABLE:-minimist@1.2.0}" # a version the upstream has advisories for
registry="${NPM_E2E_REGISTRY:-local}"
ids="$(id -u):$(id -g)"
hostport="${base#*://}"
host="${hostport%%:*}"

work="$(mktemp -d)"
e2e_credentials "$work"
cp "$E2E_CURLRC" "$work/admin"
run="$(date -u +%Y%m%d%H%M%S)-$RANDOM"
repo="npm-$run"
proxy="npmjs-$run"
user="e2e-npm-$run"
scope="@e2e-$run"
plain="e2e-npm-$run"         # npm's unscoped package
scoped="$scope/npm-lib"      # npm's scoped package
pnpm_plain="e2e-pnpm-$run"   # pnpm's
pnpm_scoped="$scope/pnpm-lib" # pnpm's, which depends on $plain
yarn_plain="e2e-yarn-$run"   # yarn's
yarn_scoped="$scope/yarn-lib"
group="all-$run"
npm_app="$scope/npm-app"     # npm's, published through the group; each depends on $public
pnpm_app="$scope/pnpm-app"   # pnpm's
yarn_app="$scope/yarn-app"   # yarn's
url="$base/npm/$repo"
group_url="$base/npm/$group"
created=""
proxy_created=""
group_created=""
token_id=""
cleanup() {
	if [ -n "$token_id" ]; then
		curl -sS -o /dev/null -K "$work/admin" -X DELETE "$base/api/v1/repos/npm/$repo/deploy-tokens/$token_id" || true
	fi
	if [ -n "$group_created" ]; then # first: a member of a group cannot be deleted
		curl -sS -o /dev/null -K "$work/admin" -X DELETE "$base/api/v1/repos/npm/$group" || true
	fi
	if [ -n "$created" ]; then
		curl -sS -o /dev/null -K "$work/admin" -X DELETE "$base/api/v1/repos/npm/$repo?force=true" || true
	fi
	if [ -n "$proxy_created" ]; then
		curl -sS -o /dev/null -K "$work/admin" -X DELETE "$base/api/v1/repos/npm/$proxy?force=true" || true
	fi
	curl -sS -o /dev/null -K "$work/admin" -X PATCH "$base/api/v1/users/$user" -H 'Content-Type: application/json' -d '{"disabled": true}' || true
	rm -rf "$work"
}
trap cleanup EXIT
status="" # status of the last request

# req CRED METHOD URL [curl args...]: one request with the curl config $work/CRED, or none when
# CRED is -. The status goes to $status, the body to $work/body.
req() {
	local cred="$1" method="$2" url="$3"
	shift 3
	local how=(-X "$method")
	[ "$cred" = - ] || how+=(-K "$work/$cred")
	status="$(curl -sS "${how[@]}" -o "$work/body" -w '%{http_code}' "$@" "$url")"
}

# api METHOD PATH [BODY] WANT: a request of the admin to /api/v1/PATH, which must answer WANT.
api() {
	local method="$1" path="$2" body="$3" want="$4"
	if [ -n "$body" ]; then
		req admin "$method" "$base/api/v1/$path" -H 'Content-Type: application/json' -d "$body"
	else
		req admin "$method" "$base/api/v1/$path"
	fi
	expect "$method /api/v1/$path ($(head -c 300 "$work/body"))" "$want" "$status"
}

# enc NAME prints NAME as a path segment of the npm routes: the scope's / as %2f.
enc() { printf '%s' "${1/\//%2f}"; }

# packument NAME: the full packument of NAME in the repo into $work/packument.json, read with
# the deploy token; its status goes to $status.
packument() {
	req deploy GET "$url/$(enc "$1")"
	cp "$work/body" "$work/packument.json"
}

# tags NAME: the packument of NAME, which must be there, into $work/packument.json; its
# dist-tags, sorted, "<tag>=<version>" on one line, into $dist_tags and its versions into
# $versions.
tags() {
	packument "$1"
	expect "GET /npm/$repo/$(enc "$1") ($(head -c 300 "$work/body"))" 200 "$status"
	dist_tags="$(jq -r '."dist-tags" | to_entries | map("\(.key)=\(.value)") | sort | join(" ")' "$work/packument.json")"
	versions="$(jq -r '.versions | keys | join(" ")' "$work/packument.json")"
}

# try NAME LOG SCRIPT: SCRIPT with bash in $image as the caller, with HOME /w/NAME, the tools on
# PATH and the run's directory as /w; the output goes to $work/LOG, which must hold no secret,
# and the status to $rc.
try() {
	local name="$1" log="$2" script="$3"
	mkdir -p "$work/$name"
	rc=0
	docker run --rm --network host -u "$ids" -e HOME="/w/$name" -e PATH=/w/tools/bin:/usr/local/bin:/usr/bin:/bin \
		-e npm_config_update_notifier=false -e npm_config_fund=false -e npm_config_audit=false -e YARN_ENABLE_TELEMETRY=0 \
		-v "$work:/w" -w /w "$image" bash -c ". /w/answer.sh; set -euo pipefail; $script" >"$work/$log" 2>&1 || rc=$?
	no_secret "$work/$log"
}

# client NAME LOG SCRIPT: try, and a failure fails the scenario with the last lines of LOG.
client() {
	try "$@"
	if [ "$rc" != 0 ]; then
		{ echo "--- the last lines of $2:"; tail -n 30 "$work/$2"; } >&2
		fail "$1: $3: exit $rc"
	fi
}

# no_secret FILE: fail when FILE holds the password or a token of the run.
no_secret() {
	local s
	for s in "$work/password" "$work/token" "$work/npm-token" "$work/yarn-token" "$work/group-token" \
		"$work/npm-group-token" "$work/yarn-group-token"; do
		[ -s "$s" ] || continue
		! grep -qF -f "$s" "$1" || fail "$(basename "$1") holds the secret of $(basename "$s")"
	done
}

# package DIR NAME VERSION [DEPENDENCY RANGE]: a package in $work/DIR whose main module exports
# "NAME@VERSION", followed by " with DEPENDENCY@<the version installed>".
package() {
	local dir="$work/$1" name="$2" version="$3" dep="${4:-}" range="${5:-}"
	mkdir -p "$dir"
	if [ -n "$dep" ]; then
		jq -n --arg n "$name" --arg v "$version" --arg d "$dep" --arg r "$range" \
			'{name: $n, version: $v, main: "index.js", license: "MIT", dependencies: {($d): $r}}' >"$dir/package.json"
		printf 'module.exports = "%s@%s with %s@" + require("%s/package.json").version;\n' "$name" "$version" "$dep" "$dep" >"$dir/index.js"
	else
		jq -n --arg n "$name" --arg v "$version" '{name: $n, version: $v, main: "index.js", license: "MIT"}' >"$dir/package.json"
		printf 'module.exports = "%s@%s";\n' "$name" "$version" >"$dir/index.js"
	fi
}

# project DIR: an empty project in $work/DIR, a package.json and nothing else.
project() {
	mkdir -p "$work/$1"
	printf '{"name": "%s", "version": "1.0.0", "private": true}\n' "$1" >"$work/$1/package.json"
}

# required DIR NAME...: what require() of each NAME exports in the project /w/DIR, joined by
# spaces, into $exports.
required() {
	local dir="$1"
	shift
	docker run --rm -u "$ids" -v "$work:/w" -w "/w/$dir" "$image" \
		node -e 'for (const n of process.argv.slice(1)) console.log(require(n))' "$@" >"$work/required.log" 2>&1 ||
		fail "require() in $dir of $*: $(tail -n 5 "$work/required.log")"
	exports="$(tr '\n' ' ' <"$work/required.log" | sed 's/ $//')"
}

# snapshot FILE: what the hosted repo answers for each of $names, read as the admin, one line of
# JSON per name: its dist-tags and each version's deprecation and integrity, or the status.
snapshot() {
	local n
	: >"$1"
	for n in "${names[@]}"; do
		req admin GET "$url/$(enc "$n")"
		if [ "$status" = 200 ]; then
			jq -cS --arg n "$n" '{name: $n, "dist-tags": ."dist-tags", versions: (.versions | map_values({deprecated, integrity: .dist.integrity}))}' "$work/body" >>"$1"
		else
			jq -cn --arg n "$n" --arg s "$status" '{name: $n, status: $s}' >>"$1"
		fi
	done
}

# The login of the user: its password in $work/login for answer.sh, never on a command line.
password="$(head -c 24 /dev/urandom | base64 | tr -d '+/=\n')"
printf '%s' "$password" >"$work/password"
printf '%s\n%s\n' "$user" "$password" >"$work/login"
# answer CMD, in the clients: CMD in a pseudo-terminal (script(1)), which npm login and yarn npm
# login need, typing the user at the prompt "Username" and the password at "Password".
cat >"$work/answer.sh" <<'EOF'
answer() {
	local dir user password i rc=0
	dir="$(mktemp -d)"
	mkfifo "$dir/in"
	script -qfec "$1" /dev/null <"$dir/in" >"$dir/out" 2>&1 &
	exec 3>"$dir/in"
	{ read -r user; read -r password; } </w/login
	for i in $(seq 1 300); do grep -q Username "$dir/out" && break; sleep 0.1; done
	printf '%s\n' "$user" >&3
	for i in $(seq 1 300); do grep -q Password "$dir/out" && break; sleep 0.1; done
	printf '%s\n' "$password" >&3
	wait $! || rc=$?
	exec 3>&-
	tr -d '\r' <"$dir/out" | sed 's/\x1b\[[0-9;?]*[A-Za-z]//g'
	rm -rf "$dir"
	return $rc
}
EOF

# --- The repos, the user and the deploy token, through the API --------------------------------
api POST repos "{\"format\":\"npm\",\"name\":\"$repo\",\"registry\":\"$registry\"}" 201
created=yes
step "POST /api/v1/repos npm/$repo on the registry $registry: 201"
jq -n --arg n "$user" --rawfile p "$work/password" '{name: $n, password: $p}' >"$work/user.json"
api POST users "@$work/user.json" 201
api POST bindings "{\"principal\":\"user:$user\",\"role\":\"writer\",\"repoMask\":\"npm/$repo\"}" 201
step "the user $user, writer on npm/$repo"
api POST "repos/npm/$repo/deploy-tokens" "{\"name\":\"npm e2e $run\",\"scopes\":[\"read\",\"write\"]}" 201
jq -r .secret "$work/body" >"$work/token"
token_id="$(jq -r .id "$work/body")"
rm -f "$work/body"
printf 'header = "Authorization: Bearer %s"\n' "$(cat "$work/token")" >"$work/deploy"
step "a deploy token of npm/$repo with read and write: token $token_id"
if [ -n "$upstream" ]; then
	req admin POST "$base/api/v1/repos" -H 'Content-Type: application/json' \
		-d "{\"format\":\"npm\",\"name\":\"$proxy\",\"registry\":\"$registry\",\"kind\":\"proxy\",\"upstream\":{\"name\":\"$upstream\"}}"
	expect "POST /api/v1/repos npm/$proxy, a proxy of the upstream $upstream, which the server's configuration must name in upstreams[] (run.sh does; NPM_E2E_UPSTREAM= leaves the proxy out) ($(head -c 300 "$work/body"))" 201 "$status"
	proxy_created=yes
	api POST bindings "{\"principal\":\"user:$user\",\"role\":\"reader\",\"repoMask\":\"npm/$proxy\"}" 201
	step "POST /api/v1/repos npm/$proxy, a proxy of the upstream $upstream: 201; $user reader on it"
fi

# --- The clients --------------------------------------------------------------------------------
t0=$SECONDS
client tools tools.log "npm install -g --prefix /w/tools --loglevel=warn pnpm@$pnpm_version @yarnpkg/cli-dist@$yarn_version"
client tools versions.log 'echo "node $(node --version), npm $(npm --version), pnpm $(pnpm --version), yarn $(yarn --version)"'
step "$(cat "$work/versions.log") in $image; pnpm and yarn from registry.npmjs.org in $((SECONDS - t0)) s"

# --- npm: login, whoami, publish, install, dist-tag, deprecate ----------------------------------
mkdir -p "$work/npm"
printf 'registry=%s/\n' "$url" >"$work/npm/.npmrc"
client npm npm-login.log "answer 'npm login --registry=$url/'"
grep -q "Logged in on $url/" "$work/npm-login.log" || fail "npm login: $(tail -n 5 "$work/npm-login.log")"
sed -n "s|^//$hostport/npm/$repo/:_authToken=||p" "$work/npm/.npmrc" >"$work/npm-token"
[ -s "$work/npm-token" ] || fail "npm login wrote no _authToken for //$hostport/npm/$repo/ into .npmrc"
api GET "users/$user/tokens" "" 200
expect "the tokens of $user after npm login" "npm login npm/$repo" \
	"$(jq -r '[.tokens[] | select(.revokedAt == null) | .name | split(" ")[0:3] | join(" ")] | join(",")' "$work/body")"
client npm npm-whoami.log 'npm whoami'
expect "npm whoami" "$user" "$(tail -n 1 "$work/npm-whoami.log")"
step "J-01 npm login with the password: a personal access token in .npmrc, listed for $user; npm whoami: $user"

package pkg/plain "$plain" 1.0.0
package pkg/scoped "$scoped" 1.0.0
client npm npm-publish.log "cd /w/pkg/plain && npm publish && cd /w/pkg/scoped && npm publish"
package pkg/plain "$plain" 1.1.0
client npm npm-publish.log "cd /w/pkg/plain && npm publish"
package pkg/plain "$plain" 2.0.0-beta.1
client npm npm-publish.log "cd /w/pkg/plain && npm publish --tag beta"
tags "$plain"
expect "dist-tags of $plain" "beta=2.0.0-beta.1 latest=1.1.0" "$dist_tags"
expect "versions of $plain" "1.0.0 1.1.0 2.0.0-beta.1" "$versions"
tags "$scoped"
expect "dist-tags of $scoped" "latest=1.0.0" "$dist_tags"
step "J-02 npm publish of $plain 1.0.0, 1.1.0 and 2.0.0-beta.1 --tag beta, and of $scoped 1.0.0"

project npm-install
client npm npm-install.log "cd /w/npm-install && npm install --cache /w/npm-cache-1 --loglevel=warn $plain $scoped"
required npm-install "$plain" "$scoped"
expect "require() after npm install" "$plain@1.1.0 $scoped@1.0.0" "$exports"
step "J-02 npm install of $plain and $scoped into an empty project with an empty cache: 1.1.0 and 1.0.0"

client npm npm-tag.log "npm dist-tag add $plain@1.0.0 stable && npm dist-tag ls $plain"
grep -qx 'stable: 1.0.0' "$work/npm-tag.log" || fail "npm dist-tag ls $plain: $(cat "$work/npm-tag.log")"
tags "$plain"
expect "dist-tags of $plain after npm dist-tag add" "beta=2.0.0-beta.1 latest=1.1.0 stable=1.0.0" "$dist_tags"
project npm-stable
client npm npm-install.log "cd /w/npm-stable && npm install --cache /w/npm-cache-2 --loglevel=warn $plain@stable"
required npm-stable "$plain"
expect "require() after npm install $plain@stable" "$plain@1.0.0" "$exports"
client npm npm-tag.log "npm dist-tag rm $plain stable && npm dist-tag ls $plain"
! grep -q '^stable:' "$work/npm-tag.log" || fail "npm dist-tag ls $plain after rm: $(cat "$work/npm-tag.log")"
tags "$plain"
expect "dist-tags of $plain after npm dist-tag rm" "beta=2.0.0-beta.1 latest=1.1.0" "$dist_tags"
step "J-03 npm dist-tag add $plain@1.0.0 stable, ls, npm install $plain@stable (1.0.0), rm"

message="use 1.1.0 ($run)"
client npm npm-deprecate.log "npm deprecate $plain@1.0.0 '$message'"
tags "$plain"
expect "versions[1.0.0].deprecated of $plain" "$message" "$(jq -r '.versions["1.0.0"].deprecated // "-"' "$work/packument.json")"
expect "versions[1.1.0].deprecated of $plain" "-" "$(jq -r '.versions["1.1.0"].deprecated // "-"' "$work/packument.json")"
project npm-deprecated
client npm npm-install.log "cd /w/npm-deprecated && npm install --cache /w/npm-cache-3 $plain@1.0.0"
grep -qF "deprecated $plain@1.0.0: $message" "$work/npm-install.log" || fail "npm install $plain@1.0.0 did not warn: $(cat "$work/npm-install.log")"
client npm npm-deprecate.log "npm deprecate $plain@1.0.0 ''"
tags "$plain"
expect "versions[1.0.0].deprecated of $plain after npm deprecate ''" "-" "$(jq -r '.versions["1.0.0"].deprecated // "-"' "$work/packument.json")"
step "J-03 npm deprecate $plain@1.0.0: the message in the packument and in npm install's warning; cleared with ''"

# --- pnpm: the deploy token as _authToken -------------------------------------------------------
mkdir -p "$work/pnpm"
printf 'registry=%s/\n//%s/npm/%s/:_authToken=%s\n' "$url" "$hostport" "$repo" "$(cat "$work/token")" >"$work/pnpm/.npmrc"
client pnpm pnpm-whoami.log 'pnpm whoami'
expect "pnpm whoami with the deploy token" "token:$token_id" "$(tail -n 1 "$work/pnpm-whoami.log")"
package pkg/pnpm-plain "$pnpm_plain" 1.0.0
package pkg/pnpm-scoped "$pnpm_scoped" 1.0.0 "$plain" '^1.0.0'
client pnpm pnpm-publish.log "cd /w/pkg/pnpm-plain && pnpm publish --no-git-checks && cd /w/pkg/pnpm-scoped && pnpm publish --no-git-checks"
tags "$pnpm_scoped"
expect "dist-tags of $pnpm_scoped" "latest=1.0.0" "$dist_tags"
expect "dependencies of $pnpm_scoped 1.0.0" "{\"$plain\":\"^1.0.0\"}" "$(jq -c '.versions["1.0.0"].dependencies' "$work/packument.json")"
project pnpm-install
client pnpm pnpm-install.log "cd /w/pnpm-install && pnpm add --store-dir /w/pnpm-store $pnpm_plain $pnpm_scoped $scoped"
required pnpm-install "$pnpm_plain" "$pnpm_scoped" "$scoped"
expect "require() after pnpm add" "$pnpm_plain@1.0.0 $pnpm_scoped@1.0.0 with $plain@1.1.0 $scoped@1.0.0" "$exports"
step "J-04 pnpm with the deploy token: whoami token:$token_id; publish of $pnpm_plain and $pnpm_scoped; pnpm add from an empty store, $plain@1.1.0 for ^1.0.0"

# --- yarn 4: npm login, whoami, publish, tag, install -------------------------------------------
mkdir -p "$work/yarn"
cat >"$work/yarn/.yarnrc.yml" <<EOF
npmRegistryServer: "$url"
npmMinimalAgeGate: 0
npmRegistries:
  "$url":
    npmAlwaysAuth: true
unsafeHttpWhitelist: ["$host"]
EOF
project yarn-login
client yarn yarn-login.log "cd /w/yarn-login && answer 'yarn npm login'"
grep -q 'Successfully logged in' "$work/yarn-login.log" || fail "yarn npm login: $(tail -n 5 "$work/yarn-login.log")"
awk '/npmAuthToken:/ { print $2 }' "$work/yarn/.yarnrc.yml" | tr -d '"' >"$work/yarn-token"
[ -s "$work/yarn-token" ] || fail "yarn npm login wrote no npmAuthToken into .yarnrc.yml"
client yarn yarn-whoami.log 'cd /w/yarn-login && yarn npm whoami'
grep -q "YN0000: $user\$" "$work/yarn-whoami.log" || fail "yarn npm whoami: $(cat "$work/yarn-whoami.log")"
step "J-05 yarn npm login with the password: a token in .yarnrc.yml; yarn npm whoami: $user"

package pkg/yarn-plain "$yarn_plain" 1.0.0
package pkg/yarn-scoped "$yarn_scoped" 1.0.0
client yarn yarn-publish.log "cd /w/pkg/yarn-plain && yarn install && yarn npm publish && cd /w/pkg/yarn-scoped && yarn install && yarn npm publish"
tags "$yarn_plain"
expect "dist-tags of $yarn_plain" "latest=1.0.0" "$dist_tags"
tags "$yarn_scoped"
expect "dist-tags of $yarn_scoped" "latest=1.0.0" "$dist_tags"
client yarn yarn-tag.log "cd /w/pkg/yarn-scoped && yarn npm tag add $yarn_scoped@1.0.0 stable && yarn npm tag list $yarn_scoped"
grep -q 'stable.*1\.0\.0' "$work/yarn-tag.log" || fail "yarn npm tag list $yarn_scoped: $(cat "$work/yarn-tag.log")"
tags "$yarn_scoped"
expect "dist-tags of $yarn_scoped after yarn npm tag add" "latest=1.0.0 stable=1.0.0" "$dist_tags"
client yarn yarn-tag.log "cd /w/pkg/yarn-scoped && yarn npm tag remove $yarn_scoped stable"
tags "$yarn_scoped"
expect "dist-tags of $yarn_scoped after yarn npm tag remove" "latest=1.0.0" "$dist_tags"
step "J-05 yarn npm publish of $yarn_plain and $yarn_scoped; yarn npm tag add, list and remove"

project yarn-install
printf 'nodeLinker: node-modules\nenableGlobalCache: false\n' >"$work/yarn-install/.yarnrc.yml"
touch "$work/yarn-install/yarn.lock"
client yarn yarn-install.log "cd /w/yarn-install && yarn add $yarn_plain $yarn_scoped $pnpm_scoped"
required yarn-install "$yarn_plain" "$yarn_scoped" "$pnpm_scoped"
expect "require() after yarn add" "$yarn_plain@1.0.0 $yarn_scoped@1.0.0 $pnpm_scoped@1.0.0 with $plain@1.1.0" "$exports"
step "J-05 yarn add of $yarn_plain, $yarn_scoped and $pnpm_scoped into an empty project with an empty cache"

# --- The proxy of npmjs.org ---------------------------------------------------------------------
if [ -n "$upstream" ]; then
	fetched=0
	for i in 1 2; do
		project "proxy-$i"
		printf 'registry=%s/\n//%s/npm/%s/:_authToken=%s\n' "$base/npm/$proxy" "$hostport" "$proxy" "$(cat "$work/npm-token")" >"$work/proxy-$i/.npmrc"
		t0=$SECONDS
		client npm "proxy-$i.log" "cd /w/proxy-$i && npm install --cache /w/proxy-cache-$i --loglevel=warn $public && npm ls --all --parseable --long | tail -n +2 | cut -d: -f2 | sort -u | wc -l"
		took=$((SECONDS - t0))
		installed="$(tail -n 1 "$work/proxy-$i.log" | tr -d ' ')"
		[ "$installed" -gt 0 ] || fail "npm install $public through npm/$proxy installed nothing" # name@version of the tree
		before="$fetched"
		api GET "audit?repo=npm/$proxy&action=proxy.fetch&limit=1000" "" 200
		fetched="$(jq '.events | length' "$work/body")"
		if [ "$i" = 1 ]; then
			expect "proxy.fetch rows of npm/$proxy after the first install of $public: one per package" "$installed" "$fetched"
			step "J-06 npm install of $public through the proxy npm/$proxy, empty cache: $installed packages in $took s, each tarball fetched once from $upstream"
		else
			expect "proxy.fetch rows of npm/$proxy after the second install of $public" "$before" "$fetched"
			step "J-06 npm install of $public through npm/$proxy again, empty cache: $installed packages in $took s, none fetched"
		fi
	done
	name="${public%@*}"
	service="$(curl -sS -o /dev/null -D - "$base/v2/" | tr -d '\r' | sed -nE 's/^[Ww][Ww][Ww]-[Aa]uthenticate: .*service="([^"]*)".*/\1/p')"
	req admin GET "$base/v2/token?service=$service&scope=repository:npm/$proxy/$name:pull"
	expect "GET /v2/token for npm/$proxy/$name" 200 "$status"
	printf 'header = "Authorization: Bearer %s"\n' "$(jq -r .token "$work/body")" >"$work/v2"
	req v2 GET "$base/v2/npm/$proxy/$name/tags/list"
	expect "GET /v2/npm/$proxy/$name/tags/list ($(head -c 300 "$work/body"))" 200 "$status"
	jq -e --arg v "${public##*@}" '.tags | index($v)' "$work/body" >/dev/null || fail "no tag ${public##*@} in npm/$proxy/$name: $(cat "$work/body")"
	step "J-06 $public is in the registry: /v2/npm/$proxy/$name/tags/list $(jq -c .tags "$work/body")"
else
	echo "skip the proxy: NPM_E2E_UPSTREAM is empty"
fi

# --- The group: one registry line for the hosted repo and the proxy (the M4 demo) ---------------
if [ -n "$upstream" ]; then
	members="[\"$repo\",\"$proxy\"]"
	dep="${public%@*}" range="${public##*@}" with=" with $public"
else
	members="[\"$repo\"]"
	dep="" range="" with=""
fi
api POST repos "{\"format\":\"npm\",\"name\":\"$group\",\"kind\":\"group\",\"group\":{\"members\":$members}}" 201
group_created=yes
api POST bindings "{\"principal\":\"user:$user\",\"role\":\"reader\",\"repoMask\":\"npm/$group\"}" 201
api POST "repos/npm/$group/deploy-tokens" "{\"name\":\"npm e2e $run\",\"scopes\":[\"read\",\"write\"]}" 400
api POST "repos/npm/$group/deploy-tokens" "{\"name\":\"npm e2e $run\",\"scopes\":[\"read\"]}" 201
jq -r .secret "$work/body" >"$work/group-token"
group_token_id="$(jq -r .id "$work/body")"
rm -f "$work/body"
printf 'header = "Authorization: Bearer %s"\n' "$(cat "$work/group-token")" >"$work/group"
step "J-07 POST /api/v1/repos npm/$group, a group of $members: 201; $user reader on it; its deploy token $group_token_id reads only (write: 400)"

# npm: login on the group, a publish through it, which lands in the hosted repo, and an install.
mkdir -p "$work/npm-group"
printf 'registry=%s/\n' "$group_url" >"$work/npm-group/.npmrc"
client npm-group npm-group-login.log "answer 'npm login --registry=$group_url/'"
grep -q "Logged in on $group_url/" "$work/npm-group-login.log" || fail "npm login on npm/$group: $(tail -n 5 "$work/npm-group-login.log")"
sed -n "s|^//$hostport/npm/$group/:_authToken=||p" "$work/npm-group/.npmrc" >"$work/npm-group-token"
[ -s "$work/npm-group-token" ] || fail "npm login wrote no _authToken for //$hostport/npm/$group/ into .npmrc"
package pkg/npm-app "$npm_app" 1.0.0 "$dep" "$range"
project group-npm
client npm-group group-npm.log "cd /w/pkg/npm-app && npm publish && cd /w/group-npm && npm install --cache /w/group-cache-npm --loglevel=warn $npm_app"
required group-npm "$npm_app"
expect "require() after npm install through npm/$group" "$npm_app@1.0.0$with" "$exports"
step "J-07 npm login on npm/$group, registry=$group_url/ the one registry line; npm publish of $npm_app through it; npm install from an empty cache: $exports"

# pnpm: npm's token of the group to publish, the group's deploy token in the project to install.
mkdir -p "$work/pnpm-group"
printf 'registry=%s/\n//%s/npm/%s/:_authToken=%s\n' "$group_url" "$hostport" "$group" "$(cat "$work/npm-group-token")" >"$work/pnpm-group/.npmrc"
package pkg/pnpm-app "$pnpm_app" 1.0.0 "$dep" "$range"
project group-pnpm
printf 'registry=%s/\n//%s/npm/%s/:_authToken=%s\n' "$group_url" "$hostport" "$group" "$(cat "$work/group-token")" >"$work/group-pnpm/.npmrc"
client pnpm-group group-pnpm.log "cd /w/pkg/pnpm-app && pnpm publish --no-git-checks && cd /w/group-pnpm && pnpm whoami >/w/group-pnpm-whoami.log && pnpm add --store-dir /w/group-store $pnpm_app $npm_app"
expect "pnpm whoami in the project with the group's deploy token" "token:$group_token_id" "$(tail -n 1 "$work/group-pnpm-whoami.log")"
required group-pnpm "$pnpm_app" "$npm_app"
expect "require() after pnpm add through npm/$group" "$pnpm_app@1.0.0$with $npm_app@1.0.0$with" "$exports"
step "J-07 pnpm publish of $pnpm_app through npm/$group; pnpm add from an empty store with the group's deploy token (whoami token:$group_token_id): $exports"

# yarn: yarn npm login on the group, a publish through it and an install of the three.
mkdir -p "$work/yarn-group"
cat >"$work/yarn-group/.yarnrc.yml" <<YARNRC
npmRegistryServer: "$group_url"
npmMinimalAgeGate: 0
npmRegistries:
  "$group_url":
    npmAlwaysAuth: true
unsafeHttpWhitelist: ["$host"]
YARNRC
project group-yarn-login
client yarn-group group-yarn-login.log "cd /w/group-yarn-login && answer 'yarn npm login'"
grep -q 'Successfully logged in' "$work/group-yarn-login.log" || fail "yarn npm login on npm/$group: $(tail -n 5 "$work/group-yarn-login.log")"
awk '/npmAuthToken:/ { print $2 }' "$work/yarn-group/.yarnrc.yml" | tr -d '"' >"$work/yarn-group-token"
[ -s "$work/yarn-group-token" ] || fail "yarn npm login on npm/$group wrote no npmAuthToken into .yarnrc.yml"
package pkg/yarn-app "$yarn_app" 1.0.0 "$dep" "$range"
project group-yarn
printf 'nodeLinker: node-modules\nenableGlobalCache: false\n' >"$work/group-yarn/.yarnrc.yml"
touch "$work/group-yarn/yarn.lock"
client yarn-group group-yarn.log "cd /w/pkg/yarn-app && yarn install && yarn npm publish && cd /w/group-yarn && yarn add $yarn_app $pnpm_app $npm_app"
required group-yarn "$yarn_app" "$pnpm_app" "$npm_app"
expect "require() after yarn add through npm/$group" "$yarn_app@1.0.0$with $pnpm_app@1.0.0$with $npm_app@1.0.0$with" "$exports"
step "J-07 yarn npm login on npm/$group; yarn npm publish of $yarn_app through it; yarn add of the three from an empty cache"

# What the group answered, and where the writes went.
for n in "$npm_app" "$pnpm_app" "$yarn_app"; do
	tags "$n"
	expect "dist-tags of $n in npm/$repo, published through npm/$group" "latest=1.0.0" "$dist_tags"
done
api GET "audit?repo=npm/$repo&action=push&limit=1000" "" 200
expect "push rows of npm/$repo with details.via npm/$group" 3 "$(jq --arg g "npm/$group" '[.events[] | select(.details.via == $g)] | length' "$work/body")"
links=("$npm_app")
[ -z "$upstream" ] || links+=("${public%@*}")
for n in "${links[@]}"; do
	req group GET "$group_url/$(enc "$n")"
	expect "GET /npm/$group/$(enc "$n") with the group's deploy token ($(head -c 300 "$work/body"))" 200 "$status"
	jq -e --arg g "$group_url/" '[.versions[].dist.tarball | startswith($g)] | all' "$work/body" >/dev/null ||
		fail "a dist.tarball of $n through npm/$group names another URL: $(jq -c '[.versions[].dist.tarball][0:3]' "$work/body")"
done
if [ -n "$upstream" ]; then
	api GET "audit?repo=npm/$proxy&action=proxy.fetch&limit=1000" "" 200
	expect "proxy.fetch rows of npm/$proxy after the installs through npm/$group: those of the proxy's installs" "$fetched" "$(jq '.events | length' "$work/body")"
fi
step "J-07 the three packages in npm/$repo, each push with details.via npm/$group; every dist.tarball of ${links[*]} under $group_url/${upstream:+; nothing fetched again from $upstream}"

# A name of the upstream that the hosted repo holds hides the upstream's versions of it, but for
# the names of group.merge.
if [ -n "$upstream" ]; then
	package pkg/hidden "$hidden" 99.0.0
	client npm npm-hidden.log "cd /w/pkg/hidden && npm publish"
	req admin GET "$base/npm/$proxy/$hidden"
	expect "GET /npm/$proxy/$hidden" 200 "$status"
	upstream_versions="$(jq '.versions | length' "$work/body")"
	[ "$upstream_versions" -gt 1 ] || fail "$upstream has $upstream_versions versions of $hidden"
	req group GET "$group_url/$hidden"
	expect "GET /npm/$group/$hidden" 200 "$status"
	expect "versions of $hidden through npm/$group, which npm/$repo holds" "99.0.0" "$(jq -r '.versions | keys | join(" ")' "$work/body")"
	api PATCH "repos/npm/$group" "{\"group\":{\"merge\":[\"$hidden\"]}}" 200
	req group GET "$group_url/$hidden"
	expect "the number of versions of $hidden through npm/$group with group.merge" "$((upstream_versions + 1))" "$(jq '.versions | length' "$work/body")"
	expect "dist-tags.latest of $hidden through npm/$group with group.merge, the hosted repo's" 99.0.0 "$(jq -r '."dist-tags".latest' "$work/body")"
	step "J-08 npm publish of $hidden 99.0.0 to npm/$repo: through npm/$group only 99.0.0, of $upstream's $upstream_versions versions none; with group.merge [$hidden] all $((upstream_versions + 1))"
fi

# --- npm audit through the proxy: what deny refuses stays on the server (#514) ------------------
# A project of $vulnerable from the proxy and $yarn_scoped from the hosted repo, registry= the
# proxy: npm audit sends both names there. Without a deny list the upstream's advisories for
# $vulnerable come back, so its name went out; with deny [$scope/*, <its name>] none come back.
if [ -n "$upstream" ]; then
	project audit
	printf 'registry=%s/\n//%s/npm/%s/:_authToken=%s\n%s:registry=%s/\n//%s/npm/%s/:_authToken=%s\n' \
		"$base/npm/$proxy" "$hostport" "$proxy" "$(cat "$work/npm-token")" "$scope" "$url" "$hostport" "$repo" "$(cat "$work/npm-token")" >"$work/audit/.npmrc"
	client npm audit-install.log "cd /w/audit && npm install --cache /w/audit-cache --loglevel=warn $vulnerable $yarn_scoped"
	try npm audit-open.log "cd /w/audit && npm audit"
	expect "npm audit through npm/$proxy without a deny list ($(tail -n 3 "$work/audit-open.log" | tr '\n' ' '))" 1 "$rc"
	grep -q "^${vulnerable%@*} " "$work/audit-open.log" || fail "npm audit through npm/$proxy names no advisory of $vulnerable: $(cat "$work/audit-open.log")"
	api PATCH "repos/npm/$proxy" "{\"upstream\":{\"deny\":[\"$scope/*\",\"${vulnerable%@*}\"]}}" 200
	try npm audit-denied.log "cd /w/audit && npm audit"
	expect "npm audit through npm/$proxy with deny [$scope/*, ${vulnerable%@*}] ($(cat "$work/audit-denied.log"))" 0 "$rc"
	grep -qx 'found 0 vulnerabilities' "$work/audit-denied.log" || fail "npm audit with the names denied: $(cat "$work/audit-denied.log")"
	step "J-09 npm audit through npm/$proxy of $vulnerable and $yarn_scoped: $upstream's advisories of ${vulnerable%@*}; with deny [$scope/*, ${vulnerable%@*}] none, both names kept on the server"
fi

# --- npm: unpublish ----------------------------------------------------------------------------
try npm npm-unpublish.log "npm unpublish $plain@2.0.0-beta.1"
expect "npm unpublish as a writer (rbac.md §2: delete)" 1 "$rc"
grep -q 'E403' "$work/npm-unpublish.log" || fail "npm unpublish as a writer: $(tail -n 5 "$work/npm-unpublish.log")"
tags "$plain"
expect "versions of $plain after the refused unpublish" "1.0.0 1.1.0 2.0.0-beta.1" "$versions"
api POST bindings "{\"principal\":\"user:$user\",\"role\":\"maintainer\",\"repoMask\":\"npm/$repo\"}" 201
step "J-10 npm unpublish as a writer: E403, nothing deleted; $user made maintainer of npm/$repo"
deletes=yes # the registry deletes manifests; no: every unpublish is refused, 501, and nothing changes (#352)
[ "${LAJSTROM_E2E_DELETE_MANIFEST:-}" != false ] || deletes=no
if [ "$deletes" = no ]; then
	was="$dist_tags"
	packument "$scoped"
	expect "GET /npm/$repo/$(enc "$scoped") before npm unpublish --force" 200 "$status"
	scoped_was="$(jq -c '{"dist-tags": ."dist-tags", versions: (.versions | keys)}' "$work/packument.json")"
	# npm retries a 5xx twice, 10 and 60 s apart: the refusal is final, so not here.
	try npm npm-unpublish.log "npm unpublish --fetch-retries=0 $plain@2.0.0-beta.1"
	expect "npm unpublish $plain@2.0.0-beta.1 where the registry deletes no manifests" 1 "$rc"
	grep -q 'E501' "$work/npm-unpublish.log" || fail "npm unpublish $plain@2.0.0-beta.1 refused without E501: $(tail -n 5 "$work/npm-unpublish.log")"
	tags "$plain"
	expect "versions of $plain after the refused npm unpublish" "1.0.0 1.1.0 2.0.0-beta.1" "$versions"
	expect "dist-tags of $plain after the refused npm unpublish" "$was" "$dist_tags"
	try npm npm-unpublish.log "npm unpublish --fetch-retries=0 $scoped --force"
	expect "npm unpublish $scoped --force where the registry deletes no manifests" 1 "$rc"
	grep -q 'E501' "$work/npm-unpublish.log" || fail "npm unpublish $scoped --force refused without E501: $(tail -n 5 "$work/npm-unpublish.log")"
	packument "$scoped"
	expect "GET /npm/$repo/$(enc "$scoped") after the refused npm unpublish --force" 200 "$status"
	expect "the dist-tags and versions of $scoped after the refused npm unpublish --force" "$scoped_was" \
		"$(jq -c '{"dist-tags": ."dist-tags", versions: (.versions | keys)}' "$work/packument.json")"
	step "J-10 npm unpublish $plain@2.0.0-beta.1 and npm unpublish $scoped --force where the registry deletes no manifests: E501; every version and dist-tag still there"
else
	client npm npm-unpublish.log "npm unpublish $plain@2.0.0-beta.1"
	tags "$plain"
	expect "versions of $plain after npm unpublish $plain@2.0.0-beta.1" "1.0.0 1.1.0" "$versions"
	expect "dist-tags of $plain after the unpublish" "latest=1.1.0" "$dist_tags"
	client npm npm-unpublish.log "npm unpublish $scoped --force"
	packument "$scoped"
	expect "GET /npm/$repo/$(enc "$scoped") after npm unpublish --force" 404 "$status"
	step "J-10 npm unpublish $plain@2.0.0-beta.1, its tag beta with it; npm unpublish $scoped --force: 404"
fi

# --- Reindex restores the journal ---------------------------------------------------------------
# latest moved below the highest release, a tag and a deprecation, through the group: journal
# entries that the versions' manifests do not say.
client npm-group npm-group-tag.log "npm dist-tag add $plain@1.0.0 latest && npm dist-tag add $plain@1.1.0 next && npm deprecate $plain@1.0.0 '$message'"
tags "$plain"
tagged="latest=1.0.0 next=1.1.0"
[ "$deletes" = yes ] || tagged="beta=2.0.0-beta.1 $tagged" # the refused unpublish left 2.0.0-beta.1 and its tag (J-10)
expect "dist-tags of $plain after npm dist-tag through npm/$group" "$tagged" "$dist_tags"
expect "versions[1.0.0].deprecated of $plain after npm deprecate through npm/$group" "$message" "$(jq -r '.versions["1.0.0"].deprecated // "-"' "$work/packument.json")"
names=("$plain" "$scoped" "$pnpm_plain" "$pnpm_scoped" "$yarn_plain" "$yarn_scoped" "$npm_app" "$pnpm_app" "$yarn_app")
[ -z "$upstream" ] || names+=("$hidden")
snapshot "$work/before.jsonl"
step "J-11 npm dist-tag add $plain@1.0.0 latest and $plain@1.1.0 next, npm deprecate $plain@1.0.0, through npm/$group; ${#names[@]} packuments read"

api DELETE "repos/npm/$repo?force=true" "" 409
api DELETE "repos/npm/$group" "" 204
group_created=""
api DELETE "repos/npm/$repo?force=true" "" 204
token_id="" # its deploy token went with it
api POST repos "{\"format\":\"npm\",\"name\":\"$repo\",\"registry\":\"$registry\"}" 201
req admin GET "$url/$(enc "$plain")"
expect "GET /npm/$repo/$plain in the repo made again" 404 "$status"
api POST "repos/npm/$repo/reindex" "" 202
for _ in $(seq 1 120); do
	api GET "repos/npm/$repo/reindex" "" 200
	[ "$(jq -r .state "$work/body")" = running ] || break
	sleep 0.5
done
expect "the state of the reindex of npm/$repo ($(head -c 300 "$work/body"))" done "$(jq -r .state "$work/body")"
jq -e '.report | .errors == [] and .orphanJournal == [] and .foreign == []' "$work/body" >/dev/null ||
	fail "the report of the reindex of npm/$repo: $(jq -c .report "$work/body")"
report="$(jq -r '.report | "\(.manifests) manifests in \(.ociRepositories) OCI repositories, \(.added) added"' "$work/body")"
snapshot "$work/after.jsonl"
cmp -s "$work/before.jsonl" "$work/after.jsonl" ||
	fail "the packuments of npm/$repo after reindex differ from before: $(diff "$work/before.jsonl" "$work/after.jsonl" | head -n 20)"
step "J-11 DELETE npm/$repo: 409 while a member of npm/$group, then 204; made again, empty; reindex: $report; the ${#names[@]} packuments as before"
if [ "$deletes" = no ]; then
	expect "the status of $scoped in the packuments after the reindex" - "$(grep -F "\"name\":\"$scoped\"" "$work/after.jsonl" | jq -r '.status // "-"')"
	step "J-12 $scoped, whose npm unpublish --force was refused: there after the reindex"
else
	expect "the line of $scoped in the packuments after the reindex" "{\"name\":\"$scoped\",\"status\":\"404\"}" "$(grep -F "\"name\":\"$scoped\"" "$work/after.jsonl")"
	step "J-12 $scoped, unpublished with --force before the reindex: still 404 after it"
fi

# --- npm: logout --------------------------------------------------------------------------------
api GET "users/$user/tokens" "" 200
expect "the live tokens of $user before npm logout, of npm login and yarn npm login on npm/$repo and npm/$group" 4 "$(jq '[.tokens[] | select(.revokedAt == null)] | length' "$work/body")"
client npm npm-logout.log 'npm logout'
! grep -q '_authToken' "$work/npm/.npmrc" || fail "npm logout left the token in .npmrc"
api GET "users/$user/tokens" "" 200
expect "the live tokens of $user after npm logout" 3 "$(jq '[.tokens[] | select(.revokedAt == null)] | length' "$work/body")"
printf 'header = "Authorization: Bearer %s"\n' "$(cat "$work/npm-token")" >"$work/npm-bearer"
req npm-bearer GET "$url/-/whoami"
expect "GET /-/whoami with the token of npm login after npm logout" 401 "$status"
printf 'header = "Authorization: Bearer %s"\n' "$(cat "$work/yarn-token")" >"$work/yarn-bearer"
req yarn-bearer GET "$url/-/whoami"
expect "GET /-/whoami with the token of yarn npm login after npm logout" 200 "$status"
step "J-13 npm logout: its token gone from .npmrc and revoked (401); yarn's still valid"

if [ -n "${LAJSTROM_SERVER_LOG:-}" ] && [ -f "$LAJSTROM_SERVER_LOG" ]; then
	no_secret "$LAJSTROM_SERVER_LOG"
	step "no secret of the run in the server's log"
fi

echo "PASS npm e2e against $base"
