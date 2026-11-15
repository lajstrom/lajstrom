#!/usr/bin/env bash
# End-to-end test of a terraform repo with real clients against a running Lajstrom (#66, ADR 0023;
# docs/spec/http-routing.md, terraform):
#
#   - three versions of the module greet/lajstrom published with curl, PUT
#     /terraform/<repo>/greet/lajstrom/<version>: 1.0.0, 1.1.0 and 2.0.0-rc.1, each a .tar.gz of a
#     main.tf whose output says its version; the same bytes again are the repeat, other bytes
#     under a version 409, a version with a v and a zip 400;
#   - the protocol by hand: /.well-known/terraform.json without credentials, the versions
#     document, the download's 204 with X-Terraform-Get and its token, the archive at that URL
#     without credentials, and without the token a 401;
#   - terraform init and apply of a root module that calls lajstrom.test:<port>/<repo>/greet/lajstrom
#     with version "~> 1.0", the module address on a TLS edge in front of the base URL (nginx with
#     a certificate made for the run), the credentials a deploy token of the repo with read in a
#     credentials block: the output says 1.1.0, the newest version the constraint takes; the same
#     with tofu when $TERRAFORM_E2E_TOFU_IMAGE is not empty;
#   - 1.1.0 deleted through DELETE /api/v1/repos/terraform/<repo>/packages/greet%2Flajstrom/versions/1.1.0:
#     the versions document and terraform init -upgrade then take 1.0.0;
#   - reindex: the repo deleted with ?force=true (the registry keeps the manifests), made again
#     empty, POST …/reindex: the versions document is the same bytes as before.
#
#   test/e2e/terraform.sh [<base url> [<repo>]]     default: http://localhost:8080 e2e-<run>
#
# The script creates the terraform repo through POST /api/v1/repos on the registry
# $TERRAFORM_E2E_REGISTRY (default local), and deletes it at the end; the user of LAJSTROM_E2E_USER
# and LAJSTROM_E2E_PASSWORD must be an admin (run.sh makes one; lib.sh, e2e_credentials). It needs
# curl, jq, openssl, tar and docker: the image $TERRAFORM_E2E_IMAGE (default Terraform 1.17.1 of
# Docker Hub, pinned by digest), $TERRAFORM_E2E_TOFU_IMAGE (default OpenTofu 1.13.8 of ghcr.io,
# pinned by digest; empty skips the tofu step) and $TERRAFORM_E2E_NGINX_IMAGE (default the nginx
# of the dev stack, pinned by digest), the TLS edge: Terraform reads a registry over https only.
# The containers use --network host, which reaches the base URL on Linux and under OrbStack, and
# the edge listens on a free port of 127.0.0.1 there, lajstrom.test in the clients' /etc/hosts. It
# prints one line per step and exits non-zero at the first failure, naming the assertion or the
# command (lib.sh). make e2e runs it (test/e2e/run.sh).
#
# Where the registry deletes no manifests, LAJSTROM_E2E_DELETE_MANIFEST=false (run.sh reads it
# from the server's log, #800), the delete of 1.1.0 is 501 UNAVAILABLE naming the registry, the
# versions document keeps it and terraform keeps 1.1.0; the reindex then rebuilds the repo with
# the three versions.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

base="${1:-http://localhost:8080}"
base="${base%/}"
run="$(date -u +%Y%m%d%H%M%S)-$RANDOM"
repo="${2:-e2e-$run}"
for tool in curl jq openssl tar docker; do
	command -v "$tool" >/dev/null || { echo "terraform.sh: $tool is required" >&2; exit 2; }
done
image="${TERRAFORM_E2E_IMAGE:-hashicorp/terraform:1.17.1@sha256:a5d5a5a892c98f60fcbfa81eac2a40fc19e7b3fa7719bccae10ff6111d22524a}"
tofu_image="${TERRAFORM_E2E_TOFU_IMAGE-ghcr.io/opentofu/opentofu:1.13.8@sha256:94fa7a782d08069278e4a1bab055cf94b95e870d1728bf5b43b00a7795bba6d0}"
nginx_image="${TERRAFORM_E2E_NGINX_IMAGE:-nginx:1.30.6-alpine@sha256:583b7f2fce1cdb8da5ad5c39ebd3c33b98b87d493190e548bdee1fa8c619c311}"
backend="${TERRAFORM_E2E_REGISTRY:-local}"
upstream="${base#*://}" # host:port of the base URL, which the edge proxies to

work="$(mktemp -d)"
edge="lajstrom-e2e-terraform-edge-$run"
created="" # set once the repo is created, for the cleanup
cleanup() {
	docker rm -f "$edge" >/dev/null 2>&1 || true
	if [ -n "$created" ]; then
		curl -sS -o /dev/null -K "$E2E_CURLRC" -X DELETE "$base/api/v1/repos/terraform/$repo?force=true" || true
	fi
	rm -rf "$work"
}
trap cleanup EXIT
e2e_credentials "$work"
mkdir -p "$work/client" "$work/edge"
module=greet/lajstrom
status="" # status of the last request

# sha256 prints the hex sha256 of a file.
sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# hdr NAME prints the value of the response header NAME (lower case) of the last request.
hdr() {
	tr -d '\r' <"$work/headers" | awk -v n="$1" 'index(tolower($0), n ": ") == 1 { sub(/^[^:]*: /, ""); v = $0 } END { print v }'
}

# get PATH OUT [CURL ARGS]: GET /terraform/<repo>/PATH as $E2E_USER into OUT, the headers to
# $work/headers; the status goes to $status.
get() {
	local p="$1" out="$2"
	shift 2
	status="$(curl -sS -K "$E2E_CURLRC" -o "$out" -D "$work/headers" -w '%{http_code}' "$@" "$base/terraform/$repo/$p")"
}

# api METHOD PATH [BODY]: a request of $E2E_USER to /api/v1/PATH; the body goes to $work/body.
api() {
	local data=()
	[ -z "${3:-}" ] || data=(-H 'Content-Type: application/json' -d "$3")
	status="$(curl -sS -K "$E2E_CURLRC" -o "$work/body" -w '%{http_code}' -X "$1" ${data[@]+"${data[@]}"} "$base/api/v1/$2")"
}

# put VERSION FILE: PUT FILE as VERSION of the module; the status goes to $status, the headers to
# $work/headers, the body to $work/body.
put() {
	status="$(curl -sS -K "$E2E_CURLRC" -o "$work/body" -D "$work/headers" -w '%{http_code}' -T "$2" "$base/terraform/$repo/$module/$1")"
}

# archive VERSION GREETING: write $work/VERSION.tar.gz, a module whose output says GREETING.
archive() {
	mkdir -p "$work/src/$1"
	printf 'output "greeting" {\n  value = "%s"\n}\n' "$2" >"$work/src/$1/main.tf"
	COPYFILE_DISABLE=1 tar -czf "$work/$1.tar.gz" -C "$work/src/$1" main.tf
}

# client NAME IMAGE LOG ARGS...: run IMAGE with ARGS in $work/client/NAME, as Terraform runs on a
# host: the edge's certificate as the trusted roots, the credentials block of $work/client/rc.
client() {
	local dir="$1" img="$2" log="$3"
	shift 3
	docker run --rm --network host --add-host lajstrom.test:127.0.0.1 --user "$(id -u):$(id -g)" \
		-e HOME=/tmp -e SSL_CERT_FILE=/w/edge.pem -e TF_CLI_CONFIG_FILE=/w/rc -e TF_IN_AUTOMATION=1 \
		-v "$work/client:/w" -w "/w/$dir" "$img" "$@" -no-color >"$work/$log" 2>&1
}

# versions OUT: the versions document of the module into OUT, which must answer 200.
versions() {
	get "$module/versions" "$1"
	expect "GET $module/versions" 200 "$status"
}

# T-01: the repo, and the three versions.
api POST repos "{\"format\":\"terraform\",\"name\":\"$repo\",\"registry\":\"$backend\"}"
expect "POST /api/v1/repos terraform/$repo ($(cat "$work/body"))" 201 "$status"
created=1
archive 1.0.0 "hello from 1.0.0"
archive 1.1.0 "hello from 1.1.0"
archive 2.0.0-rc.1 "hello from 2.0.0-rc.1"
for v in 1.0.0 1.1.0 2.0.0-rc.1; do
	put "$v" "$work/$v.tar.gz"
	expect "PUT $module/$v ($(cat "$work/body"))" 201 "$status"
	expect "PUT $module/$v Location" "/terraform/$repo/$module/$v/greet-lajstrom-$v.tar.gz" "$(hdr location)"
	expect "PUT $module/$v body" "$repo greet lajstrom $v greet-lajstrom-$v.tar.gz $v" \
		"$(jq -r '[.namespace, .name, .system, .version, .filename, .tag] | join(" ")' "$work/body")"
done
digest="$(jq -r .digest "$work/body")"
put 2.0.0-rc.1 "$work/2.0.0-rc.1.tar.gz"
expect "PUT $module/2.0.0-rc.1 again" 201 "$status"
expect "the digest of the repeat" "$digest" "$(hdr docker-content-digest)"
put 1.0.0 "$work/1.1.0.tar.gz"
expect "PUT other bytes under 1.0.0 ($(cat "$work/body"))" 409 "$status"
put v1.2.0 "$work/1.0.0.tar.gz"
expect "PUT $module/v1.2.0 ($(cat "$work/body"))" 400 "$status"
printf 'PK\003\004 not a tar.gz' >"$work/module.zip"
put 1.2.0 "$work/module.zip"
expect "PUT a zip ($(cat "$work/body"))" 400 "$status"
ok "POST /api/v1/repos terraform/$repo: 201; PUT 1.0.0, 1.1.0 and 2.0.0-rc.1: 201, the repeat the same digest, other bytes 409, v1.2.0 and a zip 400"

# T-02: the protocol, as Terraform reads it, by hand.
status="$(curl -sS -o "$work/discovery" -w '%{http_code}' "$base/.well-known/terraform.json")"
expect "GET /.well-known/terraform.json without credentials" 200 "$status"
expect "the service discovery" '{"modules.v1":"/terraform/"}' "$(cat "$work/discovery")"
versions "$work/versions.before"
expect "the versions document" '{"modules":[{"versions":[{"version":"1.0.0"},{"version":"1.1.0"},{"version":"2.0.0-rc.1"}]}]}' \
	"$(cat "$work/versions.before")"
get "$module/1.1.0/download" "$work/download"
expect "GET $module/1.1.0/download" 204 "$status"
location="$(hdr x-terraform-get)"
case "$location" in
"/terraform/$repo/$module/1.1.0/greet-lajstrom-1.1.0.tar.gz?token="?*) ;;
*) fail "X-Terraform-Get of 1.1.0 is not the archive's path with a token: $location" ;;
esac
status="$(curl -sS -o "$work/1.1.0.back" -w '%{http_code}' "$base$location")"
expect "GET the X-Terraform-Get of 1.1.0 without credentials" 200 "$status"
expect "the sha256 of the archive served" "$(sha256 "$work/1.1.0.tar.gz")" "$(sha256 "$work/1.1.0.back")"
status="$(curl -sS -o /dev/null -w '%{http_code}' "$base${location%%\?*}")"
expect "GET the archive without credentials nor token" 401 "$status"
ok "GET /.well-known/terraform.json: modules.v1 /terraform/; the versions document; download 204, X-Terraform-Get with a token that downloads the archive without credentials; without it 401"

# T-03: the TLS edge and the clients' configuration: a deploy token of the repo with read.
tls="$(free_port)" || fail "no free port for the TLS edge"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=lajstrom.test -addext subjectAltName=DNS:lajstrom.test \
	-keyout "$work/edge/key.pem" -out "$work/edge/cert.pem" >/dev/null 2>&1 || fail "openssl: the certificate of the edge"
cp "$work/edge/cert.pem" "$work/client/edge.pem"
cat >"$work/edge/nginx.conf" <<EOF
events {}
http {
  server {
    listen 127.0.0.1:$tls ssl;
    ssl_certificate /etc/nginx/edge/cert.pem;
    ssl_certificate_key /etc/nginx/edge/key.pem;
    client_max_body_size 0;
    location / {
      proxy_pass http://$upstream;
      proxy_set_header Host \$http_host;
      proxy_http_version 1.1;
      proxy_buffering off;
    }
  }
}
EOF
docker run -d --name "$edge" --network host -v "$work/edge:/etc/nginx/edge:ro" "$nginx_image" \
	nginx -c /etc/nginx/edge/nginx.conf -g 'daemon off;' >/dev/null
for _ in $(seq 1 50); do
	curl -sS -o /dev/null --cacert "$work/edge/cert.pem" --resolve "lajstrom.test:$tls:127.0.0.1" \
		"https://lajstrom.test:$tls/.well-known/terraform.json" 2>/dev/null && break
	sleep 0.2
done
api POST "repos/terraform/$repo/deploy-tokens" '{"name": "terraform clients", "scopes": ["read"]}'
expect "POST deploy-tokens ($(cat "$work/body"))" 201 "$status"
(umask 077 && printf 'credentials "lajstrom.test:%s" {\n  token = "%s"\n}\n' "$tls" "$(jq -r .secret "$work/body")" >"$work/client/rc")
for dir in terraform tofu; do
	mkdir -p "$work/client/$dir"
	printf 'module "greet" {\n  source  = "lajstrom.test:%s/%s/%s"\n  version = "~> 1.0"\n}\n\noutput "greeting" {\n  value = module.greet.greeting\n}\n' \
		"$tls" "$repo" "$module" >"$work/client/$dir/main.tf"
done
ok "the TLS edge at lajstrom.test:$tls, a deploy token of terraform/$repo with read in the credentials block"

# T-04: terraform init and apply: 1.1.0, the newest version of "~> 1.0".
client terraform "$image" terraform-init.log init -input=false || { cat "$work/terraform-init.log" >&2; fail "terraform init"; }
client terraform "$image" terraform-apply.log apply -auto-approve -input=false || { cat "$work/terraform-apply.log" >&2; fail "terraform apply"; }
client terraform "$image" terraform-output.log output -raw greeting || { cat "$work/terraform-output.log" >&2; fail "terraform output"; }
expect "the greeting of terraform apply" "hello from 1.1.0" "$(cat "$work/terraform-output.log")"
ok "terraform init and apply through lajstrom.test:$tls/$repo/$module: version 1.1.0, \"hello from 1.1.0\""
if [ -n "$tofu_image" ]; then
	client tofu "$tofu_image" tofu-init.log init -input=false || { cat "$work/tofu-init.log" >&2; fail "tofu init"; }
	client tofu "$tofu_image" tofu-apply.log apply -auto-approve -input=false || { cat "$work/tofu-apply.log" >&2; fail "tofu apply"; }
	client tofu "$tofu_image" tofu-output.log output -raw greeting || { cat "$work/tofu-output.log" >&2; fail "tofu output"; }
	expect "the greeting of tofu apply" "hello from 1.1.0" "$(cat "$work/tofu-output.log")"
	ok "tofu init and apply through lajstrom.test:$tls/$repo/$module: version 1.1.0"
fi

# T-05: 1.1.0 deleted through the API: the versions document and terraform take 1.0.0. Where the
# registry deletes no manifests (#352), the delete is refused, and nothing changes.
api DELETE "repos/terraform/$repo/packages/greet%2Flajstrom/versions/1.1.0"
if [ "${LAJSTROM_E2E_DELETE_MANIFEST:-}" = false ]; then
	expect "DELETE of 1.1.0 where the registry deletes no manifests ($(cat "$work/body"))" 501 "$status"
	versions "$work/versions.deleted"
	expect "the versions document after the refused delete" "$(cat "$work/versions.before")" "$(cat "$work/versions.deleted")"
	ok "DELETE …/packages/greet%2Flajstrom/versions/1.1.0 where the registry deletes no manifests: 501; the versions document the same"
else
	expect "DELETE of 1.1.0 ($(cat "$work/body"))" 204 "$status"
	versions "$work/versions.deleted"
	expect "the versions document after the delete" '{"modules":[{"versions":[{"version":"1.0.0"},{"version":"2.0.0-rc.1"}]}]}' \
		"$(cat "$work/versions.deleted")"
	client terraform "$image" terraform-upgrade.log init -upgrade -input=false || { cat "$work/terraform-upgrade.log" >&2; fail "terraform init -upgrade"; }
	client terraform "$image" terraform-apply2.log apply -auto-approve -input=false || { cat "$work/terraform-apply2.log" >&2; fail "terraform apply after the delete"; }
	client terraform "$image" terraform-output2.log output -raw greeting || { cat "$work/terraform-output2.log" >&2; fail "terraform output"; }
	expect "the greeting after the delete" "hello from 1.0.0" "$(cat "$work/terraform-output2.log")"
	ok "DELETE …/packages/greet%2Flajstrom/versions/1.1.0: 204; the versions document and terraform init -upgrade take 1.0.0"
fi

# T-06: reindex rebuilds the repo from the registry: the same versions document.
api DELETE "repos/terraform/$repo?force=true"
expect "DELETE /api/v1/repos/terraform/$repo?force=true ($(cat "$work/body"))" 204 "$status"
api POST repos "{\"format\":\"terraform\",\"name\":\"$repo\",\"registry\":\"$backend\"}"
expect "POST /api/v1/repos terraform/$repo again ($(cat "$work/body"))" 201 "$status"
get "$module/versions" "$work/none"
expect "GET $module/versions in the repo made again" 404 "$status"
api POST "repos/terraform/$repo/reindex"
expect "POST /api/v1/repos/terraform/$repo/reindex ($(cat "$work/body"))" 202 "$status"
for _ in $(seq 1 120); do
	api GET "repos/terraform/$repo/reindex"
	[ "$(jq -r .state "$work/body")" = running ] || break
	sleep 0.5
done
expect "the state of the reindex of terraform/$repo ($(head -c 300 "$work/body"))" done "$(jq -r .state "$work/body")"
jq -e '.report | .errors == [] and .foreign == []' "$work/body" >/dev/null || fail "the report of the reindex: $(jq -c .report "$work/body")"
versions "$work/versions.after"
expect "the versions document after the reindex" "$(cat "$work/versions.deleted")" "$(cat "$work/versions.after")"
ok "reindex of terraform/$repo made again: the same versions document"
