#!/usr/bin/env bash
# ============================================================================
# Offline harness for scripts/container.sh's container-resolver selection
# (compute_dns_servers + resolvers_answer + dns_probe_available + dns_args_match).
#
# WHY THIS EXISTS: compute_dns_servers decides which DNS servers `up` pins into
# the Orchicon container at CREATE time. It exists at all because Docker copies
# the host's /etc/resolv.conf only when the container is created — so a
# container born on a captive portal keeps that portal's dead gateway as its
# resolver forever: raw-IP egress is healthy, every provider hostname fails.
#
# The selection has four branches and ONE of them is a trap. A host with no
# dig and no nslookup cannot be probed, and reading "cannot probe" as "resolver
# is dead" silently repoints EVERY such host at public DNS — breaking the
# split-horizon internal names that resolve there today. bind-utils is not part
# of a default Arch/CachyOS install, so that is not a hypothetical. That defect
# is the one this harness pins, and the baseline block at the end proves the
# harness actually detects it rather than passing vacuously.
#
# WHAT IS REAL HERE: container.sh's own functions, EXTRACTED BY TEXT AND NEVER
# EXECUTED — the script drives Docker, so running it is exactly what a test must
# not do. Extraction is asserted, so renaming a function upstream fails here
# loudly instead of silently testing nothing.
#
# WHAT IS EMULATED: /etc/resolv.conf (a fixture, via the ORCHICON_RESOLV_CONF
# seam container.sh exposes for this harness) and dig (a shell function whose
# ANSWER is scripted; its wire protocol is irrelevant to resolver SELECTION).
# PATH is narrowed to a stub dir holding only awk/grep/tr, which is ALSO what
# makes "no probe tool installed" a real, deterministic state instead of a
# property of whoever's machine happens to run this.
#
#   scripts/tests/container-dns/run.sh [container.sh]
#
# Exits 0 only when every assertion passes, so it can be used as a check.
# ============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
CONTAINER_SH="${REPO_ROOT}/scripts/container.sh"
[ "$#" -gt 0 ] && CONTAINER_SH="$1"

# The harness's own environment must not decide any case.
unset ORCHICON_CONTAINER_DNS 2>/dev/null || true

PUBLIC="1.1.1.1 8.8.8.8"
PASSED=0
FAILED=0

check() { # <label> <expected> <actual>
  if [ "$2" = "$3" ]; then
    printf '  \033[32mPASS\033[0m  %-48s %s\n' "$1" "$3"
    PASSED=$((PASSED + 1))
  else
    printf '  \033[31mFAIL\033[0m  %-48s want [%s] got [%s]\n' "$1" "$2" "$3"
    FAILED=$((FAILED + 1))
  fi
}

# --- the functions under test, extracted rather than sourced ----------------
extract_fn() {
  awk -v fn="$1" '
    $0 == fn "() {" { inf = 1 }
    inf { print }
    inf && $0 == "}" { exit }
  ' "$CONTAINER_SH"
}

echo "container.sh under test: ${CONTAINER_SH}"
echo

for fn in dns_probe_available resolvers_answer compute_dns_servers dns_args_match; do
  body="$(extract_fn "$fn")"
  if [ -z "$body" ]; then
    echo "run.sh: could not extract ${fn}() from ${CONTAINER_SH}" >&2
    echo "        renamed? this harness must never pass vacuously." >&2
    exit 2
  fi
  eval "$body"
done

# The resolv.conf seam: without it compute_dns_servers reads the REAL
# /etc/resolv.conf and every case below becomes a property of this machine.
if ! grep -q '^RESOLV_CONF=' "$CONTAINER_SH"; then
  echo "run.sh: ${CONTAINER_SH} has no RESOLV_CONF seam — cannot drive resolv.conf" >&2
  exit 2
fi
eval "$(grep -m1 '^RESOLV_CONF=' "$CONTAINER_SH")"

# --- deterministic tool environment -----------------------------------------
STUB="$(mktemp -d)"
trap 'rm -rf "${STUB}" 2>/dev/null || true' EXIT   # never clobber the exit code
mkdir -p "${STUB}/bin"
# Only these three reach the function under test, because PATH is narrowed to
# this dir inside every case — so a real dig/nslookup on the host cannot leak in
# and change a result.
for tool in awk grep tr sort; do
  real="$(command -v "$tool")"
  { printf '#!/bin/sh\n'; printf 'exec %s "$@"\n' "$real"; } > "${STUB}/${tool}.src"
  install -m 0755 "${STUB}/${tool}.src" "${STUB}/bin/${tool}"
done

norm() { printf '%s' "$1" | tr -s ' ' | sed -e 's/^ //' -e 's/ $//'; }

# compute_with <function> <resolv.conf content> <probe mode> [ORCHICON_CONTAINER_DNS]
#   probe mode: answers (dig prints an answer) | silent (dig prints nothing) |
#               none (no dig/nslookup reachable at all)
compute_with() {
  local fn="$1" resolv="$2" mode="$3" override="${4-}"
  printf '%s\n' "$resolv" > "${STUB}/resolv.conf"
  (
    PATH="${STUB}/bin"
    RESOLV_CONF="${STUB}/resolv.conf"
    [ -n "$override" ] && ORCHICON_CONTAINER_DNS="$override"
    case "$mode" in
      answers) dig() { printf '%s\n' 93.184.216.34; } ;;
      silent)  dig() { return 0; } ;;
      none)    : ;;
    esac
    "$fn"
  )
}

# --- cases ------------------------------------------------------------------
CORP="nameserver 10.0.0.53"
STUB_ONLY="nameserver 127.0.0.53"

check "explicit ORCHICON_CONTAINER_DNS wins verbatim" \
  "9.9.9.9 149.112.112.112" \
  "$(norm "$(compute_with compute_dns_servers "$CORP" answers '9.9.9.9 149.112.112.112')")"

check "routable host resolver that answers is kept" \
  "10.0.0.53" \
  "$(norm "$(compute_with compute_dns_servers "$CORP" answers)")"

check "routable host resolver that is DEAD -> public" \
  "$PUBLIC" \
  "$(norm "$(compute_with compute_dns_servers "$CORP" silent)")"

check "systemd-resolved stub 127.0.0.53 alone -> public" \
  "$PUBLIC" \
  "$(norm "$(compute_with compute_dns_servers "$STUB_ONLY" answers)")"

check "NO probe tool keeps the host resolver (regression)" \
  "10.0.0.53" \
  "$(norm "$(compute_with compute_dns_servers "$CORP" none)")"

check "no probe tool + stub-only still -> public" \
  "$PUBLIC" \
  "$(norm "$(compute_with compute_dns_servers "$STUB_ONLY" none)")"

check "loopback/link-local/IPv6 dropped, routable survives" \
  "10.0.0.53" \
  "$(norm "$(compute_with compute_dns_servers "$(printf 'nameserver 127.0.0.53\nnameserver fe80::1\nnameserver ::1\nnameserver 10.0.0.53')" answers)")"

check "no nameservers at all -> public" \
  "$PUBLIC" \
  "$(norm "$(compute_with compute_dns_servers '# no nameservers here' answers)")"

# The one case the //-substitution in the emptiness test exists for: with no
# usable nameserver AND no probe tool, a whitespace-only routable must still read
# as "nothing to keep" — otherwise it is "kept" as an empty --dns list, which is
# neither the host's resolvers nor the public fallback.
check "no nameservers + no probe tool -> public" \
  "$PUBLIC" \
  "$(norm "$(compute_with compute_dns_servers '# no nameservers here' none)")"

# --- dns_args_match: the recreate decision ----------------------------------
# This is the function that decides whether `up` RECREATES a container — which
# kills in-flight runs — so a wrong result is not cosmetic. It reads
# HostConfig.Dns through `docker inspect`, stubbed here to a scripted value.
dns_match_with() { # <container HostConfig.Dns> <want>
  (
    PATH="${STUB}/bin"
    FAKE_DNS="$1"
    docker() { printf '%s\n' "${FAKE_DNS}"; }
    if dns_args_match "stub-container" "$2"; then printf 'match'; else printf 'mismatch'; fi
  )
}

check "dns_args_match: identical set -> no recreate" \
  "match" "$(dns_match_with '1.1.1.1 8.8.8.8 ' '1.1.1.1 8.8.8.8')"

check "dns_args_match: order-insensitive (sort -u)" \
  "match" "$(dns_match_with '8.8.8.8 1.1.1.1 ' '1.1.1.1 8.8.8.8')"

check "dns_args_match: different set -> recreate" \
  "mismatch" "$(dns_match_with '1.1.1.1 8.8.8.8 ' '10.0.0.53')"

check "dns_args_match: empty HostConfig.Dns (pre-fix) -> recreate" \
  "mismatch" "$(dns_match_with '' '1.1.1.1 8.8.8.8')"

# --- wiring: the call site must pass the desired set ------------------------
# Static by necessity (up_instance drives Docker), and it guards a REAL crash:
# dns_args_match reads its second parameter, so up_instance calling it with only
# $NAME kills the script outright under `set -u` — "unbound variable" — on the
# second `up` of an existing container, which is exactly the DNS-mismatch path
# this whole change exists to serve.
if grep -qF 'dns_args_match "$NAME" "$dns_servers"' "$CONTAINER_SH"; then
  printf '  \033[32mPASS\033[0m  %-48s %s\n' \
    "call site passes the desired set" 'dns_args_match "$NAME" "$dns_servers"'
  PASSED=$((PASSED + 1))
else
  printf '  \033[31mFAIL\033[0m  %-48s %s\n' \
    "call site passes the desired set" "no 2-arg call found — set -u will kill up"
  FAILED=$((FAILED + 1))
fi

# --- baseline: the PRE-FIX logic must FAIL the regression case -------------- #
# Reproduced verbatim from the pre-fix compute_dns_servers. Its only job is to
# prove the harness detects the defect it exists for: if the baseline ever
# PASSES the regression case, this harness has stopped testing anything.
baseline_compute_dns_servers() {
  if [ -n "${ORCHICON_CONTAINER_DNS:-}" ]; then
    printf '%s' "$ORCHICON_CONTAINER_DNS"
    return 0
  fi
  local host_dns routable
  host_dns=$(awk '/^nameserver[[:space:]]/{printf "%s ", $2}' "$RESOLV_CONF" 2>/dev/null)
  routable=$(printf '%s\n' $host_dns | grep -vE '^(127\.|::1$|fe80:|0\.0\.0\.0$)' | tr '\n' ' ')
  if [ -n "$routable" ] && resolvers_answer example.com $routable; then
    printf '%s' "$routable"
    return 0
  fi
  printf '1.1.1.1 8.8.8.8'
}

base_out="$(norm "$(compute_with baseline_compute_dns_servers "$CORP" none)")"
if [ "$base_out" = "$PUBLIC" ]; then
  printf '  \033[32mPASS\033[0m  %-48s (defect reproduced: %s)\n' \
    "baseline (pre-fix) repoints a no-tool host" "$base_out"
  PASSED=$((PASSED + 1))
else
  printf '  \033[31mFAIL\033[0m  %-48s baseline returned [%s] — detects nothing\n' \
    "baseline (pre-fix) repoints a no-tool host" "$base_out"
  FAILED=$((FAILED + 1))
fi

echo
if [ "$FAILED" -eq 0 ]; then
  echo "RESULT: all ${PASSED} assertions passed"
  exit 0
fi
echo "RESULT: FAILED — ${FAILED} of $((PASSED + FAILED)) assertions failed" >&2
exit 1
