#!/bin/sh
# JFrog Container Registry for the capability-matrix spike (issue #338,
# docs/registries/jfrog-jcr.md): the compose project "lajstrom-jcr" of compose.yaml on
# http://localhost:5010, beside the dev stack and not part of it, set up through the REST API.
# For development only: plain HTTP.
#
#   deploy/compose/jfrog-jcr/jcr.sh up      docker compose up --wait, then setup
#   deploy/compose/jfrog-jcr/jcr.sh setup   the REST setup alone, again (each step is idempotent)
#   deploy/compose/jfrog-jcr/jcr.sh down    docker compose down -v, and remove the secrets
#
# setup, as the admin: set the admin password, accept the EULA (the UI's endpoint, POST
# /artifactory/ui/jcr/eula/accept: JCR has no other), get a short-lived admin token with Basic
# credentials (access.config.patch.yml allows it), create the repositories with the YAML
# configuration API (the repositories API is Artifactory Pro only), and a token that never
# expires for Lajstrom and the tests. JCR creates no user through its REST API (Pro only), so
# that token is the admin's, with the user scope (docs/registries/jfrog-jcr.md). The
# repositories, in the repository path method (localhost:5010/<repo-key>/<image>):
#
#   lajstrom          local, docker, V2     Lajstrom's prefix: prefix: lajstrom/
#   lajstrom-oci      local, oci            the same with the OCI package type, to compare
#   lajstrom-virtual  virtual, docker       lajstrom behind it, deployments go to lajstrom
#
# LAJSTROM_JCR_DIR (default deploy/compose/jfrog-jcr/.work, ignored by git) receives the
# secrets, each in a file of its own, mode 600: admin.password, and lajstrom.token (the
# password of the user admin for Lajstrom and the tests). They never reach the terminal.
set -eu

port=${LAJSTROM_JCR_PORT:-5010}
base=http://localhost:$port
here=$(cd "$(dirname "$0")" && pwd -P)
dir=${LAJSTROM_JCR_DIR:-$here/.work}

compose() { docker compose -f "$here/compose.yaml" "$@"; }

# secret <file>: create a random secret in the file unless it exists.
secret() {
	[ -s "$dir/$1" ] || (umask 077 && head -c 24 /dev/urandom | base64 | tr -d '+/=\n' >"$dir/$1")
}

# api <expected status> <curl arguments...>: one request; its body goes to $dir/body, and a
# status other than the expected one (a space-separated list) fails setup with it.
api() {
	want=$1
	shift
	status=$(curl -sS -o "$dir/body" -w '%{http_code}' "$@")
	case " $want " in *" $status "*) return 0 ;; esac
	echo "jcr.sh: $status from: $*" | sed -E 's/(-u [^: ]+:)[^ ]+/\1***/g' >&2
	head -c 2000 "$dir/body" >&2
	echo >&2
	exit 1
}

setup() {
	mkdir -p "$dir"
	dir=$(cd "$dir" && pwd -P)
	umask 077
	# The admin token of this run and the answers go, whatever happens.
	trap 'rm -f "$dir/admin.header" "$dir/body" "$dir/request.json" "$dir/request.yaml"' EXIT
	secret admin.password
	admin="admin:$(cat "$dir/admin.password")"

	# The admin password: the default "password" until the first setup.
	if [ "$(curl -s -o /dev/null -w '%{http_code}' -u "$admin" "$base/artifactory/api/system/version")" != 200 ]; then
		jq -n --rawfile p "$dir/admin.password" \
			'{userName: "admin", oldPassword: "password", newPassword1: $p, newPassword2: $p}' >"$dir/request.json"
		api 200 -u admin:password -X POST -H 'Content-Type: application/json' -d "@$dir/request.json" \
			"$base/artifactory/api/security/users/authorization/changePassword"
	fi

	api 200 -u "$admin" -X POST "$base/artifactory/ui/jcr/eula/accept"

	# An admin token for the Access API, for this run; it expires in 10 minutes.
	api 200 -u "$admin" -X POST -d scope=applied-permissions/admin -d expires_in=600 -d description=jcr.sh \
		"$base/access/api/v1/tokens"
	printf 'Authorization: Bearer %s\n' "$(jq -r .access_token "$dir/body")" >"$dir/admin.header"

	cat >"$dir/request.yaml" <<-'EOF'
		localRepositories:
		  lajstrom:
		    type: docker
		    description: "Lajstrom's prefix (issue #338)"
		    dockerApiVersion: V2
		  lajstrom-oci:
		    type: oci
		    description: "Lajstrom's prefix, OCI package type (issue #338)"
		virtualRepositories:
		  lajstrom-virtual:
		    type: docker
		    description: "lajstrom behind a virtual repository (issue #338)"
		    repositories:
		      - lajstrom
		    defaultDeploymentRepo: lajstrom
	EOF
	api 200 -u "$admin" -X PATCH -H 'Content-Type: application/yaml' --data-binary "@$dir/request.yaml" \
		"$base/artifactory/api/system/configuration"

	# No user of its own: on JCR the Access API answers 403 to users, groups and permissions,
	# and Artifactory's security API says "available only in Artifactory Pro"; a token of a
	# transient user has only the groups of its scope (readers: no push). So the token is the
	# admin's, with the user scope.
	if [ ! -s "$dir/lajstrom.token" ]; then
		jq -n '{scope: "applied-permissions/user", expires_in: 0, description: "lajstrom (jcr.sh)"}' >"$dir/request.json"
		api 200 -H "@$dir/admin.header" -H 'Content-Type: application/json' -d "@$dir/request.json" \
			"$base/access/api/v1/tokens"
		jq -r .access_token "$dir/body" >"$dir/lajstrom.token"
	fi
	api 200 -u "admin:$(cat "$dir/lajstrom.token")" "$base/v2/"
	echo "JCR on $base: the token for Lajstrom (user admin) in $dir/lajstrom.token, the admin's password in $dir/admin.password"
}

up() {
	compose up -d --wait
	setup
}

down() {
	compose down -v --remove-orphans
	rm -rf "$dir"
}

case "${1:-}" in
up) up ;;
setup) setup ;;
down) down ;;
*)
	echo "usage: $0 up|setup|down" >&2
	exit 2
	;;
esac
