#!/bin/sh
# Nexus Repository for the capability-matrix spike (issue #119, docs/registries/nexus.md): the
# compose project of compose.yaml in this directory, set up through the REST API, never the UI.
# For development only: plain HTTP and the passwords below.
#
#   deploy/compose/nexus/nexus.sh up     docker compose up --wait, then: the admin password, the
#                                        Community Edition's EULA, anonymous access off, the
#                                        realms, a docker hosted and an OCI hosted repository, a
#                                        role with their privileges and a user with the role
#   deploy/compose/nexus/nexus.sh down   docker compose down -v
#
# up is idempotent: on a set-up volume it updates the repositories with the settings below.
#
#   LAJSTROM_NEXUS_PORT              8081       the UI and the REST API on 127.0.0.1
#   LAJSTROM_NEXUS_DOCKER_PORT       5008       the docker repository's HTTP connector on 127.0.0.1
#   LAJSTROM_NEXUS_OCI_PORT          5015       the OCI repository's HTTP connector on 127.0.0.1
#   LAJSTROM_NEXUS_ADMIN_PASSWORD    lajstrom-admin   set on the first up, from the generated one
#   LAJSTROM_NEXUS_WRITE_POLICY      allow      allow, allow_once (no redeploy) or deny, both repositories
#   LAJSTROM_NEXUS_USER              lajstrom   a user with the repositories' privileges only
#   LAJSTROM_NEXUS_PASSWORD          lajstrom   its password
#
# The repositories: "lajstrom", format docker, connector 8082 in the container; "lajstrom-oci",
# format oci, connector 8083. Both blob store "default", forceBasicAuth (Basic auth on /v2/).
set -eu

here=$(cd "$(dirname "$0")" && pwd -P)
port=${LAJSTROM_NEXUS_PORT:-8081}
base=http://localhost:$port
admin_password=${LAJSTROM_NEXUS_ADMIN_PASSWORD:-lajstrom-admin}
policy=${LAJSTROM_NEXUS_WRITE_POLICY:-allow}
user=${LAJSTROM_NEXUS_USER:-lajstrom}
password=${LAJSTROM_NEXUS_PASSWORD:-lajstrom}

compose() { docker compose -f "$here/compose.yaml" "$@"; }

# api METHOD PATH [JSON]: a REST call as admin; prints the body, fails on a status >= 400.
api() {
	if [ $# -ge 3 ]; then
		curl -fsS -u "admin:$admin_password" -X "$1" -H 'Content-Type: application/json' --data-binary "$3" "$base$2"
	else
		curl -fsS -u "admin:$admin_password" -X "$1" "$base$2"
	fi
}

# status PATH: the HTTP status of a GET as admin.
status() { curl -sS -o /dev/null -w '%{http_code}' -u "admin:$admin_password" "$base$1"; }

# hosted FORMAT NAME PORT: create or update a hosted repository of FORMAT (docker or oci) with
# an HTTP connector on PORT inside the container.
hosted() {
	attrs=$(jq -nc --argjson port "$3" '{v1Enabled: false, forceBasicAuth: true, httpPort: $port}')
	body=$(jq -nc --arg format "$1" --arg name "$2" --arg policy "$policy" --argjson attrs "$attrs" '{
		name: $name, online: true,
		storage: {blobStoreName: "default", strictContentTypeValidation: true, writePolicy: $policy, latestPolicy: false},
		component: {proprietaryComponents: false}
	} + {($format): $attrs}')
	if [ "$(status "/service/rest/v1/repositories/$1/hosted/$2")" = 200 ]; then
		api PUT "/service/rest/v1/repositories/$1/hosted/$2" "$body"
	else
		api POST "/service/rest/v1/repositories/$1/hosted" "$body"
	fi
}

# wait_v2 HOSTPORT: until the connector answers the user's GET /v2/ with 200.
wait_v2() {
	i=0
	until [ "$(curl -sS -o /dev/null -w '%{http_code}' -u "$user:$password" "http://$1/v2/")" = 200 ]; do
		i=$((i + 1))
		[ "$i" -le 30 ] || { echo "nexus.sh: $1 does not answer /v2/ with 200" >&2; exit 1; }
		sleep 1
	done
}

up() {
	command -v jq >/dev/null || { echo "nexus.sh: jq is required" >&2; exit 2; }
	compose up -d --wait
	# The first start writes a random admin password to admin.password; replace it.
	if generated=$(compose exec -T nexus cat /nexus-data/admin.password 2>/dev/null); then
		curl -fsS -u "admin:$generated" -X PUT -H 'Content-Type: text/plain' --data-binary "$admin_password" \
			"$base/service/rest/v1/security/users/admin/change-password"
		compose exec -T nexus rm -f /nexus-data/admin.password
	fi
	# Community Edition: its EULA, unaccepted on a new volume, is accepted first.
	eula=$(api GET /service/rest/v1/system/eula)
	if [ "$(echo "$eula" | jq -r .accepted)" != true ]; then
		api POST /service/rest/v1/system/eula "$(echo "$eula" | jq -c '.accepted = true')"
	fi
	api PUT /service/rest/v1/security/anonymous \
		'{"enabled":false,"userId":"anonymous","realmName":"NexusAuthorizingRealm"}' >/dev/null
	# DockerToken and OciBearerToken: the bearer realms, for clients that ask /v2/token.
	api PUT /service/rest/v1/security/realms/active '["NexusAuthenticatingRealm","DockerToken","OciBearerToken"]'

	hosted docker lajstrom 8082
	hosted oci lajstrom-oci 8083

	role='{"id":"lajstrom","name":"lajstrom","description":"Lajstrom on its repositories",
		"privileges":["nx-repository-view-docker-lajstrom-*","nx-repository-view-oci-lajstrom-oci-*"],"roles":[]}'
	if [ "$(status /service/rest/v1/security/roles/lajstrom)" = 200 ]; then
		api PUT /service/rest/v1/security/roles/lajstrom "$role"
	else
		api POST /service/rest/v1/security/roles "$role" >/dev/null
	fi
	if [ "$(api GET "/service/rest/v1/security/users?userId=$user" | jq --arg u "$user" 'map(select(.userId == $u)) | length')" = 0 ]; then
		api POST /service/rest/v1/security/users "$(jq -nc --arg u "$user" --arg p "$password" '{
			userId: $u, firstName: "Lajstrom", lastName: "spike", emailAddress: "\($u)@example.invalid",
			password: $p, status: "active", roles: ["lajstrom"]}')" >/dev/null
	fi
	# The connectors open a moment after the repositories are saved.
	wait_v2 "localhost:${LAJSTROM_NEXUS_DOCKER_PORT:-5008}"
	wait_v2 "localhost:${LAJSTROM_NEXUS_OCI_PORT:-5015}"
	echo "$(curl -fsSI "$base/" | sed -n 's/^[Ss]erver: //p' | tr -d '\r'): writePolicy $policy, user $user;" \
		"docker repository lajstrom on http://localhost:${LAJSTROM_NEXUS_DOCKER_PORT:-5008}," \
		"OCI repository lajstrom-oci on http://localhost:${LAJSTROM_NEXUS_OCI_PORT:-5015}"
}

down() {
	compose down -v --remove-orphans
}

case "${1:-}" in
up) up ;;
down) down ;;
*)
	echo "usage: $0 up|down" >&2
	exit 2
	;;
esac
