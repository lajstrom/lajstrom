#!/bin/sh
# Project Quay as a backend registry (docs/registries/quay.md): the compose project of
# compose.yaml in this directory, set up through Quay's REST API, never the UI. For development
# only: plain HTTP and the password below.
#
#   deploy/compose/quay/quay.sh up     docker compose up --wait, then: the first user quayadmin
#                       (a superuser) by POST /api/v1/user/initialize, whose OAuth token goes
#                       to .work/admin.token; the organisation lajstrom-it; its robot
#                       lajstrom-it+lajstrom in a team with the role admin; the robot's token
#                       written to .work/robot.token
#   deploy/compose/quay/quay.sh stop   docker compose stop: the volumes stay
#   deploy/compose/quay/quay.sh down   docker compose down -v, and the token files
#
# up is idempotent while .work/admin.token is there: Quay gives the first user's token once,
# on an empty database. Without it, on a set-up volume, down and up again.
#
#   LAJSTROM_QUAY_PORT            5012            Quay on 127.0.0.1; config.yaml's SERVER_HOSTNAME must match
#   LAJSTROM_QUAY_ADMIN_PASSWORD  lajstrom-admin  the superuser quayadmin
#   LAJSTROM_QUAY_ORG             lajstrom-it     the organisation: Lajstrom's prefix is <org>/
set -eu

here=$(cd "$(dirname "$0")" && pwd -P)
port=${LAJSTROM_QUAY_PORT:-5012}
base=http://localhost:$port
admin_password=${LAJSTROM_QUAY_ADMIN_PASSWORD:-lajstrom-admin}
org=${LAJSTROM_QUAY_ORG:-lajstrom-it}
robot=lajstrom
work=$here/.work

compose() { docker compose -f "$here/compose.yaml" "$@"; }

# api METHOD PATH [JSON]: a REST call with the superuser's token; prints the body, fails on a
# status >= 400.
api() {
	if [ $# -ge 3 ]; then
		curl -fsS -X "$1" -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
			--data-binary "$3" "$base$2"
	else
		curl -fsS -X "$1" -H "Authorization: Bearer $token" "$base$2"
	fi
}

# status PATH: the HTTP status of a GET with the superuser's token.
status() { curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token" "$base$1"; }

up() {
	command -v jq >/dev/null || { echo "quay.sh: jq is required" >&2; exit 2; }
	compose up -d --wait
	mkdir -p "$work"
	if [ ! -s "$work/admin.token" ]; then
		body=$(jq -nc --arg p "$admin_password" \
			'{username: "quayadmin", password: $p, email: "quayadmin@example.invalid", access_token: true}')
		answer=$(curl -sS -X POST -H 'Content-Type: application/json' --data-binary "$body" "$base/api/v1/user/initialize")
		token=$(printf '%s' "$answer" | jq -r '.access_token // empty')
		if [ -z "$token" ]; then
			echo "quay.sh: no first-user token: $(printf '%s' "$answer" | jq -r '.message // .')" >&2
			echo "quay.sh: a database set up before keeps its users; $0 down, then up" >&2
			exit 1
		fi
		(umask 077 && printf '%s\n' "$token" >"$work/admin.token")
	fi
	token=$(cat "$work/admin.token")
	if [ "$(status "/api/v1/organization/$org")" != 200 ]; then
		api POST /api/v1/organization/ "$(jq -nc --arg o "$org" '{name: $o, email: ($o + "@example.invalid")}')" >/dev/null
	fi
	if [ "$(status "/api/v1/organization/$org/robots/$robot")" != 200 ]; then
		api PUT "/api/v1/organization/$org/robots/$robot" '{"description":"Lajstrom backend"}' >/dev/null
	fi
	# A team with the role admin: the robot administers every repository of the organisation.
	# With the role creator it may create repositories and administers those it created, but a
	# repository that does not exist is 401 to it, not 404 NAME_UNKNOWN, which Lajstrom reads as
	# bad credentials (docs/registries/quay.md, Organisations, robots and create on push).
	api PUT "/api/v1/organization/$org/team/lajstrom" '{"role":"admin","description":"Lajstrom backend"}' >/dev/null
	# A second PUT of a member is 400 "already a member".
	if ! api GET "/api/v1/organization/$org/team/lajstrom/members" |
		jq -e --arg m "$org+$robot" 'any(.members[]; .name == $m)' >/dev/null; then
		api PUT "/api/v1/organization/$org/team/lajstrom/members/$org+$robot" >/dev/null
	fi
	(umask 077 && api GET "/api/v1/organization/$org/robots/$robot" | jq -r .token >"$work/robot.token")
	echo "$(curl -fsS "$base/config" | jq -r .version_number) on $base: prefix $org/," \
		"user $org+$robot, its token in $work/robot.token; the superuser's in $work/admin.token"
}

case "${1:-}" in
up) up ;;
stop) compose stop ;;
down)
	compose down -v --remove-orphans
	rm -rf "$work"
	;;
*)
	echo "usage: $0 up | stop | down" >&2
	exit 2
	;;
esac
