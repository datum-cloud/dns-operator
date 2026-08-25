#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
#
# Live smoke test for the `datumctl dns` plugin.
#
# Runs the plugin against a real Datum project and asserts on what comes back.
# Every step checks an exit code and greps the output for the thing that proves
# it worked; a step that only runs a command and prints its output is not a
# test and is not counted as one.
#
# See hack/smoke-datumctl-dns.md for what each step proves and why it is here.
#
# Usage:
#   DNS_SMOKE_DOMAIN=zone-i-control.example hack/smoke-datumctl-dns.sh
#   hack/smoke-datumctl-dns.sh --domain zone-i-control.example
#   hack/smoke-datumctl-dns.sh --read-only          # no domain needed, mutates nothing
#   hack/smoke-datumctl-dns.sh --domain d --skip-bulk
#   hack/smoke-datumctl-dns.sh --domain d --keep    # leave the zone behind
#
# There is deliberately NO DEFAULT DOMAIN. Creating a DNSZone claims that domain
# globally in the platform's accounting store, so a guessed domain is not a
# harmless mistake — it takes a name away from whoever actually owns it. The
# script refuses to run without one.

# pipefail is load-bearing, not decoration: cleanup pipes the teardown delete
# through sed to indent it, and without pipefail that pipeline reports sed's
# status, so a failed zone deletion would print "zone removed" and exit clean.
set -uo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PLUGIN_BIN="${REPO_ROOT}/bin/datumctl-dns"

DOMAIN="${DNS_SMOKE_DOMAIN:-}"
READ_ONLY=0
SKIP_BULK=0
KEEP=0
VERBOSE=0

# Set once the zone is known to exist, so the teardown trap knows whether there
# is anything to clean up.
ZONE_CREATED=0

PASS=0
FAIL=0
SKIP=0
PROBE=0
FAILED_STEPS=()

usage() {
	sed -n '3,30p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--domain)
		DOMAIN="${2:-}"
		shift 2
		;;
	--domain=*)
		DOMAIN="${1#*=}"
		shift
		;;
	--read-only | --dry-run)
		READ_ONLY=1
		shift
		;;
	--skip-bulk)
		SKIP_BULK=1
		shift
		;;
	--keep)
		KEEP=1
		shift
		;;
	--verbose)
		VERBOSE=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "unknown argument: $1" >&2
		usage >&2
		exit 2
		;;
	esac
done

# ---------------------------------------------------------------------------
# Output helpers
# ---------------------------------------------------------------------------

if [[ -t 1 ]]; then
	C_RESET=$'\033[0m'
	C_PASS=$'\033[32m'
	C_FAIL=$'\033[31m'
	C_SKIP=$'\033[33m'
	C_HEAD=$'\033[1m'
else
	C_RESET='' C_PASS='' C_FAIL='' C_SKIP='' C_HEAD=''
fi

CURRENT_STEP=""

group() { printf '\n%s=== %s ===%s\n' "$C_HEAD" "$1" "$C_RESET"; }
step() {
	CURRENT_STEP="$1"
	printf '\n%s\n' "$1"
}
pass() {
	PASS=$((PASS + 1))
	printf '  %sPASS%s %s\n' "$C_PASS" "$C_RESET" "$1"
}
fail() {
	FAIL=$((FAIL + 1))
	FAILED_STEPS+=("${CURRENT_STEP} :: $1")
	printf '  %sFAIL%s %s\n' "$C_FAIL" "$C_RESET" "$1"
	printf '       exit=%s\n' "${RC:-?}"
	printf '       output:\n'
	printf '%s\n' "${BOTH:-}" | sed 's/^/         | /' | head -30
}
skip() {
	SKIP=$((SKIP + 1))
	printf '  %sSKIP%s %s\n' "$C_SKIP" "$C_RESET" "$1"
}
# probe records an observation that has no correct answer to assert against —
# both outcomes are informative, neither is a failure. Reported separately so a
# green run is never mistaken for "everything was checked".
probe() {
	PROBE=$((PROBE + 1))
	printf '  %sPROBE%s %s\n' "$C_SKIP" "$C_RESET" "$1"
}

# ---------------------------------------------------------------------------
# Assertion harness
#
# cap runs a command, capturing stdout, stderr, the combination, and the exit
# code into globals the assert_* helpers read. Nothing here uses `set -e`: the
# negative cases are supposed to fail, and the point is to check how.
# ---------------------------------------------------------------------------

RC=0
OUT=""
ERR=""
BOTH=""

cap() {
	local out_file err_file
	out_file="$(mktemp)"
	err_file="$(mktemp)"
	if [[ $VERBOSE -eq 1 ]]; then
		printf '  $ %s\n' "$*"
	fi
	"$@" >"$out_file" 2>"$err_file"
	RC=$?
	OUT="$(cat "$out_file")"
	ERR="$(cat "$err_file")"
	BOTH="${OUT}
${ERR}"
	rm -f "$out_file" "$err_file"
	return 0
}

# cap_stdin is cap with a here-string fed to the command's stdin. Note that a
# here-string is a pipe, which util.NonInteractive treats as unanswerable — see
# the teardown group.
cap_stdin() {
	local input="$1"
	shift
	local out_file err_file
	out_file="$(mktemp)"
	err_file="$(mktemp)"
	if [[ $VERBOSE -eq 1 ]]; then
		printf '  $ %s  <<< %q\n' "$*" "$input"
	fi
	"$@" >"$out_file" 2>"$err_file" <<<"$input"
	RC=$?
	OUT="$(cat "$out_file")"
	ERR="$(cat "$err_file")"
	BOTH="${OUT}
${ERR}"
	rm -f "$out_file" "$err_file"
	return 0
}

assert_rc() {
	local want="$1" desc="${2:-exit code is $1}"
	if [[ "$RC" == "$want" ]]; then
		pass "$desc"
	else
		fail "$desc (got exit $RC, want $want)"
	fi
}

assert_rc_nonzero() {
	local desc="${1:-command fails}"
	if [[ "$RC" != "0" ]]; then
		pass "$desc (exit $RC)"
	else
		fail "$desc (got exit 0)"
	fi
}

assert_match() {
	local pattern="$1" desc="${2:-output matches /$1/}"
	if grep -Eq -- "$pattern" <<<"$BOTH"; then
		pass "$desc"
	else
		fail "$desc"
	fi
}

assert_no_match() {
	local pattern="$1" desc="${2:-output does not match /$1/}"
	if grep -Eq -- "$pattern" <<<"$BOTH"; then
		fail "$desc"
	else
		pass "$desc"
	fi
}

assert_eq() {
	local got="$1" want="$2" desc="$3"
	if [[ "$got" == "$want" ]]; then
		pass "$desc"
	else
		fail "$desc (got ${#got} bytes, want ${#want} bytes)"
	fi
}

# assert_count checks how many lines of BOTH match a pattern, which is how the
# append-vs-replace and single-valued steps prove that nothing was lost.
assert_count() {
	local pattern="$1" want="$2" desc="$3" got
	got="$(grep -Ec -- "$pattern" <<<"$BOTH")"
	if [[ "$got" == "$want" ]]; then
		pass "$desc"
	else
		fail "$desc (matched $got lines, want $want)"
	fi
}

# ---------------------------------------------------------------------------
# Plugin invocation
# ---------------------------------------------------------------------------

PLUGIN_DIR=""

dns() {
	if [[ $READ_ONLY -eq 1 ]]; then
		# Read-only mode closes stdin. Steps 1.4 to 1.7 all traverse
		# PersistentPreRunE, and on a project where DNS is not entitled, with a
		# terminal attached, that prompts to enable it — answering yes CREATES a
		# ServiceEntitlement, which is a real write. Handing the plugin
		# /dev/null makes util.NonInteractive true, so the pre-flight returns an
		# error instead of offering to write something.
		DATUMCTL_PLUGINS_DIR="$PLUGIN_DIR" datumctl dns "$@" </dev/null
	else
		DATUMCTL_PLUGINS_DIR="$PLUGIN_DIR" datumctl dns "$@"
	fi
}

# require_sub reports whether a subcommand is available to test, and tells the
# three cases apart. A command that does not exist yet is a SKIP; a `--help`
# that itself fails is a FAILURE, because otherwise a regression that breaks
# `dns zone --help` — a panic in init, or the entitlement pre-flight starting to
# fire there — would silently convert every guarded step into a skip and the run
# would still look clean.
HELP_OUT=""
HELP_RC=0

require_sub() {
	local parent="$1" name="$2"
	HELP_OUT="$(DATUMCTL_PLUGINS_DIR="$PLUGIN_DIR" datumctl dns "$parent" --help 2>&1)"
	HELP_RC=$?
	if [[ $HELP_RC -ne 0 ]]; then
		RC=$HELP_RC
		BOTH="$HELP_OUT"
		fail "\`dns $parent --help\` exits $HELP_RC, so whether $name exists cannot be determined"
		return 1
	fi
	if grep -Eq "^[[:space:]]+${name}([[:space:],]|\$)" <<<"$HELP_OUT"; then
		return 0
	fi
	skip "dns $parent $name is not implemented yet"
	return 1
}

# ---------------------------------------------------------------------------
# Teardown
#
# In a trap, so an interrupted run does not strand a DNSZone — and therefore a
# global domain claim — behind it. The trap is idempotent and safe to run when
# nothing was created.
# ---------------------------------------------------------------------------

cleanup() {
	local rc=$?
	trap - EXIT INT TERM

	# The zone teardown dispatches through PLUGIN_DIR, so the temp directories
	# are removed after it, not before.
	if [[ $ZONE_CREATED -eq 1 && $KEEP -eq 0 ]]; then
		printf '\n%s=== teardown: removing zone %s ===%s\n' "$C_HEAD" "$DOMAIN" "$C_RESET"
		# --yes because the trap may be running from a signal, where no prompt
		# can be answered. Best effort: report, never mask the original failure.
		if DATUMCTL_PLUGINS_DIR="$PLUGIN_DIR" datumctl dns zone delete "$DOMAIN" --yes 2>&1 |
			sed 's/^/  /'; then
			printf '  zone removed\n'
		else
			printf '  %sWARNING%s could not remove zone %s — if it exists, delete it by hand:\n' \
				"$C_FAIL" "$C_RESET" "$DOMAIN"
			printf '    datumctl dns zone delete %s --yes\n' "$DOMAIN"
		fi
	elif [[ $ZONE_CREATED -eq 1 ]]; then
		printf '\n  zone %s left in place (--keep). Remove it with:\n' "$DOMAIN"
		printf '    datumctl dns zone delete %s --yes\n' "$DOMAIN"
	fi

	[[ -n "${PLUGIN_DIR:-}" && -d "${PLUGIN_DIR:-}" ]] && rm -rf "$PLUGIN_DIR"
	[[ -n "${WORK_DIR:-}" && -d "${WORK_DIR:-}" ]] && rm -rf "$WORK_DIR"

	summary

	# A failed assertion must make the process fail, whatever route brought us
	# here. cleanup is the single exit point — it runs on EXIT, so it sees every
	# path including an interrupt — and it is authoritative: a non-zero FAIL
	# beats the incoming status. Without this the script exits 0 with failures
	# on the board, and wiring that into CI produces a job that is green forever
	# no matter what breaks.
	if [[ $FAIL -gt 0 ]]; then
		exit 1
	fi
	exit "$rc"
}

summary() {
	printf '\n%s=== summary ===%s\n' "$C_HEAD" "$C_RESET"
	printf '  %d passed, %d failed, %d skipped, %d probes\n' "$PASS" "$FAIL" "$SKIP" "$PROBE"
	if [[ ${#FAILED_STEPS[@]} -gt 0 ]]; then
		printf '\n  failures:\n'
		printf '    %s\n' "${FAILED_STEPS[@]}"
	fi
}

# ---------------------------------------------------------------------------
# Preconditions
# ---------------------------------------------------------------------------

if [[ $READ_ONLY -eq 0 && -z "$DOMAIN" ]]; then
	cat >&2 <<'EOF'
Error: no domain given

This script creates a real DNSZone, and creating one claims that domain
globally in the platform's accounting store. There is no default and none will
be invented: supply a domain you control.

Fix:   DNS_SMOKE_DOMAIN=zone-i-control.example hack/smoke-datumctl-dns.sh
       or hack/smoke-datumctl-dns.sh --domain zone-i-control.example
       or hack/smoke-datumctl-dns.sh --read-only   (mutates nothing, no domain needed)
EOF
	exit 2
fi

if [[ -n "$DOMAIN" ]]; then
	# A domain with a trailing dot or uppercase would be a different kind of
	# test than this script is. spec.domainName is lowercase-only at admission.
	if [[ "$DOMAIN" != "$(tr '[:upper:]' '[:lower:]' <<<"$DOMAIN")" || "$DOMAIN" == *. ]]; then
		echo "Error: give the domain lowercase and without a trailing dot" >&2
		exit 2
	fi
fi

for tool in datumctl go jq python3; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "Error: $tool is required and not on PATH" >&2
		exit 2
	fi
done

# CI=1 forces util.NonInteractive true everywhere, which changes the behaviour
# of every confirmation gate. Say so rather than producing confusing results.
if [[ -n "${CI:-}" ]]; then
	printf '%sNOTE%s CI is set: every confirmation gate will take its non-interactive path.\n' \
		"$C_SKIP" "$C_RESET"
fi

WORK_DIR="$(mktemp -d)"
PLUGIN_DIR="$(mktemp -d)"
trap cleanup EXIT INT TERM

# unchunk.py reverses the zone-file encoding of a TXT value, so step 4.5 can
# compare what was stored against what was written. Kept as a file rather than
# an inline -c so the escaping is readable.
cat >"${WORK_DIR}/unchunk.py" <<'PYEOF'
"""Reverse the zone-file encoding of a TXT value.

Reads the stored txt.content on argv[1] and writes the logical string it
encodes to stdout: concatenate every quoted character-string, resolving
backslash escapes. Content that is not quoted at all is passed through, which
is what an unchunked short value looks like.
"""
import sys

s = sys.argv[1]
if not s.startswith('"'):
    sys.stdout.write(s)
    sys.exit(0)

out = []
i = 0
n = len(s)
while i < n:
    if s[i] != '"':
        i += 1
        continue
    i += 1
    while i < n and s[i] != '"':
        if s[i] == "\\" and i + 1 < n:
            out.append(s[i + 1])
            i += 2
            continue
        out.append(s[i])
        i += 1
    i += 1
sys.stdout.write("".join(out))
PYEOF

ABSENT_ZONE="smoke-absent-$$.example"

printf '%s' "$C_HEAD"
cat <<EOF
datumctl dns smoke test
  mode:    $([[ $READ_ONLY -eq 1 ]] && echo "read-only (mutates nothing)" || echo "full (CREATES AND DELETES A ZONE)")
  domain:  ${DOMAIN:-<none: read-only>}
  plugins: $PLUGIN_DIR
EOF
printf '%s' "$C_RESET"

# ===========================================================================
group "0. Build and dispatch  [READ-ONLY]"
# ===========================================================================

step "0.1 [READ] the plugin builds"
cap make -C "$REPO_ROOT" build-plugin
assert_rc 0 "make build-plugin succeeds"
if [[ -x "$PLUGIN_BIN" ]]; then
	pass "bin/datumctl-dns exists and is executable"
else
	fail "bin/datumctl-dns was not produced"
	exit 1
fi

step "0.2 [READ] the binary answers the plugin manifest before Cobra parses anything"
cap "$PLUGIN_BIN" --plugin-manifest
assert_rc 0 "--plugin-manifest exits 0"
assert_match '"name":[[:space:]]*"dns"' "manifest names the plugin"
# The SDK tag is `json:"api_version"` (datumctl/plugin/manifest.go); the Go
# field name is APIVersion, and grepping for that camelCase spelling would have
# been testing a key that has never existed.
assert_match '"api_version":[[:space:]]*1' "manifest declares the plugin API version"

step "0.3 [READ] datumctl dispatches to it out of DATUMCTL_PLUGINS_DIR"
install -m 0755 "$PLUGIN_BIN" "$PLUGIN_DIR/datumctl-dns"
cap dns --help
assert_rc 0 "datumctl dns --help exits 0"
assert_match 'Manage DNS zones and records' "help comes from the plugin, not the host"
assert_match '(^|[[:space:]])zone([[:space:],]|$)' "the zone noun is registered"
assert_match '(^|[[:space:]])record([[:space:],]|$)' "the record noun is registered"

step "0.4 [READ] version needs no entitlement and no project"
cap dns version
assert_rc 0 "dns version exits 0"

# ===========================================================================
group "1. Error contract  [READ-ONLY]"
# ===========================================================================

step "1.1 [READ] an unknown flag is a usage failure, not a generic one"
cap dns zone list --no-such-flag
assert_rc 2 "unknown flag exits 2"
assert_match 'DNS_USAGE' "exit line carries the symbolic name"
assert_match '^Fix:' "a Fix line is offered"
assert_match '[-]-help' "the fix names --help"

step "1.2 [READ] a typo'd noun is rejected with a suggestion"
cap dns zoen list
assert_rc 2 "typo'd noun exits 2"
assert_match 'unknown command' "says what was wrong"
assert_match 'Did you mean' "offers the nearest match"
assert_match 'zone' "the suggestion is the right one"

step "1.3 [READ] a typo'd verb is rejected with a suggestion"
cap dns zone lst
assert_rc 2 "typo'd verb exits 2"
assert_match 'Did you mean|unknown command' "says what was wrong"

step "1.4 [READ] a nonexistent zone is 4, not 1"
cap dns zone describe "$ABSENT_ZONE"
assert_rc 4 "missing zone exits 4"
assert_match 'DNS_NOT_FOUND' "exit line carries the symbolic name"
assert_match '^Fix:' "a Fix line is offered"

step "1.5 [READ] listing zones works and tallies"
cap dns zone list
assert_rc 0 "zone list exits 0"
assert_match 'NAME[[:space:]]+STATUS|No DNS zones' "prints a table or the empty state"

step "1.6 [READ] completion never errors and never falls back to filenames"
cap dns __complete zone describe ""
assert_rc 0 "__complete exits 0"
assert_match ':4$' "returns ShellCompDirectiveNoFileComp"

step "1.7 [READ] -o json on a list is machine-readable"
cap dns zone list -o json
assert_rc 0 "zone list -o json exits 0"
if jq -e . >/dev/null 2>&1 <<<"$OUT"; then
	pass "output parses as JSON"
else
	fail "output does not parse as JSON"
fi

if [[ $READ_ONLY -eq 1 ]]; then
	printf '\n%sread-only mode: stopping before anything mutates.%s\n' "$C_HEAD" "$C_RESET"
	exit $((FAIL > 0 ? 1 : 0))
fi

# ===========================================================================
group "2. Zone lifecycle  [MUTATES]"
# ===========================================================================

step "2.1 [MUTATES: no] --dry-run validates without creating"
cap dns zone create "$DOMAIN" --dry-run
assert_rc 0 "zone create --dry-run exits 0"
assert_match 'dry run, nothing was created' "says plainly that nothing happened"
cap dns zone list -o name
assert_no_match "^${DOMAIN}\$" "the zone really was not created"

step "2.2 [MUTATES] creating the zone"
# Set BEFORE the attempt, not after it. The zone exists server-side the moment
# the request lands, but --timeout keeps the client waiting for nameservers for
# up to two minutes afterwards — and Ctrl-C during that wait is the single most
# likely way anyone interrupts this script. Setting the flag on success would
# fire the trap with nothing to clean up, stranding the zone and its global
# domain claim, and would suppress the manual-recovery message too. Deleting a
# zone that was never created is a harmless no-op that cleanup already handles,
# so the only safe direction to be wrong in is this one.
ZONE_CREATED=1
cap dns zone create "$DOMAIN" --timeout 120s
assert_rc 0 "zone create exits 0"
assert_match "zone/${DOMAIN} created" "confirms the creation"
assert_match 'Set these nameservers at your domain registrar' "prints the registrar instructions"
assert_match 'Next steps:' "offers the next command"

step "2.3 [MUTATES: attempted] creating it twice is a conflict, not a silent no-op"
cap dns zone create "$DOMAIN" --no-wait
assert_rc_nonzero "the second create fails"
assert_match 'DNS_CONFLICT|already exists' "the failure says the zone is already there"

step "2.4 [READ] the zone appears in the list"
cap dns zone list
assert_rc 0 "zone list exits 0"
assert_match "^${DOMAIN}[[:space:]]" "the new zone is listed"

step "2.5 [READ] describe renders the zone"
cap dns zone describe "$DOMAIN"
assert_rc 0 "zone describe exits 0"
assert_match "^Zone[[:space:]]+${DOMAIN}" "names the zone"
assert_match '^Class' "shows the class"
assert_match '^Status' "shows status"
assert_match '^Delegation' "shows delegation state"
assert_match 'datum-external-global-dns' "the only class that exists is the one used"

step "2.6 [READ] nameservers are assigned"
cap dns zone nameservers "$DOMAIN"
assert_rc 0 "zone nameservers exits 0"
assert_match '[a-z0-9-]+\.[a-z]+' "at least one nameserver is printed"

step "2.7 [READ] --check runs a live resolution and reports a verdict"
cap dns zone nameservers "$DOMAIN" --check --timeout 10s
# Delegation almost certainly is NOT complete during a smoke run: the registrar
# has not been pointed at these nameservers. Both answers are correct, so this
# is a probe on the verdict being rendered at all, not on which verdict it is.
if grep -Eq 'delegat|nameserver|NS' <<<"$BOTH"; then
	probe "--check produced a delegation verdict (exit $RC) — check by eye, both answers are legitimate here"
else
	fail "--check produced no verdict"
fi

step "2.8 [READ] -o json emits the raw object, not the table"
cap dns zone describe "$DOMAIN" -o json
assert_rc 0 "zone describe -o json exits 0"
if jq -e '.kind == "DNSZone"' >/dev/null 2>&1 <<<"$OUT"; then
	pass "the raw DNSZone object is emitted"
else
	fail "-o json did not emit a DNSZone"
fi

step "2.9 [READ] the zone positional accepts the object name as well as the domain"
cap dns zone describe "$DOMAIN" -o json
assert_rc 0 "zone describe -o json exits 0"
ZONE_OBJECT="$(jq -r '.metadata.name // empty' <<<"$OUT" 2>/dev/null)"
if [[ -z "$ZONE_OBJECT" ]]; then
	skip "could not read the DNSZone object name"
elif [[ "$ZONE_OBJECT" == "$DOMAIN" ]]; then
	skip "the object name and the domain are identical here, so the two spellings cannot be told apart"
else
	cap dns record list "$ZONE_OBJECT"
	assert_rc 0 "record list resolves the zone by object name"
fi

# ===========================================================================
group "3. Record happy path  [MUTATES]"
# ===========================================================================

step "3.1 [MUTATES] create one A record"
cap dns record create "$DOMAIN" www A 203.0.113.10
assert_rc 0 "record create exits 0"
assert_match "record/${DOMAIN} A www created" "confirms the creation in object terms"
assert_match 'www +Auto +IN +A +203\.0\.113\.10' "echoes the value back as a presentation line"

step "3.2 [READ] it comes back, with TTL Auto"
cap dns record list "$DOMAIN" --name www --type A
assert_rc 0 "record list exits 0"
assert_match '^www[[:space:]]+A[[:space:]]+Auto[[:space:]]+203\.0\.113\.10' \
	"an omitted --ttl renders as Auto, not as 300 or a blank"

step "3.3 [MUTATES] create appends rather than replacing"
cap dns record create "$DOMAIN" www A 203.0.113.11
assert_rc 0 "the second create exits 0"
cap dns record list "$DOMAIN" --name www --type A
assert_count '^www[[:space:]]+A[[:space:]]' 2 "both values are present"

step "3.4 [MUTATES: attempted] creating a duplicate value fails and loses nothing"
cap dns record create "$DOMAIN" www A 203.0.113.10
assert_rc 5 "a duplicate create exits 5"
assert_match 'DNS_CONFLICT' "exit line carries the symbolic name"
assert_match "already has the A value" "the message names the value that is already there"
assert_match 'record set' "the fix points at the command that would replace it"
cap dns record list "$DOMAIN" --name www --type A
assert_count '^www[[:space:]]+A[[:space:]]' 2 "the record set still has exactly the two values"

step "3.5 [MUTATES] set replaces every value at the name"
cap dns record set "$DOMAIN" www A 203.0.113.20 --ttl 300
assert_rc 0 "record set exits 0"
cap dns record list "$DOMAIN" --name www --type A
assert_count '^www[[:space:]]+A[[:space:]]' 1 "set collapsed two values to one"
assert_match '^www[[:space:]]+A[[:space:]]+300[[:space:]]+203\.0\.113\.20' "the new value and TTL are live"

step "3.6 [MUTATES] --ttl accepts a duration and does not snap to a ladder"
cap dns record create "$DOMAIN" api A 203.0.113.30 --ttl 5m
assert_rc 0 "record create --ttl 5m exits 0"
cap dns record list "$DOMAIN" --name api --type A
assert_match '^api[[:space:]]+A[[:space:]]+300[[:space:]]' "5m became 300 seconds"
cap dns record create "$DOMAIN" api2 A 203.0.113.31 --ttl 240
assert_rc 0 "record create --ttl 240 exits 0"
cap dns record list "$DOMAIN" --name api2 --type A
assert_match '^api2[[:space:]]+A[[:space:]]+240[[:space:]]' "240 stayed 240 and was not rounded to 300"

step "3.7 [READ] describe shows the named fields for a value entered positionally"
cap dns record describe "$DOMAIN" www A
assert_rc 0 "record describe exits 0"
assert_match 'Address' "a flat type describes as a named field"
assert_match '203\.0\.113\.20' "the value is shown"

step "3.8 [MUTATES] a structured type entered by flags echoes in presentation format"
cap dns record create "$DOMAIN" @ MX --preference 10 --exchange "mail.${DOMAIN}."
assert_rc 0 "record create MX by flags exits 0"
assert_match "10 mail\.${DOMAIN}\." "the echo is the presentation form, teaching the other notation"
cap dns record describe "$DOMAIN" @ MX
assert_rc 0 "record describe MX exits 0"
assert_match 'Preference' "describe teaches the named fields back"
assert_match 'Exchange' "both MX fields are labelled"

step "3.9 [MUTATES] the same value pasted as presentation format parses identically"
cap dns record create "$DOMAIN" _sip._tcp SRV "10 5 5060 sip.${DOMAIN}."
assert_rc 0 "record create SRV from presentation format exits 0"
cap dns record list "$DOMAIN" --name _sip._tcp --type SRV
assert_match "10 5 5060 sip\.${DOMAIN}\." "the value round-trips unchanged"

step "3.10 [MUTATES] HTTPS parameters serialize in the backend's canonical order"
cap dns record create "$DOMAIN" svc HTTPS --priority 1 --target . --param port=443 --param alpn=h3,h2
assert_rc 0 "record create HTTPS exits 0"
cap dns record list "$DOMAIN" --name svc --type HTTPS
# alpn ranks before port in encodeSvcbParams regardless of the order given.
assert_match '1 \. alpn=h3,h2 port=443' "params are reordered into the canonical form"

# ===========================================================================
step "3.11 [MUTATES] --line takes a whole dig-shaped line"
cap dns record create "$DOMAIN" --line "pasted 300 IN A 203.0.113.70"
assert_rc 0 "record create --line exits 0"
assert_match "record/${DOMAIN} A pasted created" "name and type are read out of the line"
cap dns record list "$DOMAIN" --name pasted --type A
assert_match '^pasted +A +300 +203\.0\.113\.70' "the TTL from the line is applied, not Auto"

step "3.12 [MUTATES: no] record --dry-run shows the diff and changes nothing"
cap dns record create "$DOMAIN" dryrun A 203.0.113.80 --dry-run
assert_rc 0 "record create --dry-run exits 0"
assert_match 'Dry run — no changes were made' "says plainly that nothing happened"
assert_match '^  \+ ' "shows the arriving value with a + in compute's diff vocabulary"
cap dns record list "$DOMAIN" --type A
assert_no_match '^dryrun[[:space:]]' "the record really was not created"

step "3.13 [MUTATES: attempted] a missing value is a usage error"
cap dns record create "$DOMAIN" novalue A
assert_rc 2 "a create with no value exits 2"
assert_match 'DNS_USAGE' "exit line carries the symbolic name"
assert_match 'a value is required for a A record' "the error names what is missing"

group "4. Semantic landmines  [MUTATES / negative]"
#
# These are the cases the plugin exists to catch. Each one is a mistake the API
# server accepts and the DNS backend then mishandles.
# ===========================================================================

step "4.1 [MUTATES: attempted] a name that already spells out the zone is rejected"
# pdns.QualifyOwner appends the zone to anything without a trailing dot, so
# "www.<domain>" would become "www.<domain>.<domain>." — out of zone.
cap dns record create "$DOMAIN" "www.${DOMAIN}" A 203.0.113.40
assert_rc 2 "the zone-suffixed name exits 2, client-side"
assert_match 'already includes the zone domain' "the error names the actual problem"
assert_match '^Fix:' "a Fix line is offered"
assert_match '"www"' "the fix offers the bare label"
assert_match 'trailing dot' "the fix also offers the absolute spelling"
cap dns record list "$DOMAIN" --type A
assert_no_match "www\.${DOMAIN}" "nothing was created"

step "4.2 [MUTATES: attempted] a target field without a trailing dot is rejected"
# qualifyIfNeeded absolutizes a target by appending a dot and nothing else, so
# a relative "lb" becomes the root-relative "lb.", never "lb.<domain>.".
cap dns record create "$DOMAIN" cdn CNAME lb
assert_rc 2 "the relative target exits 2, client-side"
assert_match 'not a fully qualified domain name' "the error names the actual problem"
assert_match '^Fix:' "a Fix line is offered"
assert_match "lb\.${DOMAIN}\." "the fix suggests the likely intended FQDN"

step "4.2b [MUTATES: attempted] the same rule on a multi-label target"
cap dns record create "$DOMAIN" cdn CNAME lb.example.net
assert_rc 2 "a multi-label relative target also exits 2"
assert_match 'not a fully qualified domain name' "the error names the actual problem"
# A value that already spells out a name only needs terminating. Suggesting
# "lb.example.net.<domain>." would walk the user straight into the doubling trap
# that step 4.1 exists to catch, so the suggestion must be the bare termination.
assert_match '"lb\.example\.net\."' "the fix suggests terminating the name the user wrote"
assert_no_match "lb\.example\.net\.${DOMAIN}" "the fix does not propose the doubled name"

step "4.3 [MUTATES] a multi-value CNAME is rejected as a set, not lost silently"
# internal/pdns keeps the first non-empty CNAME entry and drops the rest.
cap dns record create "$DOMAIN" alias1 CNAME "a.example.net."
assert_rc 0 "the first CNAME is created"
cap dns record create "$DOMAIN" alias1 CNAME "b.example.net."
assert_rc 2 "a second CNAME at the same name exits 2"
assert_match 'single-valued|exactly one CNAME' "the error explains the constraint"
cap dns record list "$DOMAIN" --name alias1 --type CNAME
assert_count '^alias1[[:space:]]+CNAME' 1 "the original value survived the rejected write"

step "4.3b [MUTATES: attempted] two CNAME values in one command are rejected too"
cap dns record set "$DOMAIN" alias1 CNAME "a.example.net." "b.example.net."
assert_rc 2 "a two-value CNAME set exits 2"
assert_match 'single-valued|exactly one CNAME' "the whole-set rule fires, not just the per-entry one"

step "4.4 [MUTATES: attempted] a value that does not match its type is refused client-side"
cap dns record create "$DOMAIN" www A "lb.example.net."
assert_rc 2 "a hostname given to an A record exits 2"
assert_match 'not a valid IPv4 address' "the error names the actual problem"
cap dns record list "$DOMAIN" --name www --type A
assert_count '^www[[:space:]]+A[[:space:]]' 1 "the existing A record was not disturbed"

step "4.4b [PROBE] confirm the server still does not validate the type/field pairing"
# This is the hazard the whole validation layer exists for: a DNSRecordSet whose
# typed field does not match spec.recordType is admitted, and buildRRSets then
# emits a zero-record rrset for that owner, which becomes a DELETE of whatever
# was there. Sent as a SERVER-SIDE DRY RUN, so admission runs and nothing is
# persisted and no reconcile is triggered.
cat >"${WORK_DIR}/mismatch.yaml" <<EOF
apiVersion: dns.networking.miloapis.com/v1alpha1
kind: DNSRecordSet
metadata:
  name: smoke-mismatch-probe
spec:
  dnsZoneRef:
    name: ${DOMAIN}
  recordType: A
  records:
    - name: mismatch-probe
      cname:
        content: lb.example.net.
EOF
cap datumctl apply -f "${WORK_DIR}/mismatch.yaml" --dry-run=server
if [[ "$RC" == "0" ]]; then
	probe "the server ACCEPTED a recordType:A entry carrying cname data — the client-side rule is load-bearing, as documented"
else
	probe "the server REJECTED the mismatched entry (exit $RC) — a webhook or CEL rule may have landed; tell the lead, the doc needs updating"
fi

step "4.5 [MUTATES] a TXT value over 255 bytes survives the round trip"
# The DKIM case. quoteIfNeeded wraps content in ONE character-string unless it
# is already quoted, so a long value must be stored pre-chunked by
# rdata.TXTContentForAPI or PowerDNS gets an over-long string.
LONG_TXT="v=DKIM1; k=rsa; p=$(python3 -c 'print("A"*400)')"
cap dns record create "$DOMAIN" dkim._domainkey TXT "$LONG_TXT"
assert_rc 0 "a 400+ byte TXT value is accepted"

cap dns record list "$DOMAIN" --name dkim._domainkey --type TXT -o json
assert_rc 0 "reading it back as JSON exits 0"
# Walked recursively rather than by a fixed path, so the extraction survives
# whichever wrapper -o json puts around the objects (a List, a bare object, or
# a stream).
STORED="$(jq -r '.. | objects | select(.name? == "dkim._domainkey") | .txt?.content? // empty' \
	<<<"$OUT" 2>/dev/null | head -1)"
if [[ -z "$STORED" ]]; then
	fail "could not find txt.content in the JSON output"
else
	# The stored form is one or more quoted character-strings; unchunking it
	# must reproduce the value byte for byte.
	UNCHUNKED="$(python3 "${WORK_DIR}/unchunk.py" "$STORED")"
	assert_eq "$UNCHUNKED" "$LONG_TXT" "the stored TXT unchunks to exactly what was written"
	if [[ "${#STORED}" -gt 0 && "$STORED" == *'" "'* ]]; then
		pass "the value was stored as multiple character-strings, so no string exceeds 255 bytes"
	else
		fail "the value was stored as one string — TXTContentForAPI was bypassed, PowerDNS will reject or truncate it"
	fi
fi

cap dns record describe "$DOMAIN" dkim._domainkey TXT
assert_rc 0 "describe exits 0"
assert_match 'v=DKIM1' "describe shows the logical value, not the chunked encoding"

step "4.6 [MUTATES] a TXT value with a semicolon survives the round trip"
cap dns record create "$DOMAIN" _dmarc TXT "v=DMARC1; p=none"
assert_rc 0 "a semicolon-bearing TXT value is accepted"
cap dns record describe "$DOMAIN" _dmarc TXT
assert_match 'v=DMARC1; p=none' "the semicolon comes back unescaped in the human view"

step "4.7 [MUTATES] deleting the last value of a type removes the whole record set"
# A DNSRecordSet holds every record of one type for the whole zone, so the
# object only goes when the last record of that type does. AAAA is used here
# precisely because nothing else in this script creates one — deleting a lone
# TXT would leave the DKIM and DMARC records behind and the set would correctly
# survive, which would prove nothing.
cap dns record list "$DOMAIN" --type AAAA
assert_match 'No records in zone .* match the given filters' "the zone starts with no AAAA records"
cap dns record create "$DOMAIN" solo AAAA 2001:db8::1
assert_rc 0 "the lone AAAA record is created"
cap dns record delete "$DOMAIN" solo AAAA 2001:db8::1 --yes
assert_rc 0 "deleting the only value exits 0"
assert_match "record/${DOMAIN} AAAA solo deleted" "confirms the deletion"
assert_match 'record set .* removed — no AAAA records remain in the zone' \
	"the empty DNSRecordSet object is removed, not left holding zero entries"
cap dns record list "$DOMAIN" --type AAAA
assert_no_match '^solo[[:space:]]' "the record is gone from the flattened view"

step "4.8 [MUTATES] deleting a value non-interactively proceeds without a prompt"
# ConfirmYesNo takes the non-interactive path for a recoverable action; only
# ConfirmTyped (zone delete) refuses. This asserts that distinction holds.
cap dns record create "$DOMAIN" tmpdel A 203.0.113.50
assert_rc 0 "a record to delete is created"
cap dns record delete "$DOMAIN" tmpdel A 203.0.113.50
assert_rc 0 "record delete with stdin not a terminal proceeds without --yes"
cap dns record list "$DOMAIN" --type A
assert_no_match '^tmpdel[[:space:]]' "it is gone"

# ===========================================================================
group "5. Bulk paths  [MUTATES]"
#
# Every step is still guarded by require_sub, but these commands now exist, so
# the guards no longer fire and this group executes for real. It is the
# least-exercised code in the plugin, and 5.4 covers a known hazard rather than
# a hypothetical one.
# ===========================================================================

if [[ $SKIP_BULK -eq 1 ]]; then
	skip "group 5 skipped by --skip-bulk"
else
	step "5.1 [READ] zone export emits a BIND file"
	if require_sub zone export; then
		cap dns zone export "$DOMAIN"
		assert_rc 0 "zone export exits 0"
		assert_match '^\$ORIGIN' "the file has an \$ORIGIN directive"
		assert_match '^\$TTL|[[:space:]]IN[[:space:]]' "the file is in zone-file format"
		assert_match 'v=DKIM1' "the long TXT record is exported"
		printf '%s\n' "$OUT" >"${WORK_DIR}/exported.zone"
		# RFC 1035 chunking must survive the export, or the re-import breaks.
		if grep -Eq '" +"' "${WORK_DIR}/exported.zone"; then
			pass "the long TXT value is exported as multiple character-strings"
		else
			fail "the long TXT value was exported as one string over 255 bytes"
		fi
	fi

	step "5.2 [MUTATES] record apply against the exported file is a no-op"
	if [[ ! -s "${WORK_DIR}/exported.zone" ]]; then
		skip "no exported file to apply (5.1 did not run)"
	elif require_sub record apply; then
		cap dns record apply "$DOMAIN" -f "${WORK_DIR}/exported.zone" --dry-run
		assert_rc 0 "applying the zone's own export exits 0"
		assert_match 'no changes|up to date|0 to add' \
			"export then apply is a closed loop with an empty diff"
	fi

	step "5.3 [MUTATES] zone import reports rather than swallows"
	if require_sub zone import; then
		cat >"${WORK_DIR}/import.zone" <<EOF
\$ORIGIN ${DOMAIN}.
\$TTL 300
imported     IN  A      203.0.113.60
imported2    IN  A      203.0.113.61
@            IN  CNAME  apex-cname-should-be-rewritten.example.net.
unsupported  IN  DNSKEY 256 3 8 AwEAAa==
EOF
		cap dns zone import "$DOMAIN" --file "${WORK_DIR}/import.zone" --dry-run
		assert_rc 0 "zone import --dry-run exits 0"
		assert_match 'ALIAS' "an apex CNAME is rewritten to ALIAS"
		assert_match 'DNSKEY' "an unsupported type is named, not silently dropped"
		assert_match 'imported' "the supported records are listed"
	fi

	step "5.4 [MUTATES] importing a zone file must not overwrite the platform's SOA and NS"
	# A provider export always carries the old provider's apex NS and SOA. If
	# import writes them, the zone stops resolving through Datum's nameservers
	# and delegation is destroyed — the worst outcome any command in this plugin
	# can produce, and one the user asked for the opposite of. The operator
	# recreates its own SOA/NS, so the damage is self-healing in principle, but
	# the window is real and the failure is silent.
	#
	# Safe to run for real here: this zone is created and deleted by this
	# script, and no registrar has ever been pointed at it. Doing it only as a
	# dry run would not exercise the write path, which is where the guard has to
	# hold.
	if require_sub zone import; then
		cap dns record list "$DOMAIN" --type NS
		assert_rc 0 "reading the apex NS records exits 0"
		NS_BEFORE="$(grep -E '^@[[:space:]]+NS[[:space:]]' <<<"$OUT" | sort)"
		cap dns record list "$DOMAIN" --type SOA
		assert_rc 0 "reading the SOA record exits 0"
		SOA_BEFORE="$(grep -E '^@[[:space:]]+SOA[[:space:]]' <<<"$OUT" | sort)"

		if [[ -z "$NS_BEFORE" || -z "$SOA_BEFORE" ]]; then
			skip "the platform SOA/NS records are not present yet, so there is nothing to protect"
		else
			cat >"${WORK_DIR}/hostile.zone" <<EOF
\$ORIGIN ${DOMAIN}.
\$TTL 300
@   IN  SOA  ns1.old-provider.example. hostmaster.old-provider.example. 42 7200 3600 1209600 3600
@   IN  NS   ns1.old-provider.example.
@   IN  NS   ns2.old-provider.example.
safe IN  A    203.0.113.90
EOF
			cap dns zone import "$DOMAIN" --file "${WORK_DIR}/hostile.zone"
			# Either outcome is acceptable for the command itself — refusing the
			# platform records outright, or importing the rest and reporting
			# that it skipped them. What is not acceptable is the records
			# changing.
			assert_match 'SOA|NS|skip|platform|managed' \
				"the import says something about the platform-managed records"

			cap dns record list "$DOMAIN" --type NS
			NS_AFTER="$(grep -E '^@[[:space:]]+NS[[:space:]]' <<<"$OUT" | sort)"
			cap dns record list "$DOMAIN" --type SOA
			SOA_AFTER="$(grep -E '^@[[:space:]]+SOA[[:space:]]' <<<"$OUT" | sort)"

			BOTH="apex NS before:
${NS_BEFORE}
apex NS after:
${NS_AFTER}"
			if [[ "$NS_AFTER" == "$NS_BEFORE" ]]; then
				pass "the apex NS records are untouched — delegation survives the import"
			else
				fail "the import REWROTE the apex NS records; delegation is destroyed"
			fi

			BOTH="SOA before:
${SOA_BEFORE}
SOA after:
${SOA_AFTER}"
			if [[ "$SOA_AFTER" == "$SOA_BEFORE" ]]; then
				pass "the SOA record is untouched"
			else
				fail "the import REWROTE the SOA record"
			fi

			cap dns record list "$DOMAIN" --name safe --type A
			assert_match '^safe[[:space:]]+A[[:space:]]' \
				"the ordinary records in the same file were still imported"
			if ! grep -Eq 'old-provider' <<<"$(dns record list "$DOMAIN" 2>&1)"; then
				pass "no record anywhere in the zone points at the old provider"
			else
				fail "a record referencing the old provider survived the import"
			fi
		fi
	fi
fi

# ===========================================================================
group "6. Teardown and the cascade gate  [MUTATES]"
# ===========================================================================

step "6.1 [MUTATES: attempted] zone delete refuses non-interactively without --yes"
# util.NonInteractive treats a pipe as unanswerable, and ConfirmTyped refuses
# rather than proceeding. This is the high-blast-radius gate.
cap_stdin "$DOMAIN" dns zone delete "$DOMAIN"
assert_rc 9 "the refusal exits 9"
assert_match 'DNS_ABORTED' "exit line carries the symbolic name"
assert_match 'refusing to perform a destructive action non-interactively' "says why it refused"
assert_match '[-]-yes' "the fix names the flag that would allow it"
cap dns zone list -o name
assert_match "^${DOMAIN}\$" "the zone is still there"

step "6.2 [MUTATES: attempted] the typed confirmation, under a pty"
# ConfirmTyped only prompts when stdin is a terminal, so this needs a pty. The
# `script` shim differs between BSD and GNU; if neither shape works the step is
# skipped rather than guessed at.
PTY_OUT="${WORK_DIR}/pty.out"
PTY_RAN=0
if command -v script >/dev/null 2>&1; then
	if [[ "$(uname -s)" == "Darwin" ]]; then
		# BSD: script [-q] file command ...
		(echo "definitely-not-the-domain" | script -q "$PTY_OUT" \
			env DATUMCTL_PLUGINS_DIR="$PLUGIN_DIR" datumctl dns zone delete "$DOMAIN") >/dev/null 2>&1
		RC=$?
		PTY_RAN=1
	else
		# GNU: script -q -c "command" file
		(echo "definitely-not-the-domain" | script -q -c \
			"DATUMCTL_PLUGINS_DIR=$PLUGIN_DIR datumctl dns zone delete $DOMAIN" "$PTY_OUT") >/dev/null 2>&1
		RC=$?
		PTY_RAN=1
	fi
fi
if [[ $PTY_RAN -eq 1 && -s "$PTY_OUT" ]]; then
	BOTH="$(cat "$PTY_OUT")"
	assert_match 'will also delete all .* DNS records it contains' "the cascade is stated before the prompt"
	assert_match 'This cannot be undone' "the warning says it is irreversible"
	assert_match 'Type .* to confirm' "the typed confirmation is requested"
	assert_match 'confirmation did not match' "typing the wrong text aborts"
	cap dns zone list -o name
	assert_match "^${DOMAIN}\$" "the zone survived the declined confirmation"
else
	skip "no usable pty (script(1)); the typed-confirmation prompt could not be exercised"
	probe "the interactive cascade prompt is UNVERIFIED by this run — exercise it by hand once"
fi

step "6.3 [MUTATES] --yes deletes the zone and states the cascade in the result"
cap dns zone delete "$DOMAIN" --yes
assert_rc 0 "zone delete --yes exits 0"
assert_match "zone/${DOMAIN} deleted" "confirms the deletion"
assert_match 'DNS records were deleted with it|deleted$' "the result states what went with it"
if [[ "$RC" == "0" ]]; then
	ZONE_CREATED=0
fi

step "6.4 [READ] the zone is really gone"
cap dns zone list -o name
assert_no_match "^${DOMAIN}\$" "it is out of the list"
cap dns zone describe "$DOMAIN"
assert_rc 4 "describing it now exits 4"

exit $((FAIL > 0 ? 1 : 0))
