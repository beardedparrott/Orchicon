#!/usr/bin/env bash
# ============================================================================
# Offline harness for the HOST-RESIDENCY launch shape.
#
# WHY THIS EXISTS: host residency is a PER-INSTANCE, OPT-IN launch setting. Two
# failure modes are silent and expensive, and neither needs Docker to detect:
#
#   1. THE DEFAULT SHAPE. It was `container`, meaning the self-contained shape
#      was what a plain `up` produced while the HOST plane — the supported one,
#      where operators actually run — needed an explicit opt-in. 5cc8039c
#      ("host is the default shape, and the card states intent") reversed that on
#      purpose, so `host` is now asserted as the default and `container` as the
#      explicit parameter. The PRECEDENCE is asserted too (argument >
#      ORCHICON_PLANE_RESIDENCY > host, with a typo refused), because that is
#      what makes `container` still reachable rather than merely still present.
#   2. A GLOBAL value. One shared HTTP port / plane URL / service port between
#      dev and prod points one instance's workers at the other's services and
#      database. Every per-instance value is therefore asserted to differ AND
#      to come from the instance table.
#   3. A DERIVED-BUT-WRONG plane URL. The host plane's URL is no longer a
#      literal: it is bridge_bind_env's output (the resolved docker bridge +
#      THIS instance's port), so the bridge is pinned below to keep the
#      assertion deterministic and docker-free.
#   4. A COLLIDING BIND SET. The host plane's two listeners must be DISTINCT
#      concrete addresses: a wildcard primary (`:8080`) already owns the
#      bridge address on every interface, so the extra bind could never bind
#      and the plane retried a doomed net.Listen every 30s on every boot. The
#      bind set (primary + extra) is asserted to contain no wildcard.
#
# It also pins the security boundary: every host-residency service publish is
# bound to 127.0.0.1. The supervisor adds pg_hba `trust` rules for the published
# connection (the peer is the docker bridge gateway, not loopback), so a publish
# that is NOT loopback-bound would expose Postgres/NATS to the LAN.
#
# WHAT IS REAL HERE: the real functions, EXTRACTED BY TEXT AND NEVER EXECUTED —
# the scripts drive the container engine, so sourcing them would run them.
# Extraction is asserted, so renaming a function upstream fails here loudly
# instead of silently testing nothing.
#
# WHERE THEY LIVE MOVED, AND THE ASSERTIONS DID NOT. The per-instance table and
# the launch shape it composes (instance_info, residency_for, plane_env,
# bridge_ip, bridge_bind_env) are now in scripts/lib/instances.sh — the ONE place
# the shell side enumerates dev/test/prod — while the verbs that consume them
# (print_shape, up_instance, plane_start) stay in scripts/container.sh. This
# harness reads BOTH plus the engine resolver, so it keeps guarding the same
# properties rather than being pointed at a file that no longer has a subject.
#
#   scripts/tests/host-residency/run.sh [instances.sh]
#
# Exits 0 only when every assertion passes, so it can be used as a check.
# ============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
CONTAINER_SH="${REPO_ROOT}/scripts/container.sh"
INSTANCES_SH="${REPO_ROOT}/scripts/lib/instances.sh"
ENGINE_SH="${REPO_ROOT}/scripts/lib/engine.sh"
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
echo "launcher under test:        ${CONTAINER_SH}"
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

# The per-instance table + the shape it composes (lib/instances.sh), the engine
# resolver it now asks for the bridge (lib/engine.sh), and the verbs that consume
# them (container.sh).
from_one "$INSTANCES_SH" instance_info residency_for plane_env bridge_ip bridge_bind_env
from_one "$ENGINE_SH" engine_cmd
from_one "$CONTAINER_SH" print_shape container_residency_from_env

# residency_for/print_shape log through container.sh's helpers; stub them so the
# harness stays independent of the script's colour handling.
log_err() { echo "ERR: $*" >&2; }
log_dim() { :; }
log_ok() { :; }
log_warn() { :; }

# shape <instance> [residency] — the instance's shape as the launcher computes it.
shape() {
  local inst="$1" residency="${2-}"
  (
    if [ -n "$residency" ]; then
      ORCHICON_PLANE_RESIDENCY="$residency"
    else
      unset ORCHICON_PLANE_RESIDENCY
    fi
    export ORCHICON_PLANE_RESIDENCY="${ORCHICON_PLANE_RESIDENCY-}"
    [ -n "$residency" ] || unset ORCHICON_PLANE_RESIDENCY
    # The host plane's bind and URL are DERIVED from the docker bridge
    # (bridge_bind_env), never literal. Pin the bridge so this harness stays
    # docker-free and deterministic — the same seam `plane-bind` documents.
    export ORCHICON_DOCKER_BRIDGE_IP="${ORCHICON_DOCKER_BRIDGE_IP:-172.17.0.1}"
    print_shape "$inst"
  ) 2>&1
}

field() { # <shape> <key>
  printf '%s\n' "$1" | sed -n "s/^$2=//p" | head -1
}

# --- (i) THE DEFAULT SHAPE IS HOST -----------------------------------------
#
# REWRITTEN, NOT RELAXED, and the difference matters for a harness that guards a shape. Until
# 5cc8039c this block asserted that the default was `container` and that a plain `up` therefore
# published the plane on the wildcard. Both of those are now FALSE ON PURPOSE: the default moved to
# `host`, so a plain `up` produces the host shape — the container services-only, every service
# published on loopback, and no plane port published by the container at all (the plane is a HOST
# process that owns it).
#
# So the assertions below are the SAME PROPERTIES, restated for the shape that is now the default,
# rather than the weaker "it no longer matches container". A harness gutted to match new behaviour
# would stop guarding anything.
DEF_DEV="$(shape dev)"
check "default residency (unset) is host" "host" "$(field "$DEF_DEV" residency)"
check_contains "default dev marks the container services-only" \
  "container_env=ORCHICON_CONTAINER_SERVICES_ONLY=1" "$DEF_DEV"
check_contains "default dev publishes services on loopback" \
  "127.0.0.1:5432:" "$(field "$DEF_DEV" service_ports)"
check_lacks "default dev publishes nothing on a wildcard address" "0.0.0.0:" "$(field "$DEF_DEV" service_ports)"
# The plane port is NOT published by the container in the host shape — the plane is a host process and
# owns the port. Asserting its absence is the property that makes host residency host residency.
check_lacks "default dev does NOT publish the plane port (it is a host process)" \
  "8080:8080" "$(field "$DEF_DEV" publish_ports)"

# AND THE OPT-IN STILL WORKS, which is the other half of the change rather than a leftover.
check "explicit container is container" "container" "$(field "$(shape dev container)" residency)"
check "explicit container publishes the plane 8080:8080" "-p 8080:8080 -p 3002:3000" \
  "$(field "$(shape dev container)" publish_ports)"
check_lacks "explicit container sets no services-only env" "SERVICES_ONLY" "$(shape dev container)"

# --- (ii) HOST SHAPE: dev ------------------------------------------------
HOST_DEV="$(shape dev host)"
check "host residency reported" "host" "$(field "$HOST_DEV" residency)"
check "host dev plane HTTP port" "8080" "$(field "$HOST_DEV" plane_http_port)"
check "host dev plane public URL" "http://172.17.0.1:8080" "$(field "$HOST_DEV" plane_public_url)"
check_contains "host dev marks the container services-only" \
  "container_env=ORCHICON_CONTAINER_SERVICES_ONLY=1" "$HOST_DEV"
check_contains "host dev plane DSN uses the published loopback port" \
  "plane_env:ORCHICON_POSTGRES_DSN=postgres://orchicon:orchicon@localhost:5432/orchicon?sslmode=disable" "$HOST_DEV"
check_contains "host dev plane NATS URL uses the published loopback port" \
  "plane_env:ORCHICON_NATS_URL=nats://localhost:4222" "$HOST_DEV"
# THE PRIMARY BIND IS LOOPBACK, NOT A WILDCARD. `:8080` already owns
# 172.17.0.1:8080 on EVERY interface, so the extra bind below could never bind
# — the host plane logged "bridge listener bind failed … address already in
# use" and retried it every 30s on every boot. These two lines are the pair
# that used to contradict each other at runtime.
check_contains "host dev plane binds its own HTTP port on loopback" \
  "plane_env:ORCHICON_HTTP_ADDR=127.0.0.1:8080" "$HOST_DEV"
check_contains "host dev plane ALSO binds the docker bridge at its own port" \
  "plane_env:ORCHICON_HTTP_EXTRA_BIND=172.17.0.1:8080" "$HOST_DEV"
# …and the two cannot collide: the BIND SET (primary + extra — exactly the
# addresses listenerAddrs hands the kernel) contains no wildcard address.
DEV_BIND_SET="$(field "$HOST_DEV" plane_env:ORCHICON_HTTP_ADDR) $(field "$HOST_DEV" plane_env:ORCHICON_HTTP_EXTRA_BIND)"
check "host dev bind set is loopback + bridge" "127.0.0.1:8080 172.17.0.1:8080" "$DEV_BIND_SET"
check_lacks "host dev primary bind is not a bare-port wildcard" \
  "plane_env:ORCHICON_HTTP_ADDR=:" "$HOST_DEV"
for bind in $DEV_BIND_SET; do
  case "$bind" in
    :*|0.0.0.0:*|'[::]':*) check "host dev bind $bind names a concrete address" "concrete" "WILDCARD" ;;
    *) check "host dev bind $bind names a concrete address" "concrete" "concrete" ;;
  esac
done
check_contains "host dev plane advertises that same address to its run containers" \
  "plane_env:ORCHICON_PLANE_PUBLIC_URL=http://172.17.0.1:8080" "$HOST_DEV"
check_contains "host dev plane state dir is a HOST path" \
  "plane_env:ORCHICON_DATA_DIR=$HOME/.local/share/orchicon-dev" "$HOST_DEV"
check_contains "host dev blob dir is a HOST path" \
  "plane_env:ORCHICON_BLOB_DIR=$HOME/.local/share/orchicon-dev/blobs" "$HOST_DEV"
check_contains "host dev isolates the detached-serve PID file" \
  "plane_env:ORCHICON_SERVE_STATE_DIR=$HOME/.local/share/orchicon-dev/serve" "$HOST_DEV"

# Every service publish must be loopback-bound, and nothing may be published on
# 0.0.0.0 (the pg_hba trust rules are only safe because of this).
SVC="$(field "$HOST_DEV" service_ports)"
check "host dev publish count (9 services)" "9" "$(printf '%s' "$SVC" | grep -o '127\.0\.0\.1:' | wc -l | tr -d ' ')"
check_lacks "host dev publishes nothing on a wildcard address" "0.0.0.0:" "$SVC"
for port in 5432 4222 8222 4317 4318 3200 3100 8428 3002; do
  check_contains "host dev publishes dev port $port on loopback" "127.0.0.1:$port:" "$SVC"
done
check "host dev publishes the same set it reports" "$SVC" "$(field "$HOST_DEV" publish_ports)"

# --- (iii) HOST SHAPE: prod — and NO value shared with dev ---------------
HOST_PROD="$(shape prod host)"
check "host prod plane HTTP port" "8091" "$(field "$HOST_PROD" plane_http_port)"
check "host prod plane public URL" "http://172.17.0.1:8091" "$(field "$HOST_PROD" plane_public_url)"
check "host prod data dir differs from dev" "prod" "$(case "$(field "$HOST_PROD" host_data_dir)" in *prod) echo prod ;; *) echo "SHARED WITH DEV" ;; esac)"
check_contains "host prod publishes prod postgres port" "127.0.0.1:5433:" "$(field "$HOST_PROD" service_ports)"
check_contains "host prod publishes prod NATS port" "127.0.0.1:4223:" "$(field "$HOST_PROD" service_ports)"
check_lacks "host prod does NOT publish dev's postgres port" "127.0.0.1:5432:" "$(field "$HOST_PROD" service_ports)"
check_contains "host prod plane DSN uses prod's postgres port" \
  "ORCHICON_POSTGRES_DSN=postgres://orchicon:orchicon@localhost:5433/orchicon?sslmode=disable" "$HOST_PROD"
check_contains "host prod plane binds the docker bridge at PROD's port" \
  "plane_env:ORCHICON_HTTP_EXTRA_BIND=172.17.0.1:8091" "$HOST_PROD"
check_contains "host prod plane binds PROD's own port on loopback" \
  "plane_env:ORCHICON_HTTP_ADDR=127.0.0.1:8091" "$HOST_PROD"
check "host prod bind set is loopback + bridge" \
  "127.0.0.1:8091 172.17.0.1:8091" \
  "$(field "$HOST_PROD" plane_env:ORCHICON_HTTP_ADDR) $(field "$HOST_PROD" plane_env:ORCHICON_HTTP_EXTRA_BIND)"
check_lacks "host prod plane does NOT bind the bridge at dev's port" \
  "ORCHICON_HTTP_EXTRA_BIND=172.17.0.1:8080" "$HOST_PROD"

# No per-instance value may be shared: the two host shapes must differ in the
# plane URL, the plane port and the full service publish set.
[ "$(field "$HOST_DEV" plane_public_url)" != "$(field "$HOST_PROD" plane_public_url)" ] \
  && check "plane URL is per instance" "distinct" "distinct" \
  || check "plane URL is per instance" "distinct" "IDENTICAL: $(field "$HOST_DEV" plane_public_url)"
[ "$(field "$HOST_DEV" service_ports)" != "$(field "$HOST_PROD" service_ports)" ] \
  && check "service publish set is per instance" "distinct" "distinct" \
  || check "service publish set is per instance" "distinct" "IDENTICAL"

# --- (iv) prod does NOT follow dev by default ----------------------------
#
# THE POINT OF THIS BLOCK SURVIVES THE DEFAULT CHANGE, and it is easy to lose while updating it: the
# property is that dev's residency is not GLOBAL, i.e. prod resolves to the same shape dev does for
# the same (absent) input, from prod's own instance entry rather than from anything dev set. Both are
# now `host`, so the label says that rather than "stays containerized" — and prod's ports must still
# be PROD's, which is what catches a table row that fell back to dev's values.
check "prod follows the same default as dev (dev's residency is not global)" \
  "host" "$(field "$(shape prod)" residency)"
check_contains "prod default publishes prod's own postgres port" \
  "127.0.0.1:5433:" "$(field "$(shape prod)" service_ports)"
check_lacks "prod default does NOT publish dev's postgres port" \
  "127.0.0.1:5432:" "$(field "$(shape prod)" service_ports)"

# --- (v) an invalid residency is refused, not guessed --------------------
if (ORCHICON_PLANE_RESIDENCY=both; export ORCHICON_PLANE_RESIDENCY; residency_for >/dev/null 2>&1); then
  check "invalid residency is refused" "non-zero exit" "exit 0"
else
  check "invalid residency is refused" "non-zero exit" "non-zero exit"
fi

# --- wiring: the plane gate must stay per invocation ---------------------
# Static by necessity (up_instance drives Docker). It pins the two places that
# make coexistence real: `up` reads residency per invocation, and the container
# is created with the services-only flag only on the host path.
if grep -qF 'RESIDENCY=$(residency_for) || return 1' "$CONTAINER_SH"; then
  printf '  \033[32mPASS\033[0m  %-52s %s\n' "up_instance reads residency per invocation" "wired"
  PASSED=$((PASSED + 1))
else
  printf '  \033[31mFAIL\033[0m  %-52s %s\n' "up_instance reads residency per invocation" "MISSING"
  FAILED=$((FAILED + 1))
fi
if grep -qF 'EXTRA_RUN_ARGS+=(-e ORCHICON_CONTAINER_SERVICES_ONLY=1)' "$CONTAINER_SH"; then
  printf '  \033[32mPASS\033[0m  %-52s %s\n' "services-only flag reaches the container" "wired"
  PASSED=$((PASSED + 1))
else
  printf '  \033[31mFAIL\033[0m  %-52s %s\n' "services-only flag reaches the container" "MISSING"
  FAILED=$((FAILED + 1))
fi
# --- shape guard: an existing container built for the OTHER residency -------
# `docker start` cannot rewrite create-time properties (the services-only flag,
# the published ports). Starting a container built for the other shape silently
# gives the instance NO plane (host -> container) or leaves the plane in the
# wrong place (container -> host), so `up` must compare the shape it finds with
# the one it wants and RECREATE on a mismatch.
check "container created services-only is host" "host" \
  "$(container_residency_from_env 'PATH=/usr/bin
ORCHICON_CONTAINER_SERVICES_ONLY=1
ORCHICON_INSTANCE=dev')"
check "container without the flag is container" "container" \
  "$(container_residency_from_env 'PATH=/usr/bin
ORCHICON_CONTAINER_MODE=1
ORCHICON_INSTANCE=dev')"
check "no container env at all is container" "container" "$(container_residency_from_env '')"
check "an explicit SERVICES_ONLY=0 is container" "container" \
  "$(container_residency_from_env 'ORCHICON_CONTAINER_SERVICES_ONLY=0')"
check "ORCHICON_CONTAINER_MODE cannot be mistaken for it" "container" \
  "$(container_residency_from_env 'ORCHICON_CONTAINER_MODE=1')"
if grep -qF 'if [ "$existing_residency" != "$RESIDENCY" ]; then' "$CONTAINER_SH" \
  && grep -qF 'existing_residency=$(container_residency_from_env "$(container_env_dump "$NAME")")' "$CONTAINER_SH"; then
  printf '  \033[32mPASS\033[0m  %-52s %s\n' "up_instance recreates a container of the other shape" "wired"
  PASSED=$((PASSED + 1))
else
  printf '  \033[31mFAIL\033[0m  %-52s %s\n' "up_instance recreates a container of the other shape" "MISSING"
  FAILED=$((FAILED + 1))
fi

# The host data dir (KEK, ask-history) is a DIFFERENT path from the container's
# data volume, so the switch-over must actually copy it. A defined-but-never-
# called migrate_host_data_dir silently mints a new KEK and every tenant secret
# written by the containerized plane stops decrypting.
if grep -qF 'migrate_host_data_dir "$inst" || return 1' "$CONTAINER_SH"; then
  printf '  \033[32mPASS\033[0m  %-52s %s\n' "host switch-over migrates the data dir (KEK)" "wired"
  PASSED=$((PASSED + 1))
else
  printf '  \033[31mFAIL\033[0m  %-52s %s\n' "host switch-over migrates the data dir (KEK)" "MISSING"
  FAILED=$((FAILED + 1))
fi

# --- plane_start must not leave a SUPERSEDED plane running --------------------
#
# A rebuild REPLACES the binary while the running process keeps executing the old inode. The plane
# answers health perfectly, so a bare health check concluded "already serving" and returned success —
# leaving the stale process in place indefinitely, serving code the operator had just replaced. The
# Go installer learned this first (planeDrift reads /proc/<pid>/exe).
#
# THE CHECK NOW LIVES IN ONE PLACE: internal/planecontrol (internal/planecontrol.Start's Drifted arm,
# driven by Drift in hostproc_unix.go), and the launcher DELEGATES to `orchicon plane start`. The bash
# body that used to implement it here is gone, so these assertions follow the guarantee to where it is
# enforced instead of grepping a file that no longer holds it.
#
# THE MECHANISM IS TESTED, NOT THE PROSE, and it is testable without Docker: run a process from a
# path, replace that path, and /proc/<pid>/exe reports the " (deleted)" suffix. That is exactly the
# signal the shared layer keys on, so this drives the real filesystem state rather than grepping for
# a string that might be spelled right and wired wrong.
echo
echo "  -- a stale plane is detected and replaced --"

PLANECONTROL_GO="${REPO_ROOT}/internal/planecontrol/planecontrol.go"
PLANECONTROL_UNIX="${REPO_ROOT}/internal/planecontrol/hostproc_unix.go"

if grep -qF 'ORCHICON_INSTANCE="$inst" "$bin" plane start' "$CONTAINER_SH"; then
  printf '  \033[32mPASS\033[0m  %-52s %s\n' "plane_start delegates to the shared mechanism" "wired"
  PASSED=$((PASSED + 1))
else
  printf '  \033[31mFAIL\033[0m  %-52s %s\n' "plane_start delegates to the shared mechanism" "MISSING"
  FAILED=$((FAILED + 1))
fi

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
# THE HARNESS'S ORIGINAL TARGET WAS container.sh, AND THE GAP THAT MATTERS IS THE OTHER LAUNCHER.
# `orchicon install` implements the SAME host-residency shape in GO (install_host.go) — the bash half above no
# longer guards it, because a `curl | bash` host has no container.sh behind the binary at all. So this second
# phase drives the Go side directly, asserting the SAME three properties against the implementation that
# actually ships:
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
echo "  host-residency harness: ${PASSED} passed, ${FAILED} failed"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
