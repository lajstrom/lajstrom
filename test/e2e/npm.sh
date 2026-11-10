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
#   - the group: a marked place for #311, which is not there yet;
#   - npm unpublish refused to the writer (E403: delete is the maintainer's), then, as maintainer,
#     of one version (its dist-tag goes with it) and of the scoped package; npm logout, which
#     revokes npm's token (401) and leaves yarn's.
#
#   test/e2e/npm.sh [<base url>]     default: http://localhost:8080
#
# It runs as $LAJSTROM_E2E_USER, a user with the admin binding (lib.sh, e2e_credentials; run.sh
# makes one), creates the repos on the registry local ($NPM_E2E_REGISTRY names another), and on
# exit revokes the deploy token, deletes both repos (force: the versions leave the index; what
# was not unpublished stays in the registry) and disables its user. NPM_E2E_UPSTREAM= (empty)
# leaves the proxy out, for a server without an npm upstream (the example configuration).
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
url="$base/npm/$repo"
created=""
proxy_created=""
token_id=""
cleanup() {
	if [ -n "$token_id" ]; then
		curl -sS -o /dev/null -K "$work/admin" -X DELETE "$base/api/v1/repos/npm/$repo/deploy-tokens/$token_id" || true
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
	for s in "$work/password" "$work/token" "$work/npm-token" "$work/yarn-token"; do
		[ -s "$s" ] || continue
		! grep -qF -f "$s" "$1" || fail "$(basename "$1") holds the secret of $(basename "$s")"
	done
}

# package DIR NAME VERSION [DEPENDENCY RANGE]: a package in $work/DIR whose main module exports
# "NAME@VERSION", followed by what its dependency exports.
package() {
	local dir="$work/$1" name="$2" version="$3" dep="${4:-}" range="${5:-}"
	mkdir -p "$dir"
	if [ -n "$dep" ]; then
		jq -n --arg n "$name" --arg v "$version" --arg d "$dep" --arg r "$range" \
			'{name: $n, version: $v, main: "index.js", license: "MIT", dependencies: {($d): $r}}' >"$dir/package.json"
		printf 'module.exports = "%s@%s with " + require("%s");\n' "$name" "$version" "$dep" >"$dir/index.js"
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
step "npm login with the password: a personal access token in .npmrc, listed for $user; npm whoami: $user"

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
step "npm publish of $plain 1.0.0, 1.1.0 and 2.0.0-beta.1 --tag beta, and of $scoped 1.0.0"

project npm-install
client npm npm-install.log "cd /w/npm-install && npm install --cache /w/npm-cache-1 --loglevel=warn $plain $scoped"
required npm-install "$plain" "$scoped"
expect "require() after npm install" "$plain@1.1.0 $scoped@1.0.0" "$exports"
step "npm install of $plain and $scoped into an empty project with an empty cache: 1.1.0 and 1.0.0"

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
step "npm dist-tag add $plain@1.0.0 stable, ls, npm install $plain@stable (1.0.0), rm"

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
step "npm deprecate $plain@1.0.0: the message in the packument and in npm install's warning; cleared with ''"

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
step "pnpm with the deploy token: whoami token:$token_id; publish of $pnpm_plain and $pnpm_scoped; pnpm add from an empty store, $plain@1.1.0 for ^1.0.0"

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
step "yarn npm login with the password: a token in .yarnrc.yml; yarn npm whoami: $user"

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
step "yarn npm publish of $yarn_plain and $yarn_scoped; yarn npm tag add, list and remove"

project yarn-install
printf 'nodeLinker: node-modules\nenableGlobalCache: false\n' >"$work/yarn-install/.yarnrc.yml"
touch "$work/yarn-install/yarn.lock"
client yarn yarn-install.log "cd /w/yarn-install && yarn add $yarn_plain $yarn_scoped $pnpm_scoped"
required yarn-install "$yarn_plain" "$yarn_scoped" "$pnpm_scoped"
expect "require() after yarn add" "$yarn_plain@1.0.0 $yarn_scoped@1.0.0 $pnpm_scoped@1.0.0 with $plain@1.1.0" "$exports"
step "yarn add of $yarn_plain, $yarn_scoped and $pnpm_scoped into an empty project with an empty cache"

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
			step "npm install of $public through the proxy npm/$proxy, empty cache: $installed packages in $took s, each tarball fetched once from $upstream"
		else
			expect "proxy.fetch rows of npm/$proxy after the second install of $public" "$before" "$fetched"
			step "npm install of $public through npm/$proxy again, empty cache: $installed packages in $took s, none fetched"
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
	step "$public is in the registry: /v2/npm/$proxy/$name/tags/list $(jq -c .tags "$work/body")"
else
	echo "skip the proxy: NPM_E2E_UPSTREAM is empty"
fi

# --- The group (#311) ---------------------------------------------------------------------------
# TODO(#311): the group step goes here. A group npm/all-<run> of npm/$repo and npm/$proxy,
# created through the API; one registry line (registry=$base/npm/all-<run>/) and a token for it;
# npm, pnpm and yarn install, each from an empty cache, a scoped package of the hosted repo that
# depends on $public (the M4 demo, docs/roadmap.md); and a public name published to the hosted
# repo hides the upstream's versions of that name in the group's packument (#311).
echo "skip the group: group repos are #311"

# --- npm: unpublish, logout ---------------------------------------------------------------------
try npm npm-unpublish.log "npm unpublish $plain@2.0.0-beta.1"
expect "npm unpublish as a writer (rbac.md §2: delete)" 1 "$rc"
grep -q 'E403' "$work/npm-unpublish.log" || fail "npm unpublish as a writer: $(tail -n 5 "$work/npm-unpublish.log")"
tags "$plain"
expect "versions of $plain after the refused unpublish" "1.0.0 1.1.0 2.0.0-beta.1" "$versions"
api POST bindings "{\"principal\":\"user:$user\",\"role\":\"maintainer\",\"repoMask\":\"npm/$repo\"}" 201
step "npm unpublish as a writer: E403, nothing deleted; $user made maintainer of npm/$repo"
client npm npm-unpublish.log "npm unpublish $plain@2.0.0-beta.1"
tags "$plain"
expect "versions of $plain after npm unpublish $plain@2.0.0-beta.1" "1.0.0 1.1.0" "$versions"
expect "dist-tags of $plain after the unpublish" "latest=1.1.0" "$dist_tags"
client npm npm-unpublish.log "npm unpublish $scoped --force"
packument "$scoped"
expect "GET /npm/$repo/$(enc "$scoped") after npm unpublish --force" 404 "$status"
step "npm unpublish $plain@2.0.0-beta.1, its tag beta with it; npm unpublish $scoped --force: 404"

api GET "users/$user/tokens" "" 200
expect "the live tokens of $user before npm logout, of npm login and of yarn npm login" 2 "$(jq '[.tokens[] | select(.revokedAt == null)] | length' "$work/body")"
client npm npm-logout.log 'npm logout'
! grep -q '_authToken' "$work/npm/.npmrc" || fail "npm logout left the token in .npmrc"
api GET "users/$user/tokens" "" 200
expect "the live tokens of $user after npm logout" 1 "$(jq '[.tokens[] | select(.revokedAt == null)] | length' "$work/body")"
printf 'header = "Authorization: Bearer %s"\n' "$(cat "$work/npm-token")" >"$work/npm-bearer"
req npm-bearer GET "$url/-/whoami"
expect "GET /-/whoami with the token of npm login after npm logout" 401 "$status"
printf 'header = "Authorization: Bearer %s"\n' "$(cat "$work/yarn-token")" >"$work/yarn-bearer"
req yarn-bearer GET "$url/-/whoami"
expect "GET /-/whoami with the token of yarn npm login after npm logout" 200 "$status"
step "npm logout: its token gone from .npmrc and revoked (401); yarn's still valid"

if [ -n "${LAJSTROM_SERVER_LOG:-}" ] && [ -f "$LAJSTROM_SERVER_LOG" ]; then
	no_secret "$LAJSTROM_SERVER_LOG"
	step "no secret of the run in the server's log"
fi

echo "PASS npm e2e against $base"
