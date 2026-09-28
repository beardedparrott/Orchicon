#!/usr/bin/env bash
# ============================================================================
# Offline harness for the HOST-RESIDENCY launch shape in scripts/container.sh.
#
# WHY THIS EXISTS: host residency is a PER-INSTANCE, OPT-IN launch setting. Two
# failure modes are silent and expensive, and neither needs Docker to detect:
#
#   1. A non-default default. If the residency switch ever flips to `host`,
#      every un-migrated instance changes shape on its next `up` — the plane
#      silently leaves its container. The opt-in (default `container`) is the
#      whole safety property of the migration, so it is asserted here.
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
# WHAT IS REAL HERE: container.sh's own functions, EXTRACTED BY TEXT AND NEVER
# EXECUTED — the script drives Docker, so sourcing it would run it (and its
# `case` block exits). Extraction is asserted, so renaming a function upstream
# fails here loudly instead of silently testing nothing.
#
#   scripts/tests/host-residency/run.sh [container.sh]
#
# Exits 0 only when every assertion passes, so it can be used as a check.
# ============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
CONTAINER_SH="${REPO_ROOT}/scripts/container.sh"
[ "$#" -gt 0 ] && CONTAINER_SH="$1"

# The harness's own environment must not decide any case.
unset ORCHICON_PLANE_RESIDENCY 2>/dev/null || true
unset ORCHICON_CONTAINER_SERVICES_ONLY 2>/dev/null || true

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
extract_fn() {
  awk -v fn="$1" '
    $0 == fn "() {" { inf = 1 }
    inf { print }
    inf && $0 == "}" { exit }
  ' "$CONTAINER_SH"
}

echo "container.sh under test: ${CONTAINER_SH}"
echo

for fn in instance_info residency_for plane_env print_shape container_residency_from_env bridge_ip bridge_bind_env; do
  body="$(extract_fn "$fn")"
  if [ -z "$body" ]; then
    echo "run.sh: could not extract ${fn}() from ${CONTAINER_SH}" >&2
    echo "        renamed? this harness must never pass vacuously." >&2
    exit 2
  fi
  eval "$body"
done

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

# --- (i) OPT-IN: no setting at all means today's behaviour ------------------
DEF_DEV="$(shape dev)"
check "default residency (unset) is container" "container" "$(field "$DEF_DEV" residency)"
check "default dev publishes the plane 8080:8080" "-p 8080:8080 -p 3002:3000" "$(field "$DEF_DEV" publish_ports)"
check_lacks "default dev sets no services-only env" "SERVICES_ONLY" "$DEF_DEV"
check_lacks "default dev publishes no loopback-bound port" "127.0.0.1:" "$(field "$DEF_DEV" publish_ports)"
check "explicit container == default" "$DEF_DEV" "$(shape dev container)"

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
check "prod stays containerized by default (dev's residency is not global)" \
  "container" "$(field "$(shape prod)" residency)"
check "prod default still publishes its plane port" \
  "-p 8091:8080 -p 3003:3000" "$(field "$(shape prod)" publish_ports)"

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

echo
echo "  host-residency harness: ${PASSED} passed, ${FAILED} failed"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
