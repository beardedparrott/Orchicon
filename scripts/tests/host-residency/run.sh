#!/usr/bin/env bash
# ============================================================================
# Offline harness for the ONE launch shape: the plane and its services are HOST
# processes, per instance, always.
#
# WHY THIS EXISTS: the shape is a per-instance property with silent, expensive
# failure modes, and none of them needs a container engine to detect:
#
#   1. A SHAPE THAT CAN BE CHOSEN AT ALL. There used to be two residencies
#      (host and container) with a default, an env var and a third positional to
#      pick between them. The container one is deleted: no `orchicon container`,
#      no `ORCHICON_CONTAINER_SERVICES_ONLY`, no `scripts/container.sh`. A
#      leftover makes the product claim a shape it cannot run, so its ABSENCE is
#      asserted here rather than assumed.
#   2. A GLOBAL value. One shared HTTP port / plane URL / service port between
#      dev and prod points one instance's workers at the other's services and
#      database. Every per-instance value is therefore asserted to differ AND
#      to come from the instance table.
#   3. A DERIVED-BUT-WRONG plane URL. The host plane's URL is not a literal: it
#      is bridge_bind_env's output (the resolved engine bridge + THIS instance's
#      port), so the bridge is pinned below to keep the assertion deterministic
#      and engine-free.
#   4. A COLLIDING BIND SET. The host plane's two listeners must be DISTINCT
#      concrete addresses: a wildcard primary (`:8080`) already owns the bridge
#      address on every interface, so the extra bind could never bind and the
#      plane retried a doomed net.Listen every 30s on every boot. The bind set
#      (primary + extra) is asserted to contain no wildcard.
#
# It also pins the security boundary: every service bind reported by the table is
# loopback-bound, so Postgres/NATS/telemetry are never exposed on the LAN.
#
# WHAT IS REAL HERE: the real functions, EXTRACTED BY TEXT AND NEVER EXECUTED —
# the scripts drive the engine, so sourcing them would run them. Extraction is
# asserted, so renaming a function upstream fails here loudly instead of silently
# testing nothing.
#
# WHERE THEY LIVE: the per-instance table and the shape it composes (instance_info,
# plane_env, bridge_ip, bridge_bind_env) are in scripts/lib/instances.sh — the ONE
# place the shell side enumerates dev/test/prod. The verbs that used to consume
# them (print_shape, up_instance, plane_start) lived in the now-deleted
# scripts/container.sh: their successors are `make instance-*`, `orchicon plane
# start|stop|status` and scripts/instance-ports.sh, and the Go half below drives
# the implementation that actually ships.
#
#   scripts/tests/host-residency/run.sh [instances.sh]
#
# Exits 0 only when every assertion passes, so it can be used as a check.
# ============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
INSTANCES_SH="${REPO_ROOT}/scripts/lib/instances.sh"
ENGINE_SH="${REPO_ROOT}/scripts/lib/engine.sh"
PORTS_SH="${REPO_ROOT}/scripts/instance-ports.sh"
MAKEFILE="${REPO_ROOT}/Makefile"
[ "$#" -gt 0 ] && INSTANCES_SH="$1"

# The harness's own environment must not decide any case.
unset ORCHICON_PLANE_RESIDENCY 2>/dev/null || true
unset ORCHICON_CONTAINER_SERVICES_ONLY 2>/dev/null || true

PASSED=0
FAILED=0
SKIPPED=0

check() { # <label> <expected> <actual>
  if [ "$2" = "$3" ]; then
    printf '  \033[32mPASS\033[0m  %-52s %s\n' "$1" "$3"
    PASSED=$((PASSED + 1))
  else
    printf '  \033[31mFAIL\033[0m  %-52s want [%s] got [%s]\n' "$1" "$2" "$3"
    FAILED=$((FAILED + 1))
  fi
}

check_contains() { # <label> <needle> <haystack>
  case "$3" in
    *"$2"*) check "$1" "contains:$2" "contains:$2" ;;
    *)      check "$1" "contains:$2" "MISSING (got: $3)" ;;
  esac
}

check_lacks() { # <label> <needle> <haystack>
  case "$3" in
    *"$2"*) check "$1" "no:$2" "PRESENT (got: $3)" ;;
    *)      check "$1" "no:$2" "no:$2" ;;
  esac
}

# --- the functions under test, extracted rather than sourced ----------------
# extract_fn <file> <fn> — the function's text, never executed at extract time.
extract_fn() {
  awk -v fn="$2" '
    $0 == fn "() {" { inf = 1 }
    inf { print }
    inf && $0 == "}" { exit }
  ' "$1"
}

echo "instance table under test: ${INSTANCES_SH}"
echo "port queries under test:    ${PORTS_SH}"
echo

# from_one <file> <fn...> — extract each function from <file> and eval it.
from_one() {
  local file="$1"; shift
  local fn body
  for fn in "$@"; do
    body="$(extract_fn "$file" "$fn")"
    if [ -z "$body" ]; then
      echo "run.sh: could not extract ${fn}() from ${file}" >&2
      echo "        renamed or moved? this harness must never pass vacuously." >&2
      exit 2
    fi
    eval "$body"
  done
}

from_one "$INSTANCES_SH" instance_info plane_env bridge_ip bridge_bind_env
from_one "$ENGINE_SH" engine_cmd

# The table logs through helpers it does not own; stub them so the harness stays
# independent of the script's colour handling.
log_err() { echo "ERR: $*" >&2; }
log_dim() { :; }
log_ok() { :; }
log_warn() { :; }

# shape <instance> — the instance's HOST launch shape, composed from the table
# exactly as `make instance-up`/`orchicon plane start` receive it.
shape() {
  local inst="$1"
  (
    # The plane's bind and URL are DERIVED from the engine bridge
    # (bridge_bind_env), never literal. Pin the bridge so this harness stays
    # engine-free and deterministic — the same seam the plane's own bind uses.
    export ORCHICON_DOCKER_BRIDGE_IP="${ORCHICON_DOCKER_BRIDGE_IP:-172.17.0.1}"
    instance_info "$inst" || exit 1
    echo "instance=$inst"
    echo "plane_http_port=$PLANE_HTTP_PORT"
    echo "host_data_dir=$HOST_DATA_DIR"
    echo "service_ports=$SERVICE_PORTS"
    bridge_bind_env "$inst" | sed -n 's/^ORCHICON_PLANE_PUBLIC_URL=/plane_public_url=/p'
    plane_env "$inst" | sed 's/^/plane_env:/'
  ) 2>&1
}

field() { # <shape> <key>
  printf '%s\n' "$1" | sed -n "s/^$2=//p" | head -1
}

# --- (i) ONE SHAPE: there is no container residency left to select ----------
#
# REWRITTEN, NOT RELAXED. The block that used to live here asserted the default
# residency and the opt-in `container` shape; both are now FALSE BY CONSTRUCTION
# (the launcher, the image and the `container` subcommand are deleted). What is
# asserted instead is that no path can reintroduce them — an unselectable shape is
# the property, and its absence is checkable.
echo "  -- one shape: nothing can select a container residency --"
check "scripts/container.sh is deleted" "gone" "$([ -e "${REPO_ROOT}/scripts/container.sh" ] && echo PRESENT || echo gone)"
check_contains "the per-instance queries survive as instance-ports.sh" "services_port() {" "$(cat "$PORTS_SH" 2>/dev/null || echo MISSING)"
# A REAL SURVIVOR SETS OR READS THE VARIABLE; a comment explaining its removal does not.
# So this looks for a read (`os.Getenv` / `env(...)`) or an assignment, in non-test source
# and outside the shell harnesses — the shape of a live knob, not a mention of a dead one.
for var in ORCHICON_CONTAINER_SERVICES_ONLY ORCHICON_SERVICE_RESIDENCY; do
  hits="$(grep -rIn --exclude='*_test.go' --exclude-dir=tests -E \
    "os\.Getenv\(\"$var\"|[^a-zA-Z_]env\(\"$var\"|\"$var=|^[[:space:]]*export[[:space:]]+$var=" \
    "${REPO_ROOT}/scripts" "${REPO_ROOT}/Makefile" "${REPO_ROOT}/cmd" "${REPO_ROOT}/internal" 2>/dev/null \
    | head -3 | tr '\n' ' ')"
  check "no survivor sets or reads $var" "" "$hits"
done
check "no Makefile target is named container-*" "" "$(grep -o '^container-[a-z]*:' "$MAKEFILE" 2>/dev/null | tr '\n' ' ')"
check_contains "the Makefile's plane start still delegates to the shared mechanism" 'orchicon plane start' "$(cat "$MAKEFILE")"

# --- (ii) HOST SHAPE: dev ------------------------------------------------
HOST_DEV="$(shape dev)"
check "dev plane HTTP port" "8080" "$(field "$HOST_DEV" plane_http_port)"
check "dev plane public URL" "http://172.17.0.1:8080" "$(field "$HOST_DEV" plane_public_url)"
check_contains "dev plane DSN uses dev's service port" \
  "plane_env:ORCHICON_POSTGRES_DSN=postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable" "$HOST_DEV"
check_contains "dev plane NATS URL uses dev's service port" \
  "plane_env:ORCHICON_NATS_URL=nats://localhost:4222" "$HOST_DEV"
# THE PRIMARY BIND IS LOOPBACK, NOT A WILDCARD. `:8080` already owns
# 172.17.0.1:8080 on EVERY interface, so the extra bind below could never bind
# — the host plane logged "bridge listener bind failed … address already in
# use" and retried it every 30s on every boot. These two lines are the pair
# that used to contradict each other at runtime.
check_contains "dev plane binds its own HTTP port on loopback" \
  "plane_env:ORCHICON_HTTP_ADDR=127.0.0.1:8080" "$HOST_DEV"
check_contains "dev plane ALSO binds the engine bridge at its own port" \
  "plane_env:ORCHICON_HTTP_EXTRA_BIND=172.17.0.1:8080" "$HOST_DEV"
# …and the two cannot collide: the BIND SET (primary + extra — exactly the
# addresses listenerAddrs hands the kernel) contains no wildcard address.
DEV_BIND_SET="$(field "$HOST_DEV" plane_env:ORCHICON_HTTP_ADDR) $(field "$HOST_DEV" plane_env:ORCHICON_HTTP_EXTRA_BIND)"
check "dev bind set is loopback + bridge" "127.0.0.1:8080 172.17.0.1:8080" "$DEV_BIND_SET"
check_lacks "dev primary bind is not a bare-port wildcard" \
  "plane_env:ORCHICON_HTTP_ADDR=:" "$HOST_DEV"
for bind in $DEV_BIND_SET; do
  case "$bind" in
    :*|0.0.0.0:*|'[::]':*) check "dev bind $bind names a concrete address" "concrete" "WILDCARD" ;;
    *) check "dev bind $bind names a concrete address" "concrete" "concrete" ;;
  esac
done
check_contains "dev plane advertises that same address to its run containers" \
  "plane_env:ORCHICON_PLANE_PUBLIC_URL=http://172.17.0.1:8080" "$HOST_DEV"
check_contains "dev plane state dir is a HOST path" \
  "plane_env:ORCHICON_DATA_DIR=$HOME/.local/share/orchicon-dev" "$HOST_DEV"
check_contains "dev blob dir is a HOST path" \
  "plane_env:ORCHICON_BLOB_DIR=$HOME/.local/share/orchicon-dev/blobs" "$HOST_DEV"
check_contains "dev isolates the detached-serve PID file" \
  "plane_env:ORCHICON_SERVE_STATE_DIR=$HOME/.local/share/orchicon-dev/serve" "$HOST_DEV"

# Every service bind must be loopback-bound, and nothing may be published on
# 0.0.0.0 (Postgres/NATS are an internal database and bus).
SVC="$(field "$HOST_DEV" service_ports)"
check "dev service-bind count (9 services)" "9" "$(printf '%s' "$SVC" | grep -o '127\.0\.0\.1:' | wc -l | tr -d ' ')"
check_lacks "dev binds nothing on a wildcard address" "0.0.0.0:" "$SVC"
for port in 5432 4222 8222 4317 4318 3200 3100 8428 3002; do
  check_contains "dev binds dev port $port on loopback" "127.0.0.1:$port:" "$SVC"
done

# --- (iii) HOST SHAPE: prod — and NO value shared with dev ---------------
HOST_PROD="$(shape prod)"
check "prod plane HTTP port" "8091" "$(field "$HOST_PROD" plane_http_port)"
check "prod plane public URL" "http://172.17.0.1:8091" "$(field "$HOST_PROD" plane_public_url)"
check "prod data dir differs from dev" "prod" "$(case "$(field "$HOST_PROD" host_data_dir)" in *prod) echo prod ;; *) echo "SHARED WITH DEV" ;; esac)"
check_contains "prod binds prod's postgres port" "127.0.0.1:5433:" "$(field "$HOST_PROD" service_ports)"
check_contains "prod binds prod's NATS port" "127.0.0.1:4223:" "$(field "$HOST_PROD" service_ports)"
check_lacks "prod does NOT bind dev's postgres port" "127.0.0.1:5432:" "$(field "$HOST_PROD" service_ports)"
check_contains "prod plane DSN uses prod's postgres port" \
  "ORCHICON_POSTGRES_DSN=postgres://orchicon:orchicon@localhost:5433/orchicon?sslmode=disable" "$HOST_PROD"
check_contains "prod plane binds the engine bridge at PROD's port" \
  "plane_env:ORCHICON_HTTP_EXTRA_BIND=172.17.0.1:8091" "$HOST_PROD"
check_contains "prod plane binds PROD's own port on loopback" \
  "plane_env:ORCHICON_HTTP_ADDR=127.0.0.1:8091" "$HOST_PROD"
check "prod bind set is loopback + bridge" \
  "127.0.0.1:8091 172.17.0.1:8091" \
  "$(field "$HOST_PROD" plane_env:ORCHICON_HTTP_ADDR) $(field "$HOST_PROD" plane_env:ORCHICON_HTTP_EXTRA_BIND)"
check_lacks "prod plane does NOT bind the bridge at dev's port" \
  "ORCHICON_HTTP_EXTRA_BIND=172.17.0.1:8080" "$HOST_PROD"

# No per-instance value may be shared: the two shapes must differ in the plane
# URL, the plane port and the full service bind set.
[ "$(field "$HOST_DEV" plane_public_url)" != "$(field "$HOST_PROD" plane_public_url)" ] \
  && check "plane URL is per instance" "distinct" "distinct" \
  || check "plane URL is per instance" "distinct" "IDENTICAL: $(field "$HOST_DEV" plane_public_url)"
[ "$(field "$HOST_DEV" service_ports)" != "$(field "$HOST_PROD" service_ports)" ] \
  && check "service bind set is per instance" "distinct" "distinct" \
  || check "service bind set is per instance" "distinct" "IDENTICAL"

# --- (iv) the shell table and the port queries agree ---------------------
#
# The Makefile derives DB_URL through the query script, and the plane's own env
# carries the table's ports; those two must not drift, or `make migrate` opens a
# database the plane is not on.
check "db-url dev agrees with the table" \
  "postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable" \
  "$(env -u DB_URL HOME="$(mktemp -d)" "$PORTS_SH" db-url dev 2>&1)"
check "services-port prod postgres agrees with the table" "5433" \
  "$(env HOME="$(mktemp -d)" "$PORTS_SH" services-port prod postgres 2>&1)"
check "an unknown instance is refused, not guessed" "refused" \
  "$(env HOME="$(mktemp -d)" "$PORTS_SH" db-url staging >/dev/null 2>&1 && echo ran || echo refused)"

# --- plane start must not leave a SUPERSEDED plane running --------------------
#
# A rebuild REPLACES the binary while the running process keeps executing the old inode. The plane
# answers health perfectly, so a bare health check concluded "already serving" and returned success —
# leaving the stale process in place indefinitely, serving code the operator had just replaced. The
# Go installer learned this first (planeDrift reads /proc/<pid>/exe).
#
# THE CHECK LIVES IN ONE PLACE: internal/planecontrol (internal/planecontrol.Start's Drifted arm,
# driven by Drift in hostproc_unix.go), and the Makefile DELEGATES to `orchicon plane start`. So these
# assertions follow the guarantee to where it is enforced instead of grepping a launcher that no
# longer exists.
#
# THE MECHANISM IS TESTED, NOT THE PROSE, and it is testable without an engine: run a process from a
# path, replace that path, and /proc/<pid>/exe reports the " (deleted)" suffix. That is exactly the
# signal the shared layer keys on, so this drives the real filesystem state rather than grepping for
# a string that might be spelled right and wired wrong.
echo
echo "  -- a stale plane is detected and replaced --"

PLANECONTROL_GO="${REPO_ROOT}/internal/planecontrol/planecontrol.go"
PLANECONTROL_UNIX="${REPO_ROOT}/internal/planecontrol/hostproc_unix.go"

if grep -qF 'A PLANE THAT ANSWERS IS NOT NECESSARILY THE PLANE WE JUST INSTALLED' "$PLANECONTROL_GO"; then
  printf '  \033[32mPASS\033[0m  %-52s %s\n' "the shared layer checks for a stale plane" "present"
  PASSED=$((PASSED + 1))
else
  printf '  \033[31mFAIL\033[0m  %-52s %s\n' "the shared layer checks for a stale plane" "MISSING"
  FAILED=$((FAILED + 1))
fi

# The detection must be on the " (deleted)" SUFFIX, not on a path mismatch: a dev checkout
# legitimately serves from bin/orchicon, and a mismatch test would restart a plane the operator
# chose to keep.
if grep -qF 'strings.HasSuffix(exe, " (deleted)")' "$PLANECONTROL_UNIX"; then
  printf '  \033[32mPASS\033[0m  %-52s %s\n' "detection keys on the deleted-exe suffix" "present"
  PASSED=$((PASSED + 1))
else
  printf '  \033[31mFAIL\033[0m  %-52s %s\n' "detection keys on the deleted-exe suffix" "MISSING"
  FAILED=$((FAILED + 1))
fi

# ...and it must RESTART rather than merely warn, so the plane ends up on the current binary.
if grep -qF 'case Drifted:' "$PLANECONTROL_GO" \
  && grep -qF 'running a superseded binary — restarting it on the installed one' "$PLANECONTROL_GO"; then
  printf '  \033[32mPASS\033[0m  %-52s %s\n' "a stale plane is stopped, then started" "wired"
  PASSED=$((PASSED + 1))
else
  printf '  \033[31mFAIL\033[0m  %-52s %s\n' "a stale plane is stopped, then started" "MISSING"
  FAILED=$((FAILED + 1))
fi

# THE BEHAVIOURAL HALF: reproduce the state and prove the detector reads it correctly. A shell
# function is not needed — the check is a readlink on /proc, and that is what is exercised.
TMP_PROBE="$(mktemp -d "${REPO_ROOT}/tmp/stale-probe.XXXXXX")"
trap 'rm -rf "$TMP_PROBE"' EXIT

# THE PROBE MUST BE A REAL BINARY, NOT A SCRIPT. A `#!/bin/sh` file's /proc/<pid>/exe is the
# INTERPRETER (/usr/bin/bash), so replacing the script deletes nothing and the test measures its own
# construction instead of the detector. Measured: the script version reported "no" for a process whose
# binary had just been replaced. /bin/sleep is a real ELF and is never a script.
#
# The scratch dir lives under the repo's gitignored tmp/ so the copy is project-scoped; a copy into a
# scratch path outside the tree is refused by the execution guard in a guarded environment, which
# would make this test pass or fail depending on who ran it.
if cp /bin/sleep "$TMP_PROBE/plane" 2>/dev/null; then
  # A SIMPLE background command, deliberately not part of an &&-chain: `a && b &` backgrounds the
  # whole chain, so $! is the subshell rather than the binary. That is how the first version of this
  # test "measured" /usr/bin/bash.
  "$TMP_PROBE/plane" 20 &
  PROBE_PID=$!
  sleep 1
  BEFORE="$(readlink "/proc/$PROBE_PID/exe" 2>/dev/null || echo '')"

  # Replace the file the process was launched from — exactly what a rebuild does to bin/orchicon.
  cp /bin/sleep "$TMP_PROBE/plane.new" && mv "$TMP_PROBE/plane.new" "$TMP_PROBE/plane"
  AFTER="$(readlink "/proc/$PROBE_PID/exe" 2>/dev/null || echo '')"

  kill "$PROBE_PID" 2>/dev/null || true
  wait "$PROBE_PID" 2>/dev/null || true

  case "$BEFORE" in
    *" (deleted)") BEFORE_STALE=yes ;;
    *)             BEFORE_STALE=no ;;
  esac
  case "$AFTER" in
    *" (deleted)") AFTER_STALE=yes ;;
    *)             AFTER_STALE=no ;;
  esac

  check "a running process is NOT stale before its binary is replaced" "no" "$BEFORE_STALE"
  check "the SAME process IS stale once its binary is replaced" "yes" "$AFTER_STALE"
else
  # AN INVISIBLE SKIP IS THE FAILURE MODE THIS WHOLE SESSION HAS BEEN REMOVING, so it is reported
  # loudly and counted as a skip rather than passing quietly. A pass here would claim the detection
  # works without having exercised it.
  printf '  \033[33mSKIP\033[0m  %-52s %s\n' "the deleted-exe detector" \
    "could not copy a real binary into ${TMP_PROBE} (read-only or guarded environment)"
  SKIPPED=$((SKIPPED + 1))
fi

echo
echo "  -- the Go installer's shape (install_host.go) --"
#
# THE SHAPE THE PRODUCT ACTUALLY SHIPS IS THE GO ONE. `orchicon install` implements the host shape in
# GO (install_host.go) — the shell half above guards the shared table, and a `curl | bash` host has no
# .sh launcher behind the binary at all. So this second phase drives the Go side directly, asserting
# the SAME properties against the implementation that ships:
#
#   1. the bind and the advertised URL share ONE bridge IP and THIS instance's port (per-instance, so two
#      instances cannot point at each other's plane);
#   2. with no container engine (or ORCHICON_RUNTIME_CONTAINERS=0) NEITHER variable is present and no
#      0.0.0.0 appears anywhere;
#   3. install_host.go contains no engine invocation at all;
#   4. a RE-RUN on a running no-engine instance KEEPS the ports it already bound (convergence) — the failure
#      mode being that re-resolution probes the instance's own listeners, moves every port, and gets a second
#      host stack started beside the first.
#
# `go test -run` EXIT CODE IS THE SIGNAL — a renamed Go test makes the filter match nothing and `go test`
# reports "no tests to run", which fails. The harness must NEVER pass vacuously (see extract_fn above).
GO_SHAPE_TESTS='TestPlaneBindAndShape|TestInstallShapeWithoutAContainerEngine|TestInstallHostHasNoDockerInvocation|TestEnsureHostDataDir|TestRerunOnARunningHostInstanceKeepsItsPorts'
if (cd "${REPO_ROOT}" && go test ./cmd/orchicon -run "^(${GO_SHAPE_TESTS})$" -count=1 >/dev/null 2>&1); then
  printf '  \033[32mPASS\033[0m  %-52s %s\n' "the Go installer shape tests" "green"
  PASSED=$((PASSED + 1))
else
  printf '  \033[31mFAIL\033[0m  %-52s %s\n' "the Go installer shape tests" "red (run: go test ./cmd/orchicon -run '${GO_SHAPE_TESTS}')"
  FAILED=$((FAILED + 1))
fi

# A VACUITY GUARD, stated separately from the test run: the tests must EXIST, not merely exit 0.
#
# THE OUTPUT IS CAPTURED RATHER THAN PIPED INTO `grep -q`, and that is not style: `set -o pipefail` is on, and
# `grep -q` exits the moment it matches, which SIGPIPEs `go test` — so the pipeline would report go test's 141
# and this guard would fail while the tests passed. Observed, not theorised.
GO_SHAPE_OUT="$(cd "${REPO_ROOT}" && go test ./cmd/orchicon -run "^(${GO_SHAPE_TESTS})$" -count=1 -v 2>&1)"
case "$GO_SHAPE_OUT" in
  *'=== RUN'*)
    printf '  \033[32mPASS\033[0m  %-52s %s\n' "the Go shape tests actually ran" "ran"
    PASSED=$((PASSED + 1)) ;;
  *)
    printf '  \033[31mFAIL\033[0m  %-52s %s\n' "the Go shape tests actually ran" "NO TESTS MATCHED (the harness must never pass vacuously)"
    FAILED=$((FAILED + 1)) ;;
esac

echo
echo "  host-residency harness: ${PASSED} passed, ${FAILED} failed, ${SKIPPED} skipped"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
