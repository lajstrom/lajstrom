#!/usr/bin/env bash
# End-to-end test of a maven repo with mvn and Gradle against a running Lajstrom (#54, #55, #56;
# docs/spec/http-routing.md §3 maven; docs/acceptance.md M-01 to M-04 and M-07, and H-08 when
# the base URL is the edge of the ha stack):
#
#   - a maven repo of its own, created through POST /api/v1/repos, and a deploy token with read
#     and write on it, the <server> lajstrom of settings.xml and the credentials of the Gradle
#     projects (deploy:<token>); a server without the maven format stops the script at once,
#     saying that it needs the maven frontend (#54, #55);
#   - before any deploy, GET of maven-metadata.xml and HEAD of a jar: 404;
#   - mvn deploy of <group>:lib:1.0.0 (fixtures/maven/lib: the jar, the sources and javadoc jars
#     and the pom, each with .sha1 and .md5, then maven-metadata.xml): every PUT 201 in the
#     server's access log (M-01); one manifest for the version, read through /v2, with a file
#     layer per file (its name, its sha256, the media type of its role, extension and
#     classifier), the metadata layer last, no layer for a checksum, and the annotations of the
#     profile;
#   - GET of each file, the bytes deployed (200, or 307 to the registry's storage; M-07), and of
#     its .md5, .sha1, .sha256 and .sha512, text/plain, which match it; HEAD of the jar;
#   - mvn deploy of 1.1.0; maven-metadata.xml lists 1.0.0 and 1.1.0 in that order, latest and
#     release 1.1.0, a lastUpdated, and its checksums match it (M-03);
#   - mvn dependency:get -DremoteRepositories=… -Dartifact=<group>:lib:1.0.0 into an empty local
#     repository, the plugins read from the cache as its tail: the jar and the pom come from the
#     repo, the .sha512 verified by mvn (-C) (M-02);
#   - fixtures/maven/consumer, whose dependency is the range [1.0,2.0): mvn reads
#     maven-metadata.xml and resolves 1.1.0, the jar deployed, its .sha1 verified (-C);
#   - mvn deploy of 1.0.0 again, the same bytes: succeeds, the commit is 200, and
#     maven-metadata.xml still says latest and release 1.1.0: it is rendered from the index, not
#     taken from the client, whose metadata says release 1.0.0 (M-04);
#   - mvn deploy of 1.0.0 with a changed jar: fails with a 409 at the commit; curl shows the
#     server's message, which names the version; the jar served is still the first (M-04);
#   - gradle publish of <group>:gradle-lib:1.0.0 (fixtures/maven/gradle-lib, maven-publish: the
#     jar, the pom and the .module, each with .md5, .sha1, .sha256 and .sha512): every PUT 201,
#     the manifest's three file layers, the files and checksums served; gradle resolve
#     (fixtures/maven/gradle-consumer, maven { url } with the token) gets lib 1.1.0 through the
#     range and gradle-lib 1.0.0 with its module metadata, the bytes deployed (M-02);
#   - no secret in the logs of mvn and gradle, nor in the server's.
#
# Not yet: a session left without maven-metadata.xml that expires (M-06, #54's second
# criterion) waits for a per-repo timeout (#60); the TODO below says so.
#
#   test/e2e/maven.sh [<base url>]     default: http://localhost:8080
#
# It runs as $LAJSTROM_E2E_USER, a user with the admin binding (lib.sh, e2e_credentials; run.sh
# makes one), and creates the repo maven-<run> on the registry local ($MAVEN_E2E_REGISTRY names
# another), with the token; on exit it revokes the token and deletes the repo (force: the
# versions go from the index, the blobs stay in the registry). Every coordinate is under the
# groupId com.example.e2e.r<run>, so that nothing of an earlier run is in a cache. With
# $LAJSTROM_SERVER_LOG, the server's log file (run.sh sets it), the script reads the statuses of
# the clients' requests from its access log; without it those checks are left out and the step
# says so.
#
# The clients run in maven:3.9.16-eclipse-temurin-21 and gradle:8.14.6-jdk21 ($MAVEN_E2E_IMAGE,
# $GRADLE_E2E_IMAGE) with --network host, as the caller (-u), with the run's directory as /w.
# Their caches are the docker volumes lajstrom-e2e-m2 (the local repository, /m2/repository) and
# lajstrom-e2e-gradle (GRADLE_USER_HOME), kept between runs so that the plugins are downloaded
# once; $MAVEN_E2E_M2 and $MAVEN_E2E_GRADLE_HOME name other volumes, or directories of the host
# (absolute paths) that a CI cache can keep. A new volume is root's: the script hands each cache
# to the caller first (chown, as root, of what is not the caller's). No artifact of the run stays
# in them: the deploys skip install, every mvn container removes the run's groupId from the local
# repository when it ends, and dependency:get writes into a directory of the run; Gradle keeps
# what it resolved under the run's groupId in its cache, which it cleans up itself after 30 days
# unused. The token goes into settings.xml and gradle.properties in the run's directory, mode
# 600, and to curl in a config file, never on a command line, and is never printed. It needs curl,
# jq and docker, prints one line per step and exits non-zero at the first failure, naming the
# assertion or the command (lib.sh).
set -euo pipefail
. "$(dirname "$0")/lib.sh"
umask 077

base="${1:-http://localhost:8080}"
base="${base%/}"
for tool in curl jq docker; do
	command -v "$tool" >/dev/null || { echo "maven.sh: $tool is required" >&2; exit 2; }
done
fixtures="$(cd "$(dirname "$0")" && pwd)/fixtures/maven"
maven_image="${MAVEN_E2E_IMAGE:-maven:3.9.16-eclipse-temurin-21}"
gradle_image="${GRADLE_E2E_IMAGE:-gradle:8.14.6-jdk21}"
m2="${MAVEN_E2E_M2:-lajstrom-e2e-m2}"
gradle_home="${MAVEN_E2E_GRADLE_HOME:-lajstrom-e2e-gradle}"
registry="${MAVEN_E2E_REGISTRY:-local}"
dependency_plugin=org.apache.maven.plugins:maven-dependency-plugin:3.8.1 # the version of fixtures/maven/consumer
cache=-Dmaven.repo.local=/m2/repository
server_log=""
if [ -n "${LAJSTROM_SERVER_LOG:-}" ] && [ -f "$LAJSTROM_SERVER_LOG" ]; then server_log="$LAJSTROM_SERVER_LOG"; fi
ids="$(id -u):$(id -g)"

work="$(mktemp -d)"
e2e_credentials "$work"
cp "$E2E_CURLRC" "$work/admin"
run="$(date -u +%Y%m%d%H%M%S)-$RANDOM"
repo="maven-$run"
group="com.example.e2e.r${run//-/}"
gpath="${group//.//}" # the groupId as a path of the repository layout
url="$base/maven/$repo"
created=""
token_id=""
cleanup() {
	if [ -n "$token_id" ]; then
		curl -sS -o /dev/null -K "$work/admin" -X DELETE "$base/api/v1/repos/maven/$repo/deploy-tokens/$token_id" || true
	fi
	if [ -n "$created" ]; then
		curl -sS -o /dev/null -K "$work/admin" -X DELETE "$base/api/v1/repos/maven/$repo?force=true" || true
	fi
	rm -rf "$work"
}
trap cleanup EXIT
status="" # status of the last request

# req CRED METHOD URL [curl args...]: one request with the curl config $work/CRED, or none when
# CRED is -. The status goes to $status, the headers to $work/headers (lower-case names, no CR),
# the body to $work/body.
req() {
	local cred="$1" method="$2" url="$3"
	shift 3
	local how=(-X "$method") # never empty: bash 3.2 takes an empty array for unset under set -u
	[ "$method" != HEAD ] || how=(-I)
	[ "$cred" = - ] || how+=(-K "$work/$cred")
	status="$(curl -sS "${how[@]}" -o "$work/body" -D "$work/headers.raw" -w '%{http_code}' "$@" "$url")"
	tr -d '\r' <"$work/headers.raw" | awk -F': ' 'NF > 1 { $1 = tolower($1); print }' OFS=': ' >"$work/headers"
}

# hdr NAME prints the value of the response header NAME (lower case).
hdr() { awk -F': ' -v n="$1" '$1 == n { sub(/^[^:]*: /, ""); v = $0 } END { print v }' "$work/headers"; }

# digest ALG FILE prints the hex digest of FILE: ALG is md5, sha1, sha256 or sha512.
digest() {
	if command -v "${1}sum" >/dev/null; then
		"${1}sum" "$2" | cut -d' ' -f1
	elif [ "$1" = md5 ]; then
		md5 -q "$2"
	else
		shasum -a "${1#sha}" "$2" | cut -d' ' -f1
	fi
}

# own CACHE IMAGE hands the cache, a volume or a directory, to the caller: docker makes a new
# volume root's, and the clients run as the caller. Only what is not the caller's is changed.
own() {
	docker run --rm -v "$1:/c" --entrypoint find "$2" /c '(' ! -user "${ids%:*}" -o ! -group "${ids#*:}" ')' -exec chown "$ids" '{}' + ||
		fail "chown of the cache $1 to $ids in $2"
}

# The JVMs of the clients connect from IPv4 sockets: under OrbStack, --network host forwards a
# connection to the Mac's 127.0.0.1 from an IPv4 socket only, and Java's are IPv6 by default.
jvm=JAVA_TOOL_OPTIONS=-Djava.net.preferIPv4Stack=true

# mvn LOG ARGS...: mvn in $maven_image as the caller, in batch mode with $work/settings.xml, the
# run's directory as /w and the cache as /m2; the output goes to $work/LOG and mvn's status is
# returned. The run's groupId leaves the cache's local repository when mvn ends: a deploy keeps
# the repo's maven-metadata.xml there, a build the files it resolved.
mvn() {
	local log="$1"
	shift
	docker run --rm --network host -u "$ids" -e HOME=/tmp -e MAVEN_CONFIG=/tmp/m2 -e "$jvm" -v "$work:/w" -v "$m2:/m2" "$maven_image" \
		bash -c 'trap "rm -rf /m2/repository/$0" EXIT; mvn -B -s /w/settings.xml "$@"' "$gpath" "$@" >"$work/$log" 2>&1
}

# gradle LOG ARGS...: gradle in $gradle_image as the caller, with the run's directory as /w and
# the cache as GRADLE_USER_HOME; the output goes to $work/LOG and gradle's status is returned.
gradle() {
	local log="$1"
	shift
	docker run --rm --network host -u "$ids" -e HOME=/tmp -e GRADLE_USER_HOME=/g -e "$jvm" -v "$work:/w" -v "$gradle_home:/g" "$gradle_image" \
		gradle --no-daemon --console=plain "$@" >"$work/$log" 2>&1
}

# mark prints the number of lines of the server log: what the server logs after it is the next
# step's.
mark() {
	if [ -n "$server_log" ]; then wc -l <"$server_log" | tr -d ' '; else echo 0; fi
}

# requests FROM prints the requests to the repo that the server logged after line FROM of its
# log, one per line: "<method> <status> <path>". The access log is text or JSON.
requests() {
	[ -n "$server_log" ] || return 0
	tail -n "+$(($1 + 1))" "$server_log" | sed -nE \
		-e 's/.* method=([A-Z]+) path=([^ ]+) status=([0-9]+).*/\1 \3 \2/p' \
		-e 's/.*"method":"([A-Z]+)","path":"([^"]+)","status":([0-9]+).*/\1 \3 \2/p' |
		awk -v p="/maven/$repo/" 'index($3, p) == 1'
}

# answered FROM METHOD STATUS PATH: the server logged METHOD PATH with STATUS after line FROM.
answered() {
	requests "$1" | awk -v m="$2" -v s="$3" -v p="/maven/$repo/$4" '$1 == m && $2 == s && $3 == p { found = 1 } END { exit !found }'
}

# client_failed LOG WHAT FROM: fail, after the client's error lines from $work/LOG and the
# server's answers to the repo since line FROM of its log.
client_failed() {
	{
		echo "--- $1:"
		grep -E '^\[ERROR\]|FAILURE|What went wrong|^> |Could not' "$work/$1" | head -n 30 || tail -n 30 "$work/$1"
		if [ -n "$server_log" ]; then
			echo "--- the server's answers to /maven/$repo/:"
			requests "$3" | tail -n 40
		fi
	} >&2
	if requests "$3" | awk '$1 == "PUT" && ($2 == 404 || $2 == 405 || $2 == 501) { found = 1 } END { exit !found }'; then
		fail "$2 failed: the server does not take maven PUTs (404, 405 or 501): the deploy path of the maven frontend is #54"
	fi
	fail "$2 failed"
}

# project DIR VERSION: a copy of fixtures/maven/lib in $work/DIR with the run's groupId and
# VERSION in its pom.
project() {
	cp -R "$fixtures/lib" "$work/$1"
	sed -e "s|<groupId>com.example.e2e</groupId>|<groupId>$group</groupId>|" \
		-e "s|<version>1.0.0</version>|<version>$2</version>|" "$fixtures/lib/pom.xml" >"$work/$1/pom.xml"
	if ! grep -qF "<groupId>$group</groupId>" "$work/$1/pom.xml" || ! grep -qF "<version>$2</version>" "$work/$1/pom.xml"; then
		fail "the copy of fixtures/maven/lib/pom.xml in $1 has not the groupId $group and the version $2"
	fi
}

# deploy LOG DIR: mvn deploy of the project in $work/DIR to the repo, without install: nothing of
# the run goes into the cache.
deploy() {
	mvn "$1" -f "/w/$2/pom.xml" "$cache" -Dmaven.install.skip=true -DaltDeploymentRepository="lajstrom::$url" deploy
}

# files DIR VERSION prints what mvn deploy sends for the project in $work/DIR, one per line: the
# file's name in the repo and the file.
files() {
	echo "lib-$2.jar $work/$1/target/lib-$2.jar"
	echo "lib-$2-sources.jar $work/$1/target/lib-$2-sources.jar"
	echo "lib-$2-javadoc.jar $work/$1/target/lib-$2-javadoc.jar"
	echo "lib-$2.pom $work/$1/pom.xml"
}

# built DIR prints the sha1 of each file that mvn deploy sends for the project in $work/DIR.
built() {
	local f
	for f in "$work/$1"/target/*.jar "$work/$1/pom.xml"; do echo "$(basename "$f") $(digest sha1 "$f")"; done
}

# check_puts FROM WHAT STATUSES META DIR LIST SUMS: the server logged, after line FROM, a PUT of
# each file of LIST ("<name> <file>" lines) under DIR (<groupPath>/<artifactId>/<version>) and of
# its checksum files SUMS (md5 sha1 …), then of maven-metadata.xml: every PUT answered with one
# of STATUSES ("201", "200 201"), maven-metadata.xml itself with META. The number of PUTs goes to
# $puts.
check_puts() {
	local from="$1" what="$2" statuses="$3" meta="$4" dir="$5" list="$6" sums="$7" name file s method path
	puts=0
	requests "$from" >"$work/requests"
	while read -r name file; do
		for s in "" $sums; do
			path="/maven/$repo/$dir/$name${s:+.$s}"
			awk -v p="$path" '$1 == "PUT" && $3 == p { found = 1 } END { exit !found }' "$work/requests" ||
				fail "$what: no PUT $path in the server log"
		done
	done <"$list"
	path="/maven/$repo/${dir%/*}/maven-metadata.xml"
	awk -v p="$path" '$1 == "PUT" && $3 == p { found = 1 } END { exit !found }' "$work/requests" ||
		fail "$what: no PUT $path in the server log"
	while read -r method s path; do
		[ "$method" = PUT ] || continue
		puts=$((puts + 1))
		case "$path" in
		*/maven-metadata.xml) [ "$s" = "$meta" ] || fail "$what: PUT $path: $s, want $meta" ;;
		*) case " $statuses " in *" $s "*) ;; *) fail "$what: PUT $path: $s, want $statuses" ;; esac ;;
		esac
	done <"$work/requests"
}

# manifest NAME VERSION: the manifest of <group>:NAME:VERSION from /v2 into $work/manifest.json,
# with a pull token of the admin.
manifest() {
	local repository="maven/$repo/$group/$1"
	req admin GET "$base/v2/token?service=$service&scope=repository:$repository:pull"
	expect "GET /v2/token for $repository" 200 "$status"
	printf 'header = "Authorization: Bearer %s"\n' "$(jq -r .token "$work/body")" >"$work/bearer"
	req bearer GET "$base/v2/$repository/manifests/$2" -H 'Accept: application/vnd.oci.image.manifest.v1+json'
	expect "GET /v2/$repository/manifests/$2 ($(head -c 300 "$work/body"))" 200 "$status"
	cp "$work/body" "$work/manifest.json"
}

# layers NAME VERSION LIST prints the file layers that the files of LIST ("<name> <file>" lines)
# of <group>:NAME:VERSION make, sorted: "<title> <media type> <digest>", then, but for the pom,
# " <extension> <classifier or ->". (A function: bash 3.2 cannot parse a case in $(...).)
layers() {
	local f file rest
	while read -r f file; do
		rest="${f#"$1-$2"}"
		case "$f" in
		*.pom) echo "$f application/vnd.lajstrom.maven.pom.v1+xml sha256:$(digest sha256 "$file")" ;;
		*)
			case "$rest" in
			-*) rest="${rest##*.} $(echo "${rest#-}" | cut -d. -f1)" ;;
			*) rest="${rest##*.} -" ;;
			esac
			echo "$f application/vnd.lajstrom.maven.artifact.v1 sha256:$(digest sha256 "$file") $rest"
			;;
		esac
	done <"$3" | LC_ALL=C sort
}

# check_manifest NAME VERSION PACKAGING LIST: the manifest of <group>:NAME:VERSION has the
# annotations of the profile and one file layer for each file of LIST ("<name> <file>" lines):
# its name as the title, its sha256, the media type of its role and, but for the pom, the
# extension and the classifier of its name; then the metadata layer, and nothing else.
check_manifest() {
	local name="$1" version="$2" packaging="$3" list="$4" got
	manifest "$name" "$version"
	got="$(jq -r '[.mediaType, .artifactType, (.annotations | ."lajstrom.schema", ."lajstrom.format", ."lajstrom.repo", ."lajstrom.name",
		."lajstrom.version", ."lajstrom.maven.groupId", ."lajstrom.maven.artifactId", ."lajstrom.maven.packaging")] | map(. // "-") | join(" ")' "$work/manifest.json")"
	expect "the manifest of $group:$name:$version" \
		"application/vnd.oci.image.manifest.v1+json application/vnd.lajstrom.maven.v1 1 maven $repo $group:$name $version $group $name $packaging" "$got"
	got="$(jq -r '.layers[:-1][] | .annotations as $a | "\($a["org.opencontainers.image.title"]) \(.mediaType) \(.digest)" +
		(if .mediaType == "application/vnd.lajstrom.maven.pom.v1+xml" then ""
		else " \($a["lajstrom.maven.extension"] // "-") \(($a["lajstrom.maven.classifier"] // "") | if . == "" then "-" else . end)" end)' \
		"$work/manifest.json" | LC_ALL=C sort)"
	expect "the file layers of $group:$name:$version (title, media type, digest, extension, classifier)" "$(layers "$name" "$version" "$list")" "$got"
	got="$(jq -r '.layers[-1] | "\(.annotations["org.opencontainers.image.title"]) \(.mediaType)"' "$work/manifest.json")"
	expect "the last layer of $group:$name:$version" "metadata.json application/vnd.lajstrom.maven.metadata.v1+json" "$got"
}

# check_files LIST DIR: GET of each file of LIST ("<name> <file>" lines) under DIR
# (<groupPath>/<artifactId>/<version>): 200, or 307 to the registry's storage, and the bytes of
# the file; GET of its .md5, .sha1, .sha256 and .sha512: 200, text/plain, its digest.
check_files() {
	local list="$1" dir="$2" name file alg got
	while read -r name file; do
		req deploy GET "$url/$dir/$name"
		case "$status" in
		200) ;;
		307) # the registry's storage: no credentials go there
			status="$(curl -sS -o "$work/body" -w '%{http_code}' "$(hdr location)")"
			expect "M-07 GET $dir/$name, the 307 followed" 200 "$status"
			;;
		*) fail "M-07 GET $dir/$name: status $status, want 200 or 307: $(head -c 300 "$work/body")" ;;
		esac
		cmp -s "$file" "$work/body" || fail "GET $dir/$name: not the bytes deployed"
		for alg in md5 sha1 sha256 sha512; do
			req deploy GET "$url/$dir/$name.$alg"
			expect "GET $dir/$name.$alg status" 200 "$status"
			got="$(tr -d '\r' <"$work/body" | awk '{ print $1; exit }')"
			expect "GET $dir/$name.$alg" "$(digest "$alg" "$file")" "$got"
			case "$(hdr content-type)" in
			text/plain*) ;;
			*) fail "GET $dir/$name.$alg: Content-Type '$(hdr content-type)', want text/plain" ;;
			esac
		done
	done <"$list"
}

# xml TAG prints the text of the last <TAG> element of $work/metadata.xml.
xml() { tr -d '\r\n' <"$work/metadata.xml" | sed -nE "s|.*<$1>([^<]*)</$1>.*|\1|p"; }

# check_metadata ARTIFACT VERSIONS LATEST: GET …/<groupPath>/ARTIFACT/maven-metadata.xml: 200,
# the groupId, the artifactId, the versions VERSIONS in that order, latest and release LATEST,
# lastUpdated as yyyyMMddHHmmss; GET of its .md5, .sha1, .sha256 and .sha512: its digests.
check_metadata() {
	local path="$gpath/$1/maven-metadata.xml" alg got
	req deploy GET "$url/$path"
	expect "M-03 GET $path status" 200 "$status"
	cp "$work/body" "$work/metadata.xml"
	got="$(tr -d '\r\n' <"$work/metadata.xml" | sed -nE 's|.*<versions>(.*)</versions>.*|\1|p' |
		grep -o '<version>[^<]*</version>' | sed -E 's|</?version>||g' | tr '\n' ' ' || true)"
	got="$(xml groupId) $(xml artifactId) [${got% }] latest $(xml latest) release $(xml release)"
	expect "M-03 maven-metadata.xml of $group:$1" "$group $1 [$2] latest $3 release $3" "$got"
	xml lastUpdated | grep -Eq '^[0-9]{14}$' || fail "M-03 maven-metadata.xml of $group:$1: lastUpdated '$(xml lastUpdated)', want yyyyMMddHHmmss"
	for alg in md5 sha1 sha256 sha512; do
		req deploy GET "$url/$path.$alg"
		expect "GET $path.$alg status" 200 "$status"
		expect "GET $path.$alg" "$(digest "$alg" "$work/metadata.xml")" "$(tr -d '\r' <"$work/body" | awk '{ print $1; exit }')"
	done
}

# The repo, through the API, and a deploy token for it.
req admin POST "$base/api/v1/repos" -H 'Content-Type: application/json' \
	-d "{\"format\":\"maven\",\"name\":\"$repo\",\"registry\":\"$registry\"}"
if [ "$status" = 400 ] && jq -e '.error.message | test("not a format")' "$work/body" >/dev/null 2>&1; then
	fail "this scenario needs the maven frontend of the server, the deploy path (#54) and the resolve path (#55): POST /api/v1/repos {\"format\":\"maven\"} answered 400 $(jq -r .error.message "$work/body")"
fi
expect "POST /api/v1/repos maven/$repo ($(head -c 300 "$work/body"))" 201 "$status"
created=yes
step "POST /api/v1/repos maven/$repo on the registry $registry as $E2E_USER: 201"
req admin POST "$base/api/v1/repos/maven/$repo/deploy-tokens" -H 'Content-Type: application/json' \
	-d "{\"name\":\"maven e2e $run\",\"scopes\":[\"read\",\"write\"]}"
expect "POST /api/v1/repos/maven/$repo/deploy-tokens ($(jq -c 'del(.secret)' "$work/body" 2>/dev/null || true))" 201 "$status"
jq -r .secret "$work/body" >"$work/token"
token_id="$(jq -r .id "$work/body")"
rm -f "$work/body"
printf 'user = "deploy:%s"\n' "$(cat "$work/token")" >"$work/deploy"
cat >"$work/settings.xml" <<EOF
<settings>
  <servers>
    <server>
      <id>lajstrom</id>
      <username>deploy</username>
      <password>$(cat "$work/token")</password>
    </server>
  </servers>
  <profiles>
    <profile>
      <id>lajstrom</id>
      <repositories>
        <repository>
          <id>lajstrom</id>
          <url>$url</url>
          <releases><updatePolicy>always</updatePolicy><checksumPolicy>fail</checksumPolicy></releases>
          <snapshots><enabled>false</enabled></snapshots>
        </repository>
      </repositories>
    </profile>
  </profiles>
</settings>
EOF
printf 'lajstromUsername=deploy\nlajstromPassword=%s\n' "$(cat "$work/token")" >"$work/gradle-credentials"
step "POST /api/v1/repos/maven/$repo/deploy-tokens, scopes read and write: 201, token $token_id, in settings.xml (<server> lajstrom) and gradle.properties as deploy:<token>"

# The caches belong to the caller, who runs the clients.
own "$m2" "$maven_image"
own "$gradle_home" "$gradle_image"
step "the caches $m2 and $gradle_home belong to $ids, who runs mvn and gradle"

# What the clients ask before the first deploy.
req deploy GET "$url/$gpath/lib/maven-metadata.xml"
expect "GET …/lib/maven-metadata.xml before the first deploy" 404 "$status"
req deploy HEAD "$url/$gpath/lib/1.0.0/lib-1.0.0.jar"
expect "HEAD …/lib-1.0.0.jar before the first deploy" 404 "$status"
step "before the first deploy: GET $gpath/lib/maven-metadata.xml 404, HEAD …/1.0.0/lib-1.0.0.jar 404"

# M-01: mvn deploy of 1.0.0, one manifest.
project lib-1.0.0 1.0.0
files lib-1.0.0 1.0.0 >"$work/files-1.0.0"
from="$(mark)"
deploy deploy-1.0.0.log lib-1.0.0 || client_failed deploy-1.0.0.log "mvn deploy of $group:lib:1.0.0" "$from"
if [ -n "$server_log" ]; then
	check_puts "$from" "M-01 mvn deploy of $group:lib:1.0.0" 201 201 "$gpath/lib/1.0.0" "$work/files-1.0.0" "sha1 md5"
	step "M-01 mvn deploy of $group:lib:1.0.0 (jar, sources, javadoc, pom, each with .sha1 and .md5, then maven-metadata.xml): $puts PUTs, every one 201"
else
	step "M-01 mvn deploy of $group:lib:1.0.0: done (no server log: the statuses are not checked)"
fi
req - GET "$base/v2/"
expect "GET /v2/ without credentials" 401 "$status"
service="$(hdr www-authenticate | sed -E 's/.*service="([^"]*)".*/\1/')"
check_manifest lib 1.0.0 jar "$work/files-1.0.0"
step "M-01 GET /v2/maven/$repo/$group/lib/manifests/1.0.0: $(jq -r .artifactType "$work/manifest.json"), $(jq '.layers | length - 1' "$work/manifest.json") file layers with the digests of the files built, the metadata layer last, no checksum layer, the annotations of the profile"

# M-07 and the checksums: every file of 1.0.0 as deployed.
check_files "$work/files-1.0.0" "$gpath/lib/1.0.0"
req deploy HEAD "$url/$gpath/lib/1.0.0/lib-1.0.0.jar"
expect "HEAD lib-1.0.0.jar" "200 $(wc -c <"$work/lib-1.0.0/target/lib-1.0.0.jar" | tr -d ' ')" "$status $(hdr content-length)"
req deploy GET "$url/$gpath/lib/1.0.0/lib-1.0.0-tests.jar"
expect "GET of a file that 1.0.0 does not have" 404 "$status"
step "M-07 GET of the jar, the sources, the javadoc and the pom of 1.0.0: the bytes deployed; their .md5, .sha1, .sha256 and .sha512: text/plain, matching; HEAD of the jar 200 with its Content-Length; a file it does not have: 404"

# M-03: 1.1.0, and maven-metadata.xml with both.
project lib-1.1.0 1.1.0
files lib-1.1.0 1.1.0 >"$work/files-1.1.0"
from="$(mark)"
deploy deploy-1.1.0.log lib-1.1.0 || client_failed deploy-1.1.0.log "mvn deploy of $group:lib:1.1.0" "$from"
if [ -n "$server_log" ]; then
	check_puts "$from" "mvn deploy of $group:lib:1.1.0" 201 201 "$gpath/lib/1.1.0" "$work/files-1.1.0" "sha1 md5"
	summary="$puts PUTs, every one 201"
else
	summary="done (no server log: the statuses are not checked)"
fi
check_manifest lib 1.1.0 jar "$work/files-1.1.0"
step "mvn deploy of $group:lib:1.1.0: $summary; a manifest of its own"
check_metadata lib "1.0.0 1.1.0" 1.1.0
step "M-03 GET $gpath/lib/maven-metadata.xml: versions 1.0.0 1.1.0, latest and release 1.1.0, lastUpdated $(xml lastUpdated); its .md5, .sha1, .sha256 and .sha512 match it"

# M-02: dependency:get into an empty local repository; the plugin is read from the cache, the
# local repository's tail, which mvn never writes.
mkdir "$work/m2-empty"
from="$(mark)"
mvn get.log -Dmaven.repo.local=/w/m2-empty -Dmaven.repo.local.tail=/m2/repository -C -Daether.checksums.algorithms=SHA-512,SHA-1 \
	"$dependency_plugin:get" -DremoteRepositories="lajstrom::default::$url" -Dartifact="$group:lib:1.0.0" -Dtransitive=false ||
	client_failed get.log "mvn dependency:get of $group:lib:1.0.0" "$from"
for f in lib-1.0.0.jar:target/lib-1.0.0.jar lib-1.0.0.pom:pom.xml; do
	cmp -s "$work/lib-1.0.0/${f#*:}" "$work/m2-empty/$gpath/lib/1.0.0/${f%%:*}" ||
		fail "M-02 dependency:get: ${f%%:*} in the local repository is not the file deployed"
	grep -qF "Downloaded from lajstrom: $url/$gpath/lib/1.0.0/${f%%:*}" "$work/get.log" ||
		fail "M-02 dependency:get: mvn did not download ${f%%:*} from the repo"
done
if [ -n "$server_log" ]; then
	answered "$from" GET 200 "$gpath/lib/1.0.0/lib-1.0.0.jar.sha512" || fail "M-02 dependency:get: no GET of lib-1.0.0.jar.sha512 answered 200"
fi
step "M-02 mvn dependency:get -DremoteRepositories=lajstrom::default::$url -Dartifact=$group:lib:1.0.0 into an empty local repository: the jar and the pom downloaded from the repo, the .sha512 verified (-C)"

# The range [1.0,2.0) through maven-metadata.xml.
cp -R "$fixtures/consumer" "$work/consumer"
from="$(mark)"
mvn consumer.log -f /w/consumer/pom.xml "$cache" -C -Plajstrom -De2e.groupId="$group" \
	dependency:list dependency:copy-dependencies -DoutputFile=/w/consumer/dependencies.txt -DoutputDirectory=/w/consumer/resolved ||
	client_failed consumer.log "mvn dependency:list of fixtures/maven/consumer ($group:lib:[1.0,2.0))" "$from"
grep -qF "$group:lib:jar:1.1.0:compile" "$work/consumer/dependencies.txt" ||
	fail "the range [1.0,2.0) of $group:lib resolved to '$(grep -o 'lib:jar:[^:]*' "$work/consumer/dependencies.txt" || true)', want lib:jar:1.1.0"
cmp -s "$work/lib-1.1.0/target/lib-1.1.0.jar" "$work/consumer/resolved/lib-1.1.0.jar" ||
	fail "the range [1.0,2.0): lib-1.1.0.jar is not the jar deployed"
if [ -n "$server_log" ]; then
	answered "$from" GET 200 "$gpath/lib/maven-metadata.xml" || fail "the range [1.0,2.0): no GET of maven-metadata.xml answered 200"
fi
step "fixtures/maven/consumer, $group:lib:[1.0,2.0): mvn reads maven-metadata.xml and resolves 1.1.0, the jar deployed, its .sha1 verified (-C)"

# M-04: the same version again, the same bytes.
before="$(built lib-1.0.0)"
from="$(mark)"
deploy redeploy-1.0.0.log lib-1.0.0 || client_failed redeploy-1.0.0.log "mvn deploy of $group:lib:1.0.0 again" "$from"
expect "the files of lib 1.0.0 built again (the build is reproducible, so that the redeploy sends the same bytes)" "$before" "$(built lib-1.0.0)"
if [ -n "$server_log" ]; then
	check_puts "$from" "M-04 mvn deploy of $group:lib:1.0.0 again" "200 201" 200 "$gpath/lib/1.0.0" "$work/files-1.0.0" "sha1 md5"
	summary="PUT maven-metadata.xml 200, the $puts PUTs 200 or 201"
else
	summary="done (no server log: the statuses are not checked)"
fi
check_metadata lib "1.0.0 1.1.0" 1.1.0
step "M-04 mvn deploy of $group:lib:1.0.0 again, the same bytes: succeeds, $summary; maven-metadata.xml still says latest and release 1.1.0 (mvn sent release 1.0.0)"

# M-04: the same version with a changed jar: 409, and the first jar stays.
cp -R "$work/lib-1.0.0" "$work/lib-1.0.0-changed"
rm -rf "$work/lib-1.0.0-changed/target"
mkdir -p "$work/lib-1.0.0-changed/src/main/resources"
echo "changed in $run" >"$work/lib-1.0.0-changed/src/main/resources/changed.txt"
from="$(mark)"
if deploy changed-1.0.0.log lib-1.0.0-changed; then
	fail "M-04 mvn deploy of $group:lib:1.0.0 with a changed jar succeeded; want a 409"
fi
said="$(grep -o 'status code: 409[^]]*Conflict[^]]*([0-9]*)' "$work/changed-1.0.0.log" | head -n 1 || true)"
[ -n "$said" ] || client_failed changed-1.0.0.log "mvn deploy of $group:lib:1.0.0 with a changed jar failed, but not with a 409:" "$from"
if [ -n "$server_log" ]; then
	at="$(requests "$from" | awk '$1 == "PUT" && $2 == 409 { print $3; exit }')"
	[ -n "$at" ] || fail "M-04 the server log has no PUT answered 409 for the changed jar"
	at="PUT ${at#"/maven/$repo/$gpath/lib/"} 409 in the server log"
else
	at="(no server log)"
fi
req deploy GET "$url/$gpath/lib/1.0.0/lib-1.0.0.jar" -L
cmp -s "$work/lib-1.0.0/target/lib-1.0.0.jar" "$work/body" || fail "M-04 after the 409, lib-1.0.0.jar is not the first jar"
# The server's message, with curl: the changed jar and the pom into a new session (the 409
# closed mvn's), then the commit.
req deploy PUT "$url/$gpath/lib/1.0.0/lib-1.0.0.jar" -T "$work/lib-1.0.0-changed/target/lib-1.0.0.jar"
if [ "$status" != 409 ]; then
	expect "PUT of the changed lib-1.0.0.jar into the session ($(head -c 300 "$work/body"))" 201 "$status"
	req deploy PUT "$url/$gpath/lib/1.0.0/lib-1.0.0.pom" -T "$work/lib-1.0.0-changed/pom.xml"
	expect "PUT of lib-1.0.0.pom into the session ($(head -c 300 "$work/body"))" 201 "$status"
	# The commit is for the document's release, which mvn sets to the version it deploys.
	req deploy GET "$url/$gpath/lib/maven-metadata.xml"
	sed -e 's|<release>[^<]*</release>|<release>1.0.0</release>|' "$work/body" >"$work/commit.xml"
	req deploy PUT "$url/$gpath/lib/maven-metadata.xml" -T "$work/commit.xml"
fi
expect "M-04 the commit of other bytes under $group:lib:1.0.0 with curl ($(head -c 300 "$work/body"))" 409 "$status"
grep -qF 1.0.0 "$work/body" || fail "M-04 the message of the 409 does not name the version 1.0.0: $(head -c 300 "$work/body")"
step "M-04 mvn deploy of $group:lib:1.0.0 with a changed jar: fails, \"$said\", $at; with curl: 409 \"$(tr -d '\n' <"$work/body" | head -c 200)\"; lib-1.0.0.jar is still the first"

# TODO(#60): M-06 and #54's second criterion end to end, a session left without
# maven-metadata.xml that expires: PUT lib-2.0.0.jar alone with curl, wait out the repo's
# timeout, then 2.0.0 is not in maven-metadata.xml and its jar is 404. A session lives 30
# minutes after its last request and its files are dropped (there is no seal, #53); the timeout
# becomes a repo's policy, maven.sessionTimeout, with #60, and this step comes with it. Until
# then the expiry is tested in internal/ingest (TestSessionExpiry) and internal/format/maven
# (TestDeployExpiry) with a clock.

# Gradle: maven-publish of another artifact, then a resolve of both.
cp -R "$fixtures/gradle-lib" "$work/gradle-lib"
cat "$work/gradle-credentials" >>"$work/gradle-lib/gradle.properties"
from="$(mark)"
gradle publish.log -p /w/gradle-lib -Pgroup="$group" -Pversion=1.0.0 -Pe2eUrl="$url" publish ||
	client_failed publish.log "gradle publish of $group:gradle-lib:1.0.0" "$from"
{
	echo "gradle-lib-1.0.0.jar $work/gradle-lib/build/libs/gradle-lib-1.0.0.jar"
	echo "gradle-lib-1.0.0.pom $work/gradle-lib/build/publications/lib/pom-default.xml"
	echo "gradle-lib-1.0.0.module $work/gradle-lib/build/publications/lib/module.json"
} >"$work/files-gradle"
if [ -n "$server_log" ]; then
	check_puts "$from" "gradle publish of $group:gradle-lib:1.0.0" 201 201 "$gpath/gradle-lib/1.0.0" "$work/files-gradle" "md5 sha1 sha256 sha512"
	summary="$puts PUTs, every one 201"
else
	summary="done (no server log: the statuses are not checked)"
fi
check_manifest gradle-lib 1.0.0 jar "$work/files-gradle"
check_files "$work/files-gradle" "$gpath/gradle-lib/1.0.0"
check_metadata gradle-lib 1.0.0 1.0.0
step "gradle publish of $group:gradle-lib:1.0.0 (jar, pom, .module, each with .md5, .sha1, .sha256 and .sha512, then maven-metadata.xml): $summary; a manifest with three file layers; the files, their checksums and maven-metadata.xml served"

cp -R "$fixtures/gradle-consumer" "$work/gradle-consumer"
cp "$work/gradle-credentials" "$work/gradle-consumer/gradle.properties"
from="$(mark)"
gradle resolve.log -p /w/gradle-consumer -Pe2eUrl="$url" -Pe2eGroup="$group" -Pe2eGradleVersion=1.0.0 resolve ||
	client_failed resolve.log "gradle resolve of $group:lib:[1.0,2.0) and $group:gradle-lib:1.0.0" "$from"
expect "M-02 the files gradle resolved" "gradle-lib-1.0.0.jar lib-1.1.0.jar" "$(cd "$work/gradle-consumer/build/resolved" && echo *)"
cmp -s "$work/lib-1.1.0/target/lib-1.1.0.jar" "$work/gradle-consumer/build/resolved/lib-1.1.0.jar" ||
	fail "M-02 gradle resolve: lib-1.1.0.jar is not the jar deployed"
cmp -s "$work/gradle-lib/build/libs/gradle-lib-1.0.0.jar" "$work/gradle-consumer/build/resolved/gradle-lib-1.0.0.jar" ||
	fail "M-02 gradle resolve: gradle-lib-1.0.0.jar is not the jar published"
if [ -n "$server_log" ]; then
	answered "$from" GET 200 "$gpath/lib/maven-metadata.xml" || fail "gradle resolve: no GET of lib's maven-metadata.xml answered 200"
	answered "$from" GET 200 "$gpath/gradle-lib/1.0.0/gradle-lib-1.0.0.module" || fail "gradle resolve: no GET of gradle-lib-1.0.0.module answered 200"
fi
step "M-02 gradle resolve (maven { url } with the token): $group:lib:[1.0,2.0) is 1.1.0 through maven-metadata.xml, $group:gradle-lib:1.0.0 with its .module; the bytes deployed"

# No secret in the logs.
for f in "$work"/*.log ${server_log:+"$server_log"}; do
	! grep -qF -f "$work/token" "$f" || fail "$(basename "$f") holds the secret of the deploy token"
done
step "no secret in the logs of mvn, gradle${server_log:+ and the server}"
echo "PASS maven e2e against $base, repo maven/$repo, groupId $group"
