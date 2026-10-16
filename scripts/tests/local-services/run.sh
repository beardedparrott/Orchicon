#!/usr/bin/env bash
# ============================================================================
# Offline harness for the LOCAL-SERVICES inner loop.
#
# WHY THIS EXISTS: with the instance container gone, `make run` and `make migrate`
# have no backend. This work item wires them to services the DEVELOPER starts on
# the host (`make services`), and four properties of that wiring are silent when
# they break — none needs Docker or a database to detect:
#
#   1. A LITERAL PORT. DB_URL used to be a hardcoded 5432, so a prod checkout's
#      `make migrate` reached DEV's database, and two instances on one host
#      collided. It is DERIVED per instance now, and that is asserted here for
#      dev AND prod, plus the ORCHICON_*_PORT override rung.
#   2. A MISSING PREREQUISITE. `make run` must start the services first — but
#      `migrate`/`rls-check`/`test` must NOT, because CI runs those after
#      `container up` against the container DB and starting host services there
#      would double-bind 5432.
#   3. A BROKEN TEST ISOLATION CONTRACT. `make test` unsets the ambient config /
#      live-plane variables; that list must survive untouched and the OPT-IN
#      variables (ORCHICON_TEST_DSN) must stay reachable.
#   4. AN UNDOCUMENTED TARGET. `make help` is generated from `##` comments, so a
#      new target without one is invisible.
#
# WHAT IS REAL HERE: scripts/container.sh's pure query verbs are EXECUTED (they
# touch no Docker and no state), and the Makefile is driven with `make -n` /
# `make help`. The Go side is guarded by `go test ./cmd/orchicon` in ci-go, so this
# harness does not shell out to Go.
#
#   scripts/tests/local-services/run.sh
#
# Exits 0 only when every assertion passes, so it can be used as a check.
# ============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
CONTAINER_SH="${REPO_ROOT}/scripts/container.sh"
MAKEFILE="${REPO_ROOT}/Makefile"

# The harness's own environment must not decide any case, and a PARENT make's
# flags must not leak into the child make calls (a `-n`/`-s`/jobserver-inherited
# MAKEFLAGS turns `make help` into a printout of what it would run).
unset MAKEFLAGS MFLAGS 2>/dev/null || true
unset ORCHICON_INSTANCE ORCHICON_POSTGRES_PORT ORCHICON_NATS_PORT ORCHICON_NATS_MONITOR_PORT 2>/dev/null || true

PASSED=0
FAILED=0
SKIPPED=0

check() { # <label> <expected> <actual>
  if [ "$2" = "$3" ]; then
    printf '  \033[32mPASS\033[0m  %-56s %s\n' "$1" "$3"
    PASSED=$((PASSED + 1))
  else
    printf '  \033[31mFAIL\033[0m  %-56s want [%s] got [%s]\n' "$1" "$2" "$3"
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

# strip_banners removes recursive-make directory banners so the assertions below
# match on recipe text rather than on make's bookkeeping.
strip_banners() {
  sed -e '/^make\[[0-9]*\]: Entering directory/d' \
      -e '/^make\[[0-9]*\]: Leaving directory/d' \
      -e '/^make: Entering directory/d' \
      -e '/^make: Leaving directory/d'
}

# cs <verb args...> — run scripts/container.sh with the override knobs cleared.
cs() {
  ( cd "$REPO_ROOT" && \
    env -u ORCHICON_POSTGRES_PORT -u ORCHICON_NATS_PORT -u ORCHICON_NATS_MONITOR_PORT \
      "$CONTAINER_SH" "$@" ) 2>&1
}

make_n() { ( cd "$REPO_ROOT" && make -n "$@" ) 2>&1 | strip_banners; }
make_do() { ( cd "$REPO_ROOT" && make "$@" ) 2>&1 | strip_banners; }

echo "container.sh under test: ${CONTAINER_SH}"
echo "Makefile under test:     ${MAKEFILE}"
echo

# --- (i) the pure per-instance query verbs --------------------------------
echo "  -- container.sh services-port / db-url (the derived port table) --"

check "services-port dev postgres"  "5432" "$(cs services-port dev postgres)"
check "services-port prod postgres" "5433" "$(cs services-port prod postgres)"
check "services-port test postgres" "5434" "$(cs services-port test postgres)"
check "services-port dev nats"      "4222" "$(cs services-port dev nats)"
check "services-port prod nats"     "4223" "$(cs services-port prod nats)"
check "services-port dev nats-mon"  "8222" "$(cs services-port dev nats-mon)"

# THE OVERRIDE RUNG: same knobs the installer honours, at instance_info's one
# choke point — so the derived DSN cannot disagree with the bound port.
OVR="$(cd "$REPO_ROOT" && ORCHICON_POSTGRES_PORT=6543 "$CONTAINER_SH" services-port dev postgres 2>&1)"
check "services-port honours ORCHICON_POSTGRES_PORT" "6543" "$OVR"

# THE SHELL HALF MUST MIRROR THE GO HALF'S RECORD-FIRST RULE. `orchicon services
# up` binds the ports the instance's install RECORDED (ports.json) when it has
# them, and the defaults+overrides otherwise; a DSN derived the other way round
# would point the plane at a port the started services never bound. The record is
# ALL-OR-NOTHING (both postgres and nats present), exactly as servicesPortsFor
# decides, so a partial record falls back rather than resolving key-by-key.
REC_HOME="$(mktemp -d)"
mkdir -p "$REC_HOME/.local/share/orchicon-dev"
printf '{\n  "ORCHICON_POSTGRES_PORT": 6001,\n  "ORCHICON_NATS_PORT": 6002,\n  "ORCHICON_CONTROL_PORT": 6003\n}\n' \
  > "$REC_HOME/.local/share/orchicon-dev/ports.json"
check "services-port uses the RECORDED postgres port" "6001" "$(cd "$REPO_ROOT" && HOME="$REC_HOME" "$CONTAINER_SH" services-port dev postgres 2>&1)"
check "services-port uses the RECORDED control port" "6003" "$(cd "$REPO_ROOT" && HOME="$REC_HOME" "$CONTAINER_SH" services-port dev control 2>&1)"
check_contains "db-url agrees with the RECORD" "6001" "$(cd "$REPO_ROOT" && HOME="$REC_HOME" "$CONTAINER_SH" db-url dev 2>&1)"
printf '{\n  "ORCHICON_POSTGRES_PORT": 6001\n}\n' > "$REC_HOME/.local/share/orchicon-dev/ports.json"
check "a PARTIAL record falls back to the default (all-or-nothing)" "5432" "$(cd "$REPO_ROOT" && HOME="$REC_HOME" "$CONTAINER_SH" services-port dev postgres 2>&1)"

check_contains "db-url dev uses 5432"  ":5432/" "$(cs db-url dev)"
check_contains "db-url prod uses 5433" ":5433/" "$(cs db-url prod)"
check_contains "db-url is a postgres DSN" "postgres://orchicon:orchicon@localhost:" "$(cs db-url dev)"

# An unknown service is a REFUSAL, not a silent 0 — a typo must not look like a port.
cs services-port dev bogus >/dev/null 2>&1
check "unknown service is refused" "1" "$?"

# The 3rd-arg residency guard consults a REAL third word for services-port and
# must not claim it, while still refusing a bad shape on the management verbs.
cs services-port dev postgres >/dev/null 2>&1
check "the shape guard does not claim services-port's 3rd word" "0" "$?"
cs up dev bogus >/dev/null 2>&1
check "a bad residency shape is still refused (rc 2)" "2" "$?"

# --- (ii) DB_URL is DERIVED from the instance ----------------------------
echo
echo "  -- Makefile DB_URL is per-instance (no 5432 literal) --"

MIGRATE_PROD="$(make_n migrate instance=prod)"
check_contains "make migrate instance=prod resolves a :5433 DSN"  ":5433/" "$MIGRATE_PROD"
check_lacks    "make migrate instance=prod does not fall back to dev's 5432" ":5432/" "$MIGRATE_PROD"

MIGRATE_DEV="$(make_n migrate)"
check_contains "make migrate (default) resolves dev's :5432 DSN" ":5432/" "$MIGRATE_DEV"
# THE ENGINE, not just the DSN. The Atlas CLI cannot read this migration directory at all (it
# rejects the paired *_down.sql as a duplicate version) and it needs a network install, so an
# offline inner loop must apply through the repo's own engine - the one the plane and CI use.
check_contains "make migrate applies through the repo's own engine" "atlas-ci" "$MIGRATE_DEV"
check_lacks    "make migrate needs no atlas CLI install (offline)" "atlasgo.sh" "$MIGRATE_DEV"

RLS_PROD="$(make_n rls-check instance=prod)"
check_contains "make rls-check instance=prod uses the same :5433 DSN" ":5433/" "$RLS_PROD"
# THE INSTANCE TRAVELS WITH THE DSN. check-rls.sh resolves the instance's OWN
# downloaded psql from ORCHICON_INSTANCE, so a prod run that left it at dev would
# reach for dev's record (and then the dev container) — the wrong instance's tools.
check_contains "make rls-check instance=prod tells check-rls.sh which instance" "ORCHICON_INSTANCE=prod" "$RLS_PROD"
check_contains "make rls-check (default) names dev" "ORCHICON_INSTANCE=dev" "$(make_n rls-check)"

# --- (iii) services is a prerequisite of `run` ALONE ---------------------
echo
echo "  -- services is a run-only prerequisite (test isolation unbroken) --"

RUN="$(make_n run)"
check_contains "make run starts the instance's local services" "services up" "$RUN"
check_contains "make run points the plane at the derived DSN" "ORCHICON_POSTGRES_DSN=" "$RUN"
check_contains "make run points the plane at the instance's NATS port" "ORCHICON_NATS_URL=" "$RUN"
# THE PLANE'S OWN BIND IS PER-INSTANCE TOO, and a wildcard would be a collision
# AND a LAN exposure: the config default is `:8080` (dev's port on every
# interface), so `instance=prod` would take dev's 8080. Exported from the same table.
RUN_PROD="$(make_n run instance=prod)"
check_contains "make run instance=prod binds prod's control port" "services-port prod control" "$RUN_PROD"
check_contains "make run instance=prod binds loopback, not a wildcard" "127.0.0.1:" "$RUN_PROD"
check_lacks    "make run instance=prod does not leave ORCHICON_HTTP_ADDR at dev's wildcard" "ORCHICON_HTTP_ADDR=:8080" "$RUN_PROD"

for t in migrate "rls-check" test; do
  check_lacks "make $t does NOT start host services" "services up" "$(make_n "$t")"
done

TEST="$(make_n test)"
check_contains "make test still sets ORCHICON_CONFIG_DIR" "ORCHICON_CONFIG_DIR=" "$TEST"

# THE `instance ?= dev` DEFAULT MUST NOT SILENCE THE TARGETS THAT REQUIRE A CHOICE.
# `container-rebuild`/`full-rebuild` are DESTRUCTIVE and deliberately refuse to run
# without an explicit instance; a `?=` default makes their `test -n "$(instance)"`
# see "dev" and pass, so `make full-rebuild` with no argument silently rebuilt dev
# instead of printing its usage line. They must test the operator's CHOICE, not the
# default — and a real one must still get through.
# `make -n` EXPANDS $(instance_given), so the assertion is on the EXPANDED signal,
# not the variable name: the guard must read `test -n ""` (empty => refuse) with no
# argument, and a non-empty value when the operator names one.
check_contains "Makefile defines the instance_given choice test" 'instance_given = $(filter' "$(cat "$MAKEFILE")"
for t in container-rebuild full-rebuild; do
  check_contains "make $t refuses WITHOUT an explicit instance" 'test -n ""' "$(make_n "$t" 2>&1 | grep -m1 'test -n' || true)"
  check_contains "make $t instance=prod still proceeds" 'test -n "command line"' "$(make_n "$t" instance=prod 2>&1 | grep -m1 'test -n' || true)"
done

# The list spans two recipe lines (a trailing backslash), so both are joined.
UNSET_LINE="$(printf '%s\n' "$TEST" | grep -A1 -m1 'for v in ORCHICON_GUARD_POLICY' | tr '\n' ' ' || true)"
check_contains "make test still unsets the ambient config/plane vars" "ORCHICON_SERVE_STATE_DIR; do unset" "$UNSET_LINE"
check_lacks    "ORCHICON_TEST_DSN is not unset (opt-in stays reachable)" "ORCHICON_TEST_DSN" "$UNSET_LINE"
check_lacks    "ORCHICON_SERVICE_RESIDENCY is not unset (inner loop stays reachable)" "ORCHICON_SERVICE_RESIDENCY" "$UNSET_LINE"
check_contains "ORCHICON_TEST_DSN is named as reachable in the recipe" "ORCHICON_TEST_DSN" "$TEST"

# --- (iv) make help documents the new targets ----------------------------
echo
echo "  -- make help lists the local-services targets --"

HELP="$(make_do help)"
check_contains "make help lists 'services'"        "services "        "$HELP"
check_contains "make help lists 'services-down'"   "services-down "   "$HELP"
check_contains "make help lists 'services-status'" "services-status " "$HELP"
check_contains "make help documents instance= for migrate" "instance=<dev|prod|test>" "$HELP"

# --- (v) vacuity guards: the things under test EXIST ---------------------
echo
echo "  -- vacuity guards (a rename must fail this harness, not pass it) --"

for fn in services_port db_url pg_psql; do
  check_contains "container.sh defines ${fn}()" "$(printf '%s() {' "$fn")" "$(cat "$CONTAINER_SH")"
done

MAIN_GO="$(cat "${REPO_ROOT}/cmd/orchicon/main.go")"
check_contains "main.go dispatches a 'services' case"        'case "services":' "$MAIN_GO"
check_contains 'main.go documents services in printHelp' 'bin + " services"' "$MAIN_GO"

SERVICES_GO="$(cat "${REPO_ROOT}/cmd/orchicon/services.go" 2>/dev/null || true)"
check_contains "cmd/orchicon/services.go defines runServices" "func runServices(" "$SERVICES_GO"

echo
echo "  local-services harness: ${PASSED} passed, ${FAILED} failed, ${SKIPPED} skipped"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
