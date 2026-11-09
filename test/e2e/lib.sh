# The assertion library of the end-to-end scenarios (#34, docs/testing.md). A scenario sources
# it right after set -euo pipefail:
#
#   . "$(dirname "$0")/lib.sh"
#
# and gets:
#
#   step TEXT...                one line, "ok   TEXT", for a step that passed (ok is its older name)
#   fail TEXT...                "FAIL <script>: TEXT", the calls that led there, and exit 1
#   assert_eq WANT GOT WHAT     fail unless WANT and GOT are the same string
#   expect WHAT WANT GOT        the same, in the order the first scenarios use
#   assert_status WANT ARGS...  curl -sS ARGS... and fail unless the status is WANT; the status
#                               is left in $E2E_STATUS, the body in $E2E_BODY. Its FAIL line
#                               has the arguments: credentials go in a file, -K FILE
#   e2e_credentials DIR         the user the scenario runs as, from LAJSTROM_E2E_USER and
#                               LAJSTROM_E2E_PASSWORD (run.sh makes one, an admin): $E2E_USER,
#                               $E2E_PASSWORD, and $E2E_CURLRC, a curl configuration in DIR with
#                               both, for -K; exits 2 when they are not set
#
# and an ERR trap: any command that fails where set -e would exit prints "FAIL <script>: exit N
# from: <the command>" and the calls that led there before the script exits, so that a broken
# endpoint names the script and the command even where no assertion was written (a curl that
# cannot connect, a jq that finds no JSON). test/e2e/run.sh repeats those lines in its report.
# The trap prints in the script's own shell only: a command that fails inside $(...) or a
# subshell is reported once, by the command around it. The trap prints a command as it is
# written, before expansion, so a secret held in a variable or a file stays out of it.
#
# It runs under bash 3.2, the bash of macOS, which gives no line number for a failure inside a
# function: the trap then names the function and the lines of its callers.

set -E

# e2e_script is the scenario's file name, the first word of every FAIL line.
e2e_script="$(basename "$0")"
_e2e_subshell="$BASH_SUBSHELL"

# _e2e_source FILE LINE prints LINE of FILE without its indentation, at most 160 characters.
_e2e_source() {
	sed -n "${2}{s/^[[:space:]]*//;p;}" "$1" 2>/dev/null | cut -c1-160 || true
}

# _e2e_calls FIRST prints one line for each call of the stack from frame FIRST outwards:
# "  at <file>:<line>[ in <function>]: <source of that line>". Frames in this file are left out.
_e2e_calls() {
	local i file line fn
	for ((i = $1 + 1; i < ${#FUNCNAME[@]}; i++)); do
		file="${BASH_SOURCE[$i]}"
		line="${BASH_LINENO[$((i - 1))]}"
		fn="${FUNCNAME[$i]}"
		[ "$file" != "${BASH_SOURCE[0]}" ] || continue
		if [ "$fn" = main ]; then fn=""; else fn=" in $fn"; fi
		printf '  at %s:%s%s: %s\n' "$(basename "$file")" "$line" "$fn" "$(_e2e_source "$file" "$line")"
	done
}

fail() {
	printf 'FAIL %s: %s\n' "$e2e_script" "$*" >&2
	_e2e_calls 1 >&2
	exit 1
}

step() { printf 'ok   %s\n' "$*"; }
ok() { step "$@"; }

assert_eq() { [ "$1" = "$2" ] || fail "$3: got '$2', want '$1'"; }
expect() { [ "$2" = "$3" ] || fail "$1: got '$3', want '$2'"; }

assert_status() {
	local want="$1" out
	shift
	out="$(curl -sS -w '\n%{http_code}' "$@")" || fail "curl $*: exit $?"
	E2E_STATUS="${out##*$'\n'}"
	E2E_BODY="${out%$'\n'*}"
	[ "$E2E_STATUS" = "$want" ] || fail "curl $*: status $E2E_STATUS, want $want: ${E2E_BODY:0:300}"
}

e2e_credentials() {
	if [ -z "${LAJSTROM_E2E_USER:-}" ] || [ -z "${LAJSTROM_E2E_PASSWORD:-}" ]; then
		echo "$e2e_script: set LAJSTROM_E2E_USER and LAJSTROM_E2E_PASSWORD to a user that may do what the scenario does (test/e2e/run.sh makes an admin)" >&2
		exit 2
	fi
	E2E_USER="$LAJSTROM_E2E_USER" E2E_PASSWORD="$LAJSTROM_E2E_PASSWORD" E2E_CURLRC="$1/e2e.curlrc"
	(umask 077 && printf 'user = "%s:%s"\n' "$E2E_USER" "$E2E_PASSWORD" >"$E2E_CURLRC")
}

# _e2e_err STATUS COMMAND LINE is the ERR trap.
_e2e_err() {
	[ "$BASH_SUBSHELL" = "$_e2e_subshell" ] || return 0
	local file="${BASH_SOURCE[1]}" fn="${FUNCNAME[1]}"
	printf 'FAIL %s: exit %s from: %s\n' "$e2e_script" "$1" "$2" >&2
	if [ "$fn" = main ]; then
		printf '  at %s:%s\n' "$(basename "$file")" "$3" >&2
	elif [ "${BASH_VERSINFO[0]}" -ge 4 ]; then
		printf '  at %s:%s in %s\n' "$(basename "$file")" "$3" "$fn" >&2
	else
		printf '  in %s of %s (bash %s gives no line inside a function)\n' "$fn" "$(basename "$file")" "$BASH_VERSION" >&2
	fi
	_e2e_calls 2 >&2
}
trap '_e2e_err $? "$BASH_COMMAND" $LINENO' ERR
