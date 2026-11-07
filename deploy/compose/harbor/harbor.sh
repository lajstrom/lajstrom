#!/bin/sh
# Harbor for the capability-matrix spike (issue #21, docs/registries/harbor.md): Harbor v2.15.4
# from its online installer, as the compose project "harbor" on http://localhost:5005, beside the
# dev stack and not part of it (its own project and network; make dev-down does not touch it).
# For development only: plain HTTP and the installer's default passwords (harbor.yml).
#
#   deploy/compose/harbor/harbor.sh up     download the installer, check its sha256, prepare,
#                                          docker compose up, and wait until Harbor is healthy
#   deploy/compose/harbor/harbor.sh down   docker compose down -v, and remove the installer,
#                                          the generated configuration, the data and the logs
#
# LAJSTROM_HARBOR_DIR (default deploy/compose/harbor/.work, ignored by git) holds the installer,
# what prepare generates from harbor.yml, the data volume and the logs: Harbor's containers
# bind-mount them, so on macOS it must be a path Docker shares (under $HOME or /private).
# LAJSTROM_HARBOR_GC_TIME_WINDOW_HOURS sets core's GC_TIME_WINDOW_HOURS (default 2: the garbage
# collector keeps blobs younger than that); 0 lets a GC run collect what was just pushed.
# Harbor's images are linux/amd64 only: on arm64 they run emulated (OrbStack, Docker Desktop).
# The generated docker-compose.yml publishes the port on 127.0.0.1 only (patched after
# prepare). Not committed: the generated files, which hold secrets prepare makes.
set -eu

version=v2.15.4
installer=harbor-online-installer-$version.tgz
sha256=eeb899cf7b626c925790adba87db9008dd1d73a693a72442651b5bdece9805c6
url=https://github.com/goharbor/harbor/releases/download/$version/$installer
port=5005

here=$(cd "$(dirname "$0")" && pwd -P)
dir=${LAJSTROM_HARBOR_DIR:-$here/.work}

compose() { docker compose -p harbor -f "$dir/harbor/docker-compose.yml" "$@"; }

check_sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		echo "$sha256  $1" | sha256sum -c -
	else
		echo "$sha256  $1" | shasum -a 256 -c -
	fi
}

up() {
	mkdir -p "$dir"
	dir=$(cd "$dir" && pwd -P)
	compose_file=$dir/harbor/docker-compose.yml
	if [ ! -f "$dir/$installer" ]; then
		curl -fsSL -o "$dir/$installer.part" "$url"
		mv "$dir/$installer.part" "$dir/$installer"
	fi
	(cd "$dir" && check_sha256 "$installer")
	rm -rf "$dir/harbor"
	tar -xzf "$dir/$installer" -C "$dir"
	mkdir -p "$dir/data" "$dir/log"
	sed -e "s|/set/by/harbor.sh/data|$dir/data|" -e "s|/set/by/harbor.sh/log|$dir/log|" \
		"$here/harbor.yml" >"$dir/harbor/harbor.yml"
	"$dir/harbor/prepare"
	# prepare publishes the port on every interface; the dev stack publishes on 127.0.0.1.
	sed -e "s|- $port:8080|- 127.0.0.1:$port:8080|" "$compose_file" >"$compose_file.local"
	mv "$compose_file.local" "$compose_file"
	if [ -n "${LAJSTROM_HARBOR_GC_TIME_WINDOW_HOURS:-}" ]; then
		echo "GC_TIME_WINDOW_HOURS=$LAJSTROM_HARBOR_GC_TIME_WINDOW_HOURS" >>"$dir/harbor/common/config/core/env"
	fi
	# Not up --wait: jobservice exits until core answers and restarts, which --wait takes for
	# a failure. Harbor's own health check waits for every component instead.
	compose up -d
	i=0
	until curl -fsS "http://localhost:$port/api/v2.0/health" 2>/dev/null | grep -q '"status":"healthy"}$'; do
		i=$((i + 1))
		if [ "$i" -gt 300 ]; then
			echo "Harbor is not healthy after 10 minutes; see docker compose -p harbor ps" >&2
			exit 1
		fi
		sleep 2
	done
	echo "Harbor $version: http://localhost:$port (admin, the password of harbor.yml)"
}

down() {
	[ -d "$dir" ] || return 0
	dir=$(cd "$dir" && pwd -P)
	if [ -f "$dir/harbor/docker-compose.yml" ]; then
		compose down -v --remove-orphans
	fi
	# Harbor's containers and prepare write as root and as their own users: remove what they
	# wrote through a container, so that no sudo is needed on Linux.
	if [ -e "$dir/harbor" ] || [ -e "$dir/data" ] || [ -e "$dir/log" ]; then
		docker run --rm --platform linux/amd64 -v "$dir:/work" --entrypoint /bin/sh "goharbor/prepare:$version" \
			-c 'rm -rf /work/harbor /work/data /work/log'
	fi
	rm -rf "$dir"
}

case "${1:-}" in
up) up ;;
down) down ;;
*)
	echo "usage: $0 up|down" >&2
	exit 2
	;;
esac
