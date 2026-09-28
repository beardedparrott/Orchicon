#!/usr/bin/env bash
# ============================================================================
# Offline harness for scripts/container.sh's HOST-RESIDENT plane bind and
# per-instance plane URL (bridge_ip / bridge_bind_env / PLANE_HTTP_PORT).
#
# WHY THIS EXISTS: a host-resident plane must listen on the loopback address
# (host clients: orch, the GUI) AND on the docker bridge at THIS instance's
# port, and it must advertise that same address to its run containers. The
# defect this pins is a SHARED PORT: with a hardcoded 8080 both instances
# advertise the same URL, so prod's workers dial dev's plane. That dial is
# rejected (the minted credential is instance-bound — its hash lives in the
# minting instance's database) rather than silently writing to the other
# instance, so the failure is a broken run, not a wrong-instance write; the
# instance-binding is the ONLY thing standing between a misconfigured URL and
# a cross-instance write, which is why it gets its own test
# (internal/server/plane_crossinstance_gated_test.go).
#
# WHAT IS REAL HERE: container.sh's own functions, EXTRACTED BY TEXT (never
# executed — the script drives Docker, so running it is what a test must not
# do). Extraction is asserted, so renaming a function upstream fails loudly
# instead of silently testing nothing.
#
# WHAT IS EMULATED: docker / the docker0 interface (PATH is narrowed to a stub
# dir, so "docker unavailable" is a real, deterministic state rather than a
# property of whoever's machine runs this) and the bridge address itself
# (ORCHICON_DOCKER_BRIDGE_IP, the seam container.sh exposes for exactly this).
#
#   scripts/tests/plane-reachability/run.sh [container.sh]
#
# Set ORCHICON_TEST_DOCKER=1 to also run the REAL end-to-end reachability
# checks (needs a live host plane + the runtime image); they are reported as a
# loud SKIP otherwise, never silently.
#
# Exits 0 only when every assertion passes, so it can be used as a check.
# ============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
CONTAINER_SH="${REPO_ROOT}/scripts/container.sh"
[ "$#" -gt 0 ] && CONTAINER_SH="$1"

unset ORCHICON_DOCKER_BRIDGE_IP 2>/dev/null || true

PASSED=0
FAILED=0

check() { # <label> <expected> <actual>
  if [ "$2" = "$3" ]; then
    printf '  \033[32mPASS\033[0m  %-52s %s\n' "$1" "$3"
    PASSED=$((PASSED + 1))
  else
    printf '  \033[31mFAIL\033[0m  %-52s want [%s] got [%s]\n' "$1" "$2" "$3"
    FAILED=$((FAILED + 1))
  fi
}

check_absent() { # <label> <needle> <haystack>
  case "$3" in
  *"$2"*)
    printf '  \033[31mFAIL\033[0m  %-52s found %s\n' "$1" "$2"
    FAILED=$((FAILED + 1))
    ;;
  *)
    printf '  \033[32mPASS\033[0m  %-52s (absent)\n' "$1"
    PASSED=$((PASSED + 1))
    ;;
  esac
}

must_fail() { # <label> <expected> <actual> — PASSes only when check() would FAIL
  if [ "$2" = "$3" ]; then
    printf '  \033[31mFAIL\033[0m  %-52s baseline produced the wanted value — detects nothing\n' "$1"
    FAILED=$((FAILED + 1))
  else
    printf '  \033[32mPASS\033[0m  %-52s (defect reproduced: %s)\n' "$1" "$3"
    PASSED=$((PASSED + 1))
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

# log_err is a one-liner in container.sh (not extractable by the multi-line
# pattern below) and is not what this harness asserts: supply it.
log_err() { printf '%s\n' "$*" >&2; }

for fn in instance_info bridge_ip bridge_bind_env; do
  body="$(extract_fn "$fn")"
  if [ -z "$body" ]; then
    echo "run.sh: could not extract ${fn}() from ${CONTAINER_SH}" >&2
    echo "        renamed? this harness must never pass vacuously." >&2
    exit 2
  fi
  eval "$body"
done

# --- deterministic tool environment -----------------------------------------
STUB="$(mktemp -d)"
trap 'rm -rf "${STUB}" 2>/dev/null || true' EXIT   # never clobber the exit code
mkdir -p "${STUB}/bin"
# Only awk and cut can be reached from inside the functions (the docker0
# branch), so a real docker/ip on the host cannot leak in and decide a case.
for tool in awk cut; do
  real="$(command -v "$tool")"
  { printf '#!/bin/sh\n'; printf 'exec %s "$@"\n' "$real"; } > "${STUB}/${tool}.src"
  install -m 0755 "${STUB}/${tool}.src" "${STUB}/bin/${tool}"
done

norm() { printf '%s' "$1" | sed -e 's/[[:space:]]*$//'; }

# bind_with <inst> <docker mode> [ORCHICON_DOCKER_BRIDGE_IP]
#   docker mode: gateway (inspect returns a gateway) | novalue (returns
#                "<no value>", forcing the docker0 fallback) | none (no docker)
bind_with() {
  local inst="$1" mode="$2" override="${3-}"
  (
    PATH="${STUB}/bin"
    [ -n "$override" ] && ORCHICON_DOCKER_BRIDGE_IP="$override"
    case "$mode" in
    gateway) docker() { printf '%s\n' "172.17.0.1"; } ;;
    novalue) docker() { printf '%s\n' "<no value>"; } ;;
    none) : ;;
    esac
    if [ "$mode" = "novalue" ]; then
      # docker0 fallback: the stub can only be seen through a function.
      ip() { printf '%s\n' "4: docker0    inet 10.5.0.1/16 scope global docker0"; }
    fi
    bridge_bind_env "$inst"
  )
}

# --- per-instance bind + URL ------------------------------------------------ #
DEV="$(bind_with dev gateway '172.17.0.1')"
PROD="$(bind_with prod gateway '172.17.0.1')"

check "dev bind is the bridge ip at dev's own port" \
  "ORCHICON_HTTP_EXTRA_BIND=172.17.0.1:8080" \
  "$(printf '%s\n' "$DEV" | sed -n 1p)"
check "dev advertises the same address it binds" \
  "ORCHICON_PLANE_PUBLIC_URL=http://172.17.0.1:8080" \
  "$(printf '%s\n' "$DEV" | sed -n 2p)"
check "bridge_bind_env prints exactly two lines" "2" "$(printf '%s\n' "$DEV" | wc -l | tr -d ' ')"

check "prod bind is the bridge ip at prod's own port" \
  "ORCHICON_HTTP_EXTRA_BIND=172.17.0.1:8091" \
  "$(printf '%s\n' "$PROD" | sed -n 1p)"
check "prod advertises the same address it binds" \
  "ORCHICON_PLANE_PUBLIC_URL=http://172.17.0.1:8091" \
  "$(printf '%s\n' "$PROD" | sed -n 2p)"
check_absent "prod's values contain no 8080 (dev's port)" "8080" "$PROD"

# The crossover assertion: no line of dev's output may appear in prod's.
shared=""
for line in $DEV; do
  case "$PROD" in *"$line"*) shared="$line" ;; esac
done
check "dev and prod share no bind/URL value" "" "$shared"

# --- bridge discovery ------------------------------------------------------- #
check "docker's IPAM gateway is used when available" \
  "ORCHICON_HTTP_EXTRA_BIND=172.17.0.1:8080" \
  "$(bind_with dev gateway | sed -n 1p)"
check "a <no value> gateway falls back to the docker0 address" \
  "ORCHICON_HTTP_EXTRA_BIND=10.5.0.1:8080" \
  "$(bind_with dev novalue | sed -n 1p)"
check "ORCHICON_DOCKER_BRIDGE_IP overrides discovery" \
  "ORCHICON_HTTP_EXTRA_BIND=192.168.65.1:8080" \
  "$(bind_with dev gateway '192.168.65.1' | sed -n 1p)"

# No docker, no docker0, no override: a HARD failure, and never a wildcard.
NOBRIDGE="$(bind_with dev none 2>/dev/null)"
NOBRIDGE_RC=$?
check "no resolvable bridge exits non-zero" "1" "$NOBRIDGE_RC"
check "no resolvable bridge emits nothing" "" "$NOBRIDGE"
check_absent "no 0.0.0.0 fallback anywhere in the bind output" "0.0.0.0" "$NOBRIDGE"

# --- no shared-profile export ----------------------------------------------- #
PLANE_URL_REFS="$(grep -rn 'export ORCHICON_PLANE_PUBLIC_URL' --exclude-dir=plane-reachability \
  "${REPO_ROOT}/scripts" "${REPO_ROOT}/.env.example" "${REPO_ROOT}/Makefile" 2>/dev/null || true)"
check_absent "nothing export's a globally-shared ORCHICON_PLANE_PUBLIC_URL" \
  "export ORCHICON_PLANE_PUBLIC_URL" "${PLANE_URL_REFS}"
check "container.sh computes ORCHICON_PLANE_PUBLIC_URL in exactly one place" "1" \
  "$(grep -c "printf 'ORCHICON_PLANE_PUBLIC_URL=" "${CONTAINER_SH}" | tr -d ' ')"
check_absent ".env.example does not carry ORCHICON_PLANE_PUBLIC_URL" \
  "ORCHICON_PLANE_PUBLIC_URL" "$(cat "${REPO_ROOT}/.env.example" 2>/dev/null || true)"

# --- the code-side anchors these values depend on --------------------------- #
if grep -qF '"--add-host", "host.docker.internal:host-gateway"' "${REPO_ROOT}/internal/runtime/daemon.go"; then
  printf '  \033[32mPASS\033[0m  %-52s %s\n' "runtime containers get the host-gateway mapping" "daemon.go create args"
  PASSED=$((PASSED + 1))
else
  printf '  \033[31mFAIL\033[0m  %-52s %s\n' "runtime containers get the host-gateway mapping" "missing from daemon.go create args"
  FAILED=$((FAILED + 1))
fi
# The property is "no write to the error channel", so match the WRITE, not the
# word (listen.go's comments name errCh precisely to explain why it is never
# used — Run returns from the whole plane on any value it receives).
if [ -f "${REPO_ROOT}/internal/server/listen.go" ] && ! grep -q 'errCh *<-' "${REPO_ROOT}/internal/server/listen.go"; then
  printf '  \033[32mPASS\033[0m  %-52s %s\n' "the bridge listener can never kill the plane" "listen.go never writes errCh"
  PASSED=$((PASSED + 1))
else
  printf '  \033[31mFAIL\033[0m  %-52s %s\n' "the bridge listener can never kill the plane" "listen.go touches errCh (Run exits on it)"
  FAILED=$((FAILED + 1))
fi

# --- baseline: the PRE-FIX logic must FAIL the crossover case --------------- #
# Verbatim shape of the pre-fix behaviour: one shared 8080 for both
# instances. Its only job is to prove this harness detects the defect it
# exists for; if the baseline ever PASSES, the harness tests nothing.
baseline_bridge_bind_env() {
  local ip="172.17.0.1"
  printf 'ORCHICON_HTTP_EXTRA_BIND=%s:8080\n' "$ip"
  printf 'ORCHICON_PLANE_PUBLIC_URL=http://%s:8080\n' "$ip"
}
must_fail "baseline (pre-fix shared 8080) fails the prod case" \
  "no-8080" "$(baseline_bridge_bind_env | tail -1 | grep -o '8080' || true)"

# --- optional real end-to-end reachability (needs docker + a live plane) ---- #
if [ "${ORCHICON_TEST_DOCKER:-0}" = "1" ]; then
  RUNTIME_IMAGE="${ORCHICON_TEST_RUNTIME_IMAGE:-orchicon-runtime:local}"
  DEVPORT=8080
  PRODPORT=8091
  echo
  echo "  ORCHICON_TEST_DOCKER=1 — real reachability checks"
  if ! command -v docker >/dev/null 2>&1; then
    printf '  \033[31mFAIL\033[0m  %-52s %s\n' "docker is required for the E2E checks" "no docker on PATH"
    FAILED=$((FAILED + 1))
  else
    # 1. Host clients reach the plane on loopback.
    if curl -fsS -m 5 "http://127.0.0.1:${DEVPORT}/healthz" >/dev/null 2>&1; then
      printf '  \033[32mPASS\033[0m  %-52s %s\n' "host clients reach dev's plane on loopback" "127.0.0.1:${DEVPORT}"
      PASSED=$((PASSED + 1))
    else
      printf '  \033[31mFAIL\033[0m  %-52s %s\n' "host clients reach dev's plane on loopback" "no answer on 127.0.0.1:${DEVPORT} (not running?)"
      FAILED=$((FAILED + 1))
    fi
    # 2. A runtime container reaches the host plane by NAME across the bridge.
    for pair in "dev:${DEVPORT}:${PRODPORT}" "prod:${PRODPORT}:${DEVPORT}"; do
      inst="${pair%%:*}"; rest="${pair#*:}"; port="${rest%%:*}"; other="${rest#*:}"
      if docker run --rm --add-host=host.docker.internal:host-gateway "$RUNTIME_IMAGE" \
        sh -c "curl -fsS -m 5 http://host.docker.internal:${port}/healthz" >/dev/null 2>&1; then
        printf '  \033[32mPASS\033[0m  %-52s %s\n' "a runtime container reaches ${inst}'s plane by name" "host.docker.internal:${port}"
        PASSED=$((PASSED + 1))
      else
        printf '  \033[31mFAIL\033[0m  %-52s %s\n' "a runtime container reaches ${inst}'s plane by name" "host.docker.internal:${port} did not answer"
        FAILED=$((FAILED + 1))
      fi
      # 3. The WRONG instance's plane must not accept this instance's token.
      if [ -n "${ORCHICON_TEST_PLANE_TOKEN:-}" ]; then
        code="$(docker run --rm --add-host=host.docker.internal:host-gateway "$RUNTIME_IMAGE" \
          sh -c "curl -s -o /dev/null -w '%{http_code}' -m 5 \
            -H 'Authorization: Bearer ${ORCHICON_TEST_PLANE_TOKEN}' \
            -H 'Content-Type: application/json' \
            -d '{}' http://host.docker.internal:${other}/orchicon.api.v1.WorkItemService/ListWorkItems" 2>/dev/null)"
        case "$code" in
        401 | 403)
          printf '  \033[32mPASS\033[0m  %-52s %s\n' "${inst}'s token is rejected by the other plane" "HTTP ${code}"
          PASSED=$((PASSED + 1))
          ;;
        *)
          printf '  \033[31mFAIL\033[0m  %-52s %s\n' "${inst}'s token is rejected by the other plane" "got HTTP [${code}], want 401/403 — the credential's instance-binding did not hold"
          FAILED=$((FAILED + 1))
          ;;
        esac
      fi
    done
    if [ -z "${ORCHICON_TEST_PLANE_TOKEN:-}" ]; then
      echo "  SKIP  misaddressed-dial rejection needs ORCHICON_TEST_PLANE_TOKEN (minted for the OTHER plane) to assert 401"
    fi
  fi
else
  echo
  echo "  SKIP  real bridge-reachability + cross-instance 401 checks (set ORCHICON_TEST_DOCKER=1 with a live plane,"
  echo "        the runtime image and ORCHICON_TEST_PLANE_TOKEN; the dispatched-run proof is the QA step's E2E)"
fi

echo
if [ "$FAILED" -eq 0 ]; then
  echo "RESULT: all ${PASSED} assertions passed"
  exit 0
fi
echo "RESULT: FAILED — ${FAILED} of $((PASSED + FAILED)) assertions failed" >&2
exit 1
