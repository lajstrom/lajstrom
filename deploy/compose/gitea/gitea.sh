#!/bin/sh
# Gitea's container registry for the capability-matrix spike (issue #120,
# docs/registries/gitea.md): the compose project of compose.yaml in this directory, set up with
# the CLI and the REST API, never the UI. For development only: plain HTTP and the passwords
# below.
#
#   deploy/compose/gitea/gitea.sh up [gitea|forgejo]   docker compose up --wait, then: a site
#                       admin, the user lajstrom, the organisation lajstrom-it with a team that
#                       may write its packages, the user in it, and a token of the user with the
#                       scope write:package, written to .work/<service>.token
#   deploy/compose/gitea/gitea.sh down                 docker compose down -v, both services
#
# up is idempotent: on a set-up volume it makes a new token.
#
#   LAJSTROM_GITEA_PORT        5009       Gitea on 127.0.0.1
#   LAJSTROM_FORGEJO_PORT      5014       Forgejo on 127.0.0.1
#   LAJSTROM_GITEA_ADMIN_PASSWORD  lajstrom-admin   the site admin lajstrom-admin
#   LAJSTROM_GITEA_PASSWORD    lajstrom-spike   the user lajstrom
#   LAJSTROM_GITEA_ORG         lajstrom-it  the organisation: Lajstrom's prefix is <org>/
set -eu

here=$(cd "$(dirname "$0")" && pwd -P)
admin_password=${LAJSTROM_GITEA_ADMIN_PASSWORD:-lajstrom-admin}
password=${LAJSTROM_GITEA_PASSWORD:-lajstrom-spike}
org=${LAJSTROM_GITEA_ORG:-lajstrom-it}

compose() { docker compose -f "$here/compose.yaml" --profile forgejo "$@"; }

# api USER:PASSWORD METHOD PATH [JSON]: a REST call; prints the body, fails on a status >= 400.
api() {
	if [ $# -ge 4 ]; then
		curl -fsS -u "$1" -X "$2" -H 'Content-Type: application/json' --data-binary "$4" "$base$3"
	else
		curl -fsS -u "$1" -X "$2" "$base$3"
	fi
}

# status USER:PASSWORD PATH: the HTTP status of a GET.
status() { curl -sS -o /dev/null -w '%{http_code}' -u "$1" "$base$2"; }

up() {
	command -v jq >/dev/null || { echo "gitea.sh: jq is required" >&2; exit 2; }
	service=${1:-gitea}
	case $service in
	gitea) port=${LAJSTROM_GITEA_PORT:-5009} ;;
	forgejo) port=${LAJSTROM_FORGEJO_PORT:-5014} ;;
	*) echo "gitea.sh: no service $service" >&2; exit 2 ;;
	esac
	base=http://localhost:$port
	compose up -d --wait "$service"
	admin="lajstrom-admin:$admin_password"
	# The first user, a site admin, from the CLI inside the container: the API needs one.
	if [ "$(status "$admin" /api/v1/user)" != 200 ]; then
		compose exec -T "$service" gitea admin user create --admin --username lajstrom-admin \
			--password "$admin_password" --email lajstrom-admin@example.invalid --must-change-password=false
	fi
	if [ "$(status "$admin" /api/v1/users/lajstrom)" != 200 ]; then
		api "$admin" POST /api/v1/admin/users "$(jq -nc --arg p "$password" \
			'{username: "lajstrom", email: "lajstrom@example.invalid", password: $p, must_change_password: false}')" >/dev/null
	fi
	if [ "$(status "$admin" "/api/v1/orgs/$org")" != 200 ]; then
		api "$admin" POST /api/v1/orgs "$(jq -nc --arg o "$org" '{username: $o, visibility: "private"}')" >/dev/null
	fi
	# A team whose only unit is the packages, with write: what pushing and deleting need.
	team=$(api "$admin" GET "/api/v1/orgs/$org/teams/search?q=packages" | jq -r '.data[] | select(.name == "packages") | .id')
	if [ -z "$team" ]; then
		team=$(api "$admin" POST "/api/v1/orgs/$org/teams" '{"name":"packages","permission":"write",
			"units":["repo.packages"],"units_map":{"repo.packages":"write"},
			"includes_all_repositories":false,"can_create_org_repo":false}' | jq -r .id)
	fi
	api "$admin" PUT "/api/v1/teams/$team/members/lajstrom" >/dev/null
	# A new token on every up: Gitea shows a token's secret once.
	user="lajstrom:$password"
	curl -sS -o /dev/null -u "$user" -X DELETE "$base/api/v1/users/lajstrom/tokens/lajstrom-spike"
	mkdir -p "$here/.work"
	api "$user" POST /api/v1/users/lajstrom/tokens '{"name":"lajstrom-spike","scopes":["write:package"]}' |
		jq -r .sha1 >"$here/.work/$service.token"
	chmod 600 "$here/.work/$service.token"
	echo "$service $(api "$admin" GET /api/v1/version | jq -r .version) on $base: prefix $org/," \
		"user lajstrom, its token in $here/.work/$service.token"
}

down() {
	compose down -v --remove-orphans
	rm -rf "$here/.work"
}

case "${1:-}" in
up) up "${2:-gitea}" ;;
down) down ;;
*)
	echo "usage: $0 up [gitea|forgejo] | down" >&2
	exit 2
	;;
esac
