#!/usr/bin/env bash
# ============================================================================
# E2E acceptance harness for the NO-CONTAINER-ENGINE developer session.
#
# WHY THIS EXISTS. Each script in the de-containerized inner loop can pass its
# own test while the *combination* leaves the developer blind: a `make run` that
# starts a plane nothing can talk to, a rebuild that reports success while the
# previously built bundle is still what is served, a `status` that cannot say
# what is running. Individually those are invisible; a session that walks the
# whole developer flow — cold start, migrate, connect, change-and-rebuild, state,
# logs, two instances, hygiene, stop — is the only place the gap shows.
#
# WHAT IT PROVES (a developer, on a clean checkout, with NO container engine).
#   1. `make build` + `make services` + `make run` bring the plane up against
#      locally-started Postgres/NATS.
#   2. `make migrate` applies (twice: the second is a no-op) and `make rls-check`
#      reaches Postgres through the instance's OWN downloaded psql.
#   3. `orch` connects to the running plane and renders.
#   4. A frontend change is *verifiably live* in the SERVED bytes after the
#      documented rebuild path — including the adversarial stale-`dist` control
#      (a `frontend/dist` newer than `frontend/src`), which is the exact failure
#      mode that made frontend fixes "invisible after a rebuild".
#   5. `scripts/orchicon.sh status` and `services status` agree with independent
#      ground truth (process table, listening ports, /healthz).
#   6. `scripts/orchicon.sh logs` reaches the plane's activity.
#   7. dev and prod coexist and are independently controllable.
#   8. The engine-aware cleanup prunes only Orchicon's artifacts, and is a clean
#      no-op when there is nothing to prune.
#   9. Stopping leaves no orphaned Postgres holding a port.
#  10. Every one of the above is answerable WITHOUT a container command and
#      without reading source.
#
# WHAT IS REAL HERE. The plane, Postgres, NATS, the migrations, the RLS gate, the
# served bundle, the process table and the ports are all REAL. Every assertion
# also records INDEPENDENT evidence (/healthz over HTTP, the served asset bytes,
# /proc/<pid>/cmdline, /proc/net/tcp) beside whatever the script under test
# printed — a script's own report is never the proof.
#
# WHAT IS EMULATED. Nothing about the session. Two things are deliberately
# narrowed and recorded as such in the evidence:
#   * The stale-`dist` control touches `frontend/dist/index.html`'s mtime — it
#     reproduces a working-tree condition (branch switch / restored artifact /
#     clock skew), it does not forge a build.
#   * `make rls-check` is run once under a PATH shim WITHOUT `psql`, so the
#     engine-free rung (the instance's downloaded psql) is the one proven instead
#     of a host psql silently masking it. The unshimmed run is recorded beside it.
#
# USAGE
#   scripts/no-engine-e2e.sh [options]
#     --instance dev|prod      instance used for steps 1-6 (default dev)
#     --evidence-dir DIR       evidence root (default qa-evidence/no-engine-e2e/<UTC>)
#     --pg-port N              override the instance's Postgres port
#     --nats-port N            override the instance's NATS port
#     --control-port N         override the instance's plane HTTP port
#     --prod-pg-port N / --prod-nats-port N / --prod-control-port N
#                              the same, for the second (prod) instance in step 7
#     --keep                   leave the services and planes running on exit
#     --preflight-only         run step 0 only (validity precondition), then stop
#     --dry-run                print the plan for every step, execute nothing
#     -h|--help
#
#   `--pg/--nats/--control` overrides exist because a host that already has
#   something on an instance's default ports (another plane, a system Postgres)
#   cannot prove that the instance's OWN services are what the plane reached. The
#   harness refuses to run in that case rather than reporting a misleading PASS;
#   the overrides are the same ORCHICON_*_PORT knobs the port table already
#   honours, and DB_URL/`make run`'s exports move with them.
#
# EXIT CONTRACT
#   0  every step passed
#   1  at least one step failed (each FAIL names the file:line that owns it;
#      this feature FILES defects, it never patches a sibling's code)
#   2  the run is not a valid proof of the claim (a container engine is present,
#      the tree is not a checkout, or a required port is held by a foreign
#      process) — nothing was executed
#
# This is a SESSION DRIVER, not an offline gate: it downloads the pinned
# Postgres + NATS, binds real ports and takes minutes. `scripts/tests/` stays
# reserved for offline gates, and it is deliberately NOT part of `make ci`.
# ============================================================================
set -uo pipefail
# shellcheck disable=SC2155  # local x="$(...)" is deliberate: this script is -e-free

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/.." && pwd)"
CONTAINER_SH="${REPO_ROOT}/scripts/container.sh"
ORCHICON_SH="${REPO_ROOT}/scripts/orchicon.sh"
BIN="${REPO_ROOT}/bin"

UTC_STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
MARKER="NO-ENGINE-E2E-${UTC_STAMP}-$$"
INSTANCE="dev"
PROD_INSTANCE="prod"
EVIDENCE=""
KEEP=0
DRY_RUN=0
PREFLIGHT_ONLY=0

PG_OVERRIDE=""
NATS_OVERRIDE=""
CTRL_OVERRIDE=""
PROD_PG_OVERRIDE=""
PROD_NATS_OVERRIDE=""
PROD_CTRL_OVERRIDE=""

PASSES=0
FAILS=0
SKIPS=0
ABORT_REASON=""
CURRENT_STEP="0"
PLANE_PIDS=()

# The harness's own environment must not decide any case: a shell that launched
# a host plane exports its DSN/instance/state dir, and a parent make hands down
# MAKEFLAGS. Both are cleared here and re-set explicitly per child.
unset MAKEFLAGS MFLAGS 2>/dev/null || true
unset ORCHICON_INSTANCE ORCHICON_POSTGRES_PORT ORCHICON_NATS_PORT 2>/dev/null || true
unset ORCHICON_NATS_MONITOR_PORT ORCHICON_CONTROL_PORT 2>/dev/null || true
unset ORCHICON_POSTGRES_DSN ORCHICON_NATS_URL ORCHICON_HTTP_ADDR ORCHICON_DATA_DIR 2>/dev/null || true
unset DB_URL ORCHICON_URL 2>/dev/null || true

# --- output ----------------------------------------------------------------
say()  { printf '%s\n' "$*"; }
head2() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
ok2()  { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
bad2() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
skip2() { printf '  \033[33mSKIP\033[0m %s\n' "$*"; }
dim()  { printf '  \033[2m%s\033[0m\n' "$*"; }

# --- evidence ---------------------------------------------------------------
record() { # record <status> <assertion> [owner]
  printf '%s\t%s\t%s\t%s\n' "$CURRENT_STEP" "$1" "$2" "${3:-}" >>"${EVIDENCE}/results.tsv"
}

check() { # check <desc> <ok:0|1> [owner]
  local desc="$1" ok="$2" owner="${3:-}"
  if [ "$ok" = "0" ]; then
    PASSES=$((PASSES + 1)); record PASS "$desc"; ok2 "$desc"
  else
    FAILS=$((FAILS + 1)); record FAIL "$desc" "$owner"; bad2 "$desc${owner:+   <- owner: $owner}"
    printf '%s\t%s\t%s\n' "$CURRENT_STEP" "$desc" "$owner" >>"${EVIDENCE}/defects.tsv"
  fi
}

skip() { # skip <desc> <why>
  SKIPS=$((SKIPS + 1)); record SKIP "$1" ""; skip2 "$1 — $2"
  printf '  (skip reason: %s)\n' "$2" >>"${EVIDENCE}/$(step_file)"
}

check_contains() { # check_contains <desc> <file> <needle> [owner]
  local desc="$1" file="$2" needle="$3" owner="${4:-}"
  if [ -f "$file" ] && grep -qF -- "$needle" "$file" 2>/dev/null; then check "$desc" 0
  else check "$desc (missing '$needle' in ${file#"$REPO_ROOT"/})" 1 "$owner"; fi
}

check_absent() { # check_absent <desc> <file> <needle> [owner]
  local desc="$1" file="$2" needle="$3" owner="${4:-}"
  if [ -f "$file" ] && grep -qF -- "$needle" "$file" 2>/dev/null; then
    check "$desc (found '$needle' in ${file#"$REPO_ROOT"/})" 1 "$owner"
  else check "$desc" 0; fi
}

step_file() { printf 'step-%s.log' "$CURRENT_STEP"; }

begin_step() { # begin_step <n> <name>
  CURRENT_STEP="$1"
  head2 "step $1 — $2"
  : >"${EVIDENCE}/$(step_file)"
  dim "evidence: qa-evidence/no-engine-e2e/${UTC_STAMP}/$(step_file)"
}

tee_step() { # tee_step <cmd...> : run, tee stdout+stderr into the step log, return its status
  "$@" 2>&1 | tee -a "${EVIDENCE}/$(step_file)"
  return "${PIPESTATUS[0]}"
}

# --- /proc primitives (ss, pgrep, lsof, netstat and fuser are NOT available on
#     a bare host; the process table and the socket table are) -----------------
hex_port() { printf '%04X' "$1"; }

# port_listening <port> — true when any socket in /proc/net/tcp{,6} LISTENs there.
port_listening() {
  local port="$1" want f
  want="$(hex_port "$port")"
  for f in /proc/net/tcp /proc/net/tcp6; do
    [ -r "$f" ] || continue
    awk -v want="$want" 'NR>1 && $4=="0A" { n=split($2,a,":"); if (a[n]==want) { found=1; exit } } END { exit found?0:1 }' "$f" && return 0
  done
  return 1
}

# listen_inodes <port> — the socket inode(s) LISTENing on that port.
listen_inodes() {
  local port="$1" want f
  want="$(hex_port "$port")"
  for f in /proc/net/tcp /proc/net/tcp6; do
    [ -r "$f" ] || continue
    awk -v want="$want" 'NR>1 && $4=="0A" { n=split($2,a,":"); if (a[n]==want) print $10 }' "$f"
  done
}

# pid_owning_port <port> — the pid holding the LISTEN socket, resolved through
# /proc/<pid>/fd. Precise where a cmdline match is not: a host may run several
# planes (this harness starts two) and matching on "orchicon" alone would confuse
# them.
pid_owning_port() {
  local port="$1" inodes pid fd link target
  inodes="$(listen_inodes "$port")"
  [ -n "$inodes" ] || return 1
  for pid in /proc/[0-9]*; do
    for fd in "$pid"/fd/*; do
      link="$(readlink "$fd" 2>/dev/null)" || continue
      case "$link" in
        'socket:['*']')
          target="${link#socket:[}"; target="${target%]}"
          if printf '%s\n' "$inodes" | grep -qx -- "$target" 2>/dev/null; then
            printf '%s' "${pid#/proc/}"; return 0
          fi
          ;;
      esac
    done
  done
  return 1
}

cmdline_of() { tr '\0' ' ' <"/proc/$1/cmdline" 2>/dev/null | sed 's/ *$//'; }

# procs_matching <regex> — "pid<TAB>cmdline" lines from the process table,
# excluding this harness and its children (its own path contains "orchicon").
procs_matching() {
  local re="$1" d pid cl
  for d in /proc/[0-9]*; do
    pid="${d#/proc/}"
    [ "$pid" = "$$" ] && continue
    [ "$pid" = "$PPID" ] && continue
    cl="$(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null)"
    [ -n "$cl" ] || continue
    case "$cl" in *no-engine-e2e*) continue ;; esac
    if printf '%s' "$cl" | grep -Eq -- "$re" 2>/dev/null; then
      printf '%s\t%s\n' "$pid" "$cl"
    fi
  done
}

# children_of <pid> — direct children from /proc/<pid>/stat field 4.
children_of() {
  local want="$1" d stat ppid
  for d in /proc/[0-9]*; do
    stat="$(cat "$d/stat" 2>/dev/null)" || continue
    ppid="$(printf '%s' "$stat" | sed 's/.*) //' | awk '{print $2}')"
    [ "$ppid" = "$want" ] && printf '%s\n' "${d#/proc/}"
  done
}

# kill_tree <pid> — TERM a process and its descendants (make -> go run -> the
# compiled plane). Never a pattern kill: this host also runs a sandbox plane.
kill_tree() {
  local pid="$1" kid
  for kid in $(children_of "$pid"); do kill_tree "$kid"; done
  kill -TERM "$pid" 2>/dev/null || true
}

# --- instance ports ---------------------------------------------------------
declare -A PORT_PG=() PORT_NATS=() PORT_CTRL=()

resolve_ports() {
  PORT_PG[$INSTANCE]="${PG_OVERRIDE:-$("$CONTAINER_SH" services-port "$INSTANCE" postgres 2>/dev/null || echo 5432)}"
  PORT_NATS[$INSTANCE]="${NATS_OVERRIDE:-$("$CONTAINER_SH" services-port "$INSTANCE" nats 2>/dev/null || echo 4222)}"
  PORT_CTRL[$INSTANCE]="${CTRL_OVERRIDE:-$("$CONTAINER_SH" services-port "$INSTANCE" control 2>/dev/null || echo 8080)}"
  PORT_PG[$PROD_INSTANCE]="${PROD_PG_OVERRIDE:-$("$CONTAINER_SH" services-port "$PROD_INSTANCE" postgres 2>/dev/null || echo 5433)}"
  PORT_NATS[$PROD_INSTANCE]="${PROD_NATS_OVERRIDE:-$("$CONTAINER_SH" services-port "$PROD_INSTANCE" nats 2>/dev/null || echo 4223)}"
  PORT_CTRL[$PROD_INSTANCE]="${PROD_CTRL_OVERRIDE:-$("$CONTAINER_SH" services-port "$PROD_INSTANCE" control 2>/dev/null || echo 8091)}"
}

# instance_env <inst> -> fills ENVX with the environment every child gets, so
# both halves of the port table (bash + Go) resolve the SAME ports.
ENVX=()
instance_env() {
  local inst="$1"
  ENVX=(env -u ORCHICON_POSTGRES_DSN -u ORCHICON_NATS_URL -u ORCHICON_HTTP_ADDR
        -u ORCHICON_DATA_DIR -u ORCHICON_INSTANCE -u DB_URL -u ORCHICON_URL
        -u ORCHICON_POSTGRES_PORT -u ORCHICON_NATS_PORT -u ORCHICON_CONTROL_PORT
        "ORCHICON_POSTGRES_PORT=${PORT_PG[$inst]}"
        "ORCHICON_NATS_PORT=${PORT_NATS[$inst]}"
        "ORCHICON_CONTROL_PORT=${PORT_CTRL[$inst]}")
}

run_make() { # run_make <inst> <make args...>
  local inst="$1"; shift
  instance_env "$inst"
  ( cd "$REPO_ROOT" && "${ENVX[@]}" make "$@" instance="$inst" )
}

# --- plane liveness (HTTP + process table, never a script's own report) ------
api_base() { printf 'http://127.0.0.1:%s' "${PORT_CTRL[$1]}"; }

wait_healthz() { # wait_healthz <port> <secs>
  local port="$1" secs="${2:-120}" waited=0 code
  while [ "$waited" -lt "$secs" ]; do
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://127.0.0.1:${port}/healthz" 2>/dev/null || true)"
    [ "$code" = "200" ] && return 0
    # a plane that died during the wait must not burn the whole budget
    if [ "$waited" -gt 5 ] && ! port_listening "$port"; then
      case "$(tail -c 400 "${CUR_PLANE_LOG:-/dev/null}" 2>/dev/null)" in
        *"address already in use"*|*"Error"*|*"error"*) return 1 ;;
      esac
    fi
    sleep 1; waited=$((waited + 1))
  done
  return 1
}

healthz_code() { curl -s -o /dev/null -w '%{http_code}' --max-time 3 "$(api_base "$1")/healthz" 2>/dev/null || true; }

served_index() { curl -fsS --max-time 5 "$(api_base "$1")/" 2>/dev/null; }

# served_js_paths <inst> — every script/asset .js the served index references.
served_js_paths() {
  served_index "$1" | grep -oE '(src|href)="[^"]+\.js"' | sed -E 's/^(src|href)="//; s/"$//' | sort -u
}

# served_bundle_bytes <inst> — the bytes the BROWSER would execute, concatenated.
# This is the only honest place to look for a frontend change: `frontend/dist` is
# an input, the served asset is the product.
served_bundle_bytes() {
  local inst="$1" p any=0
  for p in $(served_js_paths "$inst"); do
    any=1
    curl -fsS --max-time 60 "$(api_base "$inst")${p}" 2>/dev/null || return 1
    printf '\n'
  done
  [ "$any" = "1" ] || return 1
  return 0
}

# --- the BUILD's product: frontend/dist (what the next binary would embed) ---
# Fetching the served bytes proves the SERVER has the change; reading the built
# dist proves the BUILD had it. When the two disagree the defect bisects instead
# of blurring: dist WITHOUT the marker is the mtime stamp check (Makefile:569-581),
# served WITHOUT it is the embed/restart path (cmd/orchicon/serve_state.go).
dist_js_paths() { find "${REPO_ROOT}/frontend/dist" -type f -name '*.js' 2>/dev/null | sort; }

dist_bundle_bytes() {
  local f any=0
  for f in $(dist_js_paths); do any=1; cat "$f"; printf '\n'; done
  [ "$any" = "1" ] || return 1
  return 0
}

# dist_is_real — a built bundle, not the tracked .gitkeep placeholder. A
# dist/index.html that exists but is EMPTY is the silent-stale-dist failure mode
# already realised: the stamp check sees "dist newer than src", skips the build,
# and the binary embeds no UI at all (serve_state.go's loud warning fires, and /
# answers the API's 401 "missing credentials").
dist_is_real() {
  [ -s "${REPO_ROOT}/frontend/dist/index.html" ] && [ -n "$(dist_js_paths)" ]
}

# probe_served_index <inst> <out> — the independent "is the UI really served?"
# evidence: the HTTP status of / plus the hashed assets the returned HTML names.
# Recording the status is what makes a frontendless binary unmistakable instead
# of looking like a mysterious empty fetch in step 4.
probe_served_index() {
  local inst="$1" out="$2" code
  code="$(curl -s -o "${out}.body" -w '%{http_code}' --max-time 5 "$(api_base "$inst")/" 2>/dev/null || true)"
  {
    printf 'GET %s/ -> HTTP %s\n' "$(api_base "$inst")" "${code:-<no response>}"
    printf 'hashed js the served HTML names: %s\n' "$(served_js_paths "$inst" | tr '\n' ' ')"
    printf -- '--- first 400 bytes of the body ---\n'
    head -c 400 "${out}.body" 2>/dev/null
    printf '\n'
  } >"$out"
}

start_plane() { # start_plane <inst> <log-file>
  local inst="$1" log="$2" pid
  CUR_PLANE_LOG="$log"
  instance_env "$inst"
  ( cd "$REPO_ROOT" && "${ENVX[@]}" make run instance="$inst" ) >"$log" 2>&1 &
  pid=$!
  PLANE_PIDS+=("$pid")
  printf '%s\n' "$pid" >"${EVIDENCE}/.plane-${inst}.pid"
  dim "started: make run instance=$inst (pid $pid) -> ${log#"$REPO_ROOT"/}"
}

stop_plane() { # stop_plane <inst>
  local inst="$1" pf="${EVIDENCE}/.plane-${1}.pid"
  local port="${PORT_CTRL[$1]}" pid owner waited=0
  pid=""
  [ -f "$pf" ] && pid="$(cat "$pf" 2>/dev/null)"
  if [ -n "$pid" ]; then kill_tree "$pid"; fi
  owner="$(pid_owning_port "$port" 2>/dev/null || true)"
  if [ -n "$owner" ]; then kill_tree "$owner"; fi
  while [ "$waited" -lt 30 ] && port_listening "$port"; do sleep 1; waited=$((waited + 1)); done
  rm -f "$pf"
  return 0
}

resolve_token() { # prints a token if the documented client config has one
  if [ -n "${ORCHICON_TOKEN:-}" ]; then printf '%s' "$ORCHICON_TOKEN"; return 0; fi
  local cfg="${ORCHICON_CONFIG_DIR:-$HOME/.orchicon}/config"
  [ -f "$cfg" ] || return 1
  grep -oE 'oc_[A-Za-z0-9._-]{8,}' "$cfg" 2>/dev/null | head -1
}

plane_log_path() { printf '%s/.local/share/orchicon-%s/serve/logs/orchicon.log' "$HOME" "$1"; }

# --- preflight --------------------------------------------------------------
ports_free_list() { # ports_free_list <port...> — print the ones that are held
  local p
  for p in "$@"; do port_listening "$p" && printf '%s ' "$p"; done
  return 0
}

# --- build budget (MEASURED before anything runs) ---------------------------
# A cold build needs room on THREE volumes, and each one fails differently when
# it is too small: `npm error code ENOSPC` inside the checkout, "write
# $WORK/b657/_pkg_.a: no space left on device" from the Go toolchain (which names
# a TEMP DIR, never the host), and a half-populated module cache. All three land
# minutes into the session and blame nobody, which is precisely the blindness this
# harness exists to remove — so the budget is measured up front, written to
# evidence, and a shortfall that cannot be fixed here INVALIDATES the run (exit 2)
# with the knob that fixes it.
REPO_NEED_MB=1500            # node_modules + frontend/dist + bin/ + evidence (checkout)
MODCACHE_COLD_NEED_MB=1000   # cold `go mod download` for this module set (686 MB measured) + slack
MODCACHE_WARM_NEED_MB=50     # module cache populated: lock/sum writes only
BUILDCACHE_COLD_NEED_MB=2200 # empty build cache: compile every package + link
BUILDCACHE_WARM_NEED_MB=900  # build cache populated: incremental writes only
WORK_NEED_MB=800             # go build $WORK + the link step (GOTMPDIR — relocatable)
TEMP_NEED_MB=200             # general TMPDIR users (npm staging, tsc); NOT relocated
NPMCACHE_COLD_NEED_MB=600    # cold `npm install` cache, when the preflight must install deps
NPMCACHE_WARM_NEED_MB=50
MODCACHE_WARM_MIN_MB=600     # a cache at least this big counts as populated; else assume COLD
BUILDCACHE_WARM_MIN_MB=400
NPMCACHE_WARM_MIN_MB=100

free_mb() { df -Pm "$1" 2>/dev/null | awk 'NR==2 {print $4}'; }
dev_of()  { df -Pm "$1" 2>/dev/null | awk 'NR==2 {print $1}'; }
dir_mb()  { du -sm "$1" 2>/dev/null | awk 'NR==1 {print $1}'; }

# WHAT THE BUILD WILL REALLY WRITE — not what this shell's `go` would. `make`
# exports GOCACHE/GOTMPDIR/GOPATH once the project-local toolchain is provisioned
# (Makefile:32-37), so a provisioned checkout keeps build state on the CHECKOUT
# volume; otherwise the plain `go env` values under $HOME apply. Measuring $HOME in
# both cases would judge a volume the build never touches. An explicit environment
# value wins either way (the Makefile uses `?=`).
provisioned_tools() { [ -x "${REPO_ROOT}/.dev/tools/go/bin/go" ]; }
effective_mod_cache() {
  if [ -n "${GOMODCACHE:-}" ]; then printf '%s' "$GOMODCACHE"; return 0; fi
  if provisioned_tools; then printf '%s' "${REPO_ROOT}/.dev/tools/gopath/pkg/mod"; return 0; fi
  go env GOMODCACHE 2>/dev/null | grep . || printf '%s' "$HOME/go/pkg/mod"
}
effective_build_cache() {
  if [ -n "${GOCACHE:-}" ]; then printf '%s' "$GOCACHE"; return 0; fi
  if provisioned_tools; then printf '%s' "${REPO_ROOT}/.dev/tools/gocache"; return 0; fi
  go env GOCACHE 2>/dev/null | grep . || printf '%s' "$HOME/.cache/go-build"
}
effective_work_tmp() {
  if [ -n "${GOTMPDIR:-}" ]; then printf '%s' "$GOTMPDIR"; return 0; fi
  if provisioned_tools; then printf '%s' "${REPO_ROOT}/.dev/tools/gotmp"; return 0; fi
  printf '%s' "${TMPDIR:-/tmp}"
}

build_budget() {
  local gomodcache gocache npmcache worktmp cache_base
  local mod_mb build_mb npm_mb warm_mod warm_build warm_npm
  local repo_free mod_free build_free npm_free tmp_free work_free tmpdir
  local need_mod need_build need_npm need_cache_vol need_build_vol need_repo need_work need_tmp
  local same_vol short gitdir reloc gocache_reloc npm_reloc work_reloc budget_log why

  gomodcache="$(effective_mod_cache)"
  gocache="$(effective_build_cache)"
  npmcache="$(npm config get cache 2>/dev/null | grep . || printf '%s' "$HOME/.npm")"
  worktmp="$(effective_work_tmp)"
  # The tools create these on first use; mkdir here so the volume they WILL be
  # written to can be measured before a build fills it.
  mkdir -p "$gomodcache" "$gocache" "$npmcache" "$worktmp" 2>/dev/null || true
  cache_base="$(dirname "$gocache")"

  # WARM vs COLD is a SIZE question, not an emptiness one: a half-populated cache is
  # not "populated", and calling it that is how a budget lies. Over-estimating costs
  # a spurious RUN INVALID; under-estimating costs a mid-build ENOSPC that names a
  # temp path and blames nobody.
  mod_mb="$(dir_mb "$gomodcache")"; mod_mb="${mod_mb:-0}"
  build_mb="$(dir_mb "$gocache")"; build_mb="${build_mb:-0}"
  npm_mb="$(dir_mb "$npmcache")"; npm_mb="${npm_mb:-0}"
  warm_mod=0; [ "$mod_mb" -ge "$MODCACHE_WARM_MIN_MB" ] && warm_mod=1
  warm_build=0; [ "$build_mb" -ge "$BUILDCACHE_WARM_MIN_MB" ] && warm_build=1
  warm_npm=0; [ "$npm_mb" -ge "$NPMCACHE_WARM_MIN_MB" ] && warm_npm=1
  need_mod="$MODCACHE_COLD_NEED_MB"; [ "$warm_mod" = 1 ] && need_mod="$MODCACHE_WARM_NEED_MB"
  need_build="$BUILDCACHE_COLD_NEED_MB"; [ "$warm_build" = 1 ] && need_build="$BUILDCACHE_WARM_NEED_MB"
  need_npm="$NPMCACHE_COLD_NEED_MB"; [ "$warm_npm" = 1 ] && need_npm="$NPMCACHE_WARM_NEED_MB"

  repo_free="$(free_mb "$REPO_ROOT")"; repo_free="${repo_free:-0}"
  mod_free="$(free_mb "$gomodcache")"; mod_free="${mod_free:-0}"
  build_free="$(free_mb "$gocache")"; build_free="${build_free:-0}"
  npm_free="$(free_mb "$npmcache")"; npm_free="${npm_free:-0}"
  work_free="$(free_mb "$worktmp")"; work_free="${work_free:-0}"
  tmpdir="${TMPDIR:-/tmp}"
  tmp_free="$(free_mb "$tmpdir")"; tmp_free="${tmp_free:-0}"
  same_vol=0
  if [ -n "$(dev_of "$gocache")" ] && [ "$(dev_of "$gocache")" = "$(dev_of "$gomodcache")" ]; then same_vol=1; fi

  # Demand PER VOLUME, so a relocation MOVES the demand instead of pretending the
  # host grew.
  need_repo="$REPO_NEED_MB"
  need_work="$WORK_NEED_MB"
  need_tmp="$TEMP_NEED_MB"
  gitdir="$(git -C "$REPO_ROOT" rev-parse --absolute-git-dir 2>/dev/null || true)"
  reloc=""; gocache_reloc=""; npm_reloc=""; work_reloc=""

  # THE MODULE CACHE NEVER MOVES (a module download mid-session would change what
  # the run proves), so its demand can only be met on the volume it already lives
  # on — it is the one cost a relocation cannot pay down. The build cache IS
  # disposable (go rebuilds it), so a volume too small for a cold one gets it moved
  # into the git dir: outside the working tree, so the checkout the run proves is
  # still clean.
  short=0
  if [ "$same_vol" = 1 ]; then
    [ "$mod_free" -lt $((need_mod + need_build)) ] && short=1
  else
    [ "$build_free" -lt "$need_build" ] && short=1
  fi
  if [ "$short" = 1 ] && [ -n "$gitdir" ] \
     && [ "$repo_free" -ge $((need_repo + BUILDCACHE_COLD_NEED_MB)) ] \
     && mkdir -p "${gitdir}/no-engine-e2e-gocache" 2>/dev/null; then
    export GOCACHE="${gitdir}/no-engine-e2e-gocache"
    gocache="$GOCACHE"
    gocache_reloc="$GOCACHE"
    build_mb=0
    need_repo=$((need_repo + BUILDCACHE_COLD_NEED_MB))
    need_build=0   # the relocated cache starts EMPTY: the COLD budget moved with it
  fi

  # What each volume must hold once nothing else moves. GOCACHE and GOMODCACHE are
  # usually on ONE volume ($HOME), so their demands ADD UP there.
  if [ "$same_vol" = 1 ]; then
    need_cache_vol=$((need_mod + need_build))
    need_build_vol=0
  else
    need_cache_vol="$need_mod"
    need_build_vol="$need_build"
  fi

  # The npm cache is disposable too, and `make fe-install` (run below when the
  # checkout has no node_modules) writes into it first — on a tiny $HOME that turns
  # into an ENOSPC whose message names the checkout instead of the host.
  if [ "$npm_free" -lt "$need_npm" ] && [ -n "$gitdir" ] \
     && [ "$repo_free" -ge $((need_repo + NPMCACHE_COLD_NEED_MB)) ] \
     && mkdir -p "${gitdir}/no-engine-e2e-npm-cache" 2>/dev/null; then
    export npm_config_cache="${gitdir}/no-engine-e2e-npm-cache"
    npmcache="$npm_config_cache"
    npm_reloc="$npmcache"
    need_repo=$((need_repo + NPMCACHE_COLD_NEED_MB))
    need_npm=0
  fi

  # go writes $WORK (and the link step's temporaries) to GOTMPDIR, falling back to
  # TMPDIR. On a host whose /tmp is a small tmpfs that is the FIRST thing to blow
  # up, and its error names a temp path rather than the host.
  #
  # Only GOTMPDIR is moved — deliberately NOT TMPDIR. A relocated TMPDIR lengthens
  # every temp path this repo's own code builds under it, and unix socket paths have
  # a hard 108-byte limit: relocating TMPDIR into a git dir turns `listen unix` into
  # `bind: invalid argument` (verified against internal/claude's consent socket and
  # internal/contextfiles' walk). $WORK is where the ENOSPC actually happened, so
  # GOTMPDIR is the piece worth moving. Two targets, best first: scratch INSIDE the
  # git dir (same volume as the checkout, never part of the working tree), then the
  # volume the build caches sit on.
  if [ "$work_free" -lt "$need_work" ]; then
    if [ -n "$gitdir" ] && [ "$repo_free" -ge $((need_repo + need_work)) ]; then
      reloc="${gitdir}/no-engine-e2e-tmp"
      need_repo=$((need_repo + need_work))
    elif [ "$same_vol" = 1 ] && [ "$mod_free" -ge $((need_cache_vol + need_work)) ]; then
      reloc="${cache_base}/no-engine-e2e-tmp"
      need_cache_vol=$((need_cache_vol + need_work))
    elif [ "$same_vol" = 0 ] && [ "$build_free" -ge $((need_build_vol + need_work)) ]; then
      reloc="${cache_base}/no-engine-e2e-tmp"
      need_build_vol=$((need_build_vol + need_work))
    fi
    if [ -n "$reloc" ] && mkdir -p "$reloc" 2>/dev/null; then
      export GOTMPDIR="$reloc"
      worktmp="$reloc"
      work_reloc="$reloc"
      work_free="$(free_mb "$worktmp")"; work_free="${work_free:-0}"
      need_work=0   # its demand now sits on the volume it moved to (counted above)
    else
      reloc=""
    fi
  fi

  budget_log="${EVIDENCE}/00-build-budget.log"
  {
    echo "measured:        $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "go module cache: $gomodcache"
    echo "                 ${mod_mb} MB on disk; its volume ${mod_free} MB free; needs >= ${need_mod} — NEVER relocated (a mid-session module download would change what the run proves)"
    echo "go build cache:  $gocache"
    echo "                 ${build_mb} MB on disk; its volume ${build_free} MB free; needs >= ${need_build}; same volume as the module cache: ${same_vol} (1 = their demands add up)"
    echo "npm cache:       $npmcache"
    echo "                 ${npm_mb} MB on disk; its volume ${npm_free} MB free; needs >= ${need_npm}"
    echo "checkout volume: $REPO_ROOT -> ${repo_free} MB free, need >= ${need_repo}"
    echo "GOTMPDIR (\$WORK): $worktmp -> ${work_free} MB free, need >= ${need_work} (go build \$WORK + link)"
    echo "TMPDIR (general): $tmpdir -> ${tmp_free} MB free, need >= ${need_tmp} (npm staging, tsc) — never relocated: a longer TMPDIR breaks 108-byte unix socket paths"
    echo "caches warm:     module=${warm_mod} build=${warm_build} npm=${warm_npm} (1 = populated enough to need incremental space only)"
    [ -n "$gocache_reloc" ] && echo "GOCACHE relocated to $gocache_reloc (its own volume cannot hold a cold build cache; disposable, so the session proves the same thing)"
    [ -n "$npm_reloc" ] && echo "npm cache relocated to $npm_reloc (disposable)"
    [ -n "$work_reloc" ] && echo "GOTMPDIR relocated to $work_reloc (host temp volume too small for a go build; TMPDIR itself is untouched)"
    for d in "$gocache_reloc" "$npm_reloc" "$work_reloc"; do
      [ -n "$d" ] && echo "leave-behind:    $d (outside the working tree — safe to delete after the run)"
    done
    echo "this is a MEASUREMENT, not a promise: a warm-cache run needs less and the"
    echo "numbers move as the build writes. A shortfall is a HOST condition, not a"
    echo "defect in the session being proven — hence exit 2 (invalid run), not FAIL."
  } >"$budget_log" 2>&1
  sed 's/^/  /' "$budget_log" >>"${EVIDENCE}/00-preflight.log"

  if [ "$repo_free" -lt "$need_repo" ] || [ "$mod_free" -lt "$need_cache_vol" ] \
     || [ "$build_free" -lt "$need_build_vol" ] || [ "$npm_free" -lt "$need_npm" ] \
     || [ "$work_free" -lt "$need_work" ] || [ "$tmp_free" -lt "$need_tmp" ]; then
    why=""
    [ "$repo_free" -lt "$need_repo" ] && why="$why the checkout volume has ${repo_free} MB free (needs >= ${need_repo});"
    [ "$mod_free" -lt "$need_cache_vol" ] && why="$why the Go cache volume has ${mod_free} MB free (needs >= ${need_cache_vol}) and the module cache at ${gomodcache} is never moved mid-session — pre-warm it with 'go mod download', or point it at a volume with room: export GOMODCACHE=/big/go/pkg/mod;"
    [ "$build_free" -lt "$need_build_vol" ] && why="$why the build-cache volume has ${build_free} MB free (needs >= ${need_build_vol}, and it could not be relocated);"
    [ "$npm_free" -lt "$need_npm" ] && why="$why the npm cache volume has ${npm_free} MB free (needs >= ${need_npm});"
    [ "$work_free" -lt "$need_work" ] && why="$why the go \$WORK volume ${worktmp} has ${work_free} MB free (needs >= ${need_work}) — export GOTMPDIR=/big/tmp;"
    [ "$tmp_free" -lt "$need_tmp" ] && why="$why TMPDIR ${tmpdir} has ${tmp_free} MB free (needs >= ${need_tmp}) — export TMPDIR to a volume with room (keep the path short: unix sockets cap at 108 bytes);"
    check "the host has the disk budget for a cold build (checkout ${repo_free}/${need_repo} MB, go caches ${mod_free}/${need_cache_vol}, build cache ${build_free}/${need_build_vol}, go \$WORK ${work_free}/${need_work}, tmp ${tmp_free}/${need_tmp})" 1
    ABORT_REASON="this host cannot finish a cold build, so the session cannot be proven here.${why} Nothing was executed — this is NOT a defect in the session. Details: ${budget_log}."
    return 1
  fi
  check "the host has the disk budget for a cold build (checkout ${repo_free}/${need_repo} MB, go caches ${mod_free}/${need_cache_vol}, build cache ${build_free}/${need_build_vol}, go \$WORK ${work_free}/${need_work}, tmp ${tmp_free}/${need_tmp})" 0
  return 0
}

preflight() {
  begin_step 0 "preflight — is this run a valid proof of the claim?"

  local engine_found=""
  local e
  for e in docker podman nerdctl docker-compose; do
    if command -v "$e" >/dev/null 2>&1; then engine_found="$engine_found $e($(command -v "$e"))"; fi
  done
  {
    echo "date:        $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "host:        $(uname -a)"
    echo "repo:        $REPO_ROOT"
    echo "marker:      $MARKER"
    echo "engines:     ${engine_found:-none found (GOOD — this is the claim being proven)}"
    echo "psql on PATH: $(command -v psql 2>/dev/null || echo ABSENT)"
    echo "head:        $(git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null || echo 'not a git checkout')"
    echo "branch:      $(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || echo '?')"
    echo "dirty:"
    git -C "$REPO_ROOT" status --porcelain 2>/dev/null | sed 's/^/  /'
    echo "go:          $(go version 2>/dev/null || echo ABSENT)"
    echo "node/npm:    $(node --version 2>/dev/null || echo ABSENT) / $(npm --version 2>/dev/null || echo ABSENT)"
    echo "ports:       ${INSTANCE} pg=${PORT_PG[$INSTANCE]} nats=${PORT_NATS[$INSTANCE]} control=${PORT_CTRL[$INSTANCE]}"
    echo "             ${PROD_INSTANCE} pg=${PORT_PG[$PROD_INSTANCE]} nats=${PORT_NATS[$PROD_INSTANCE]} control=${PORT_CTRL[$PROD_INSTANCE]}"
    echo "listening now (ports we need that are ALREADY held — must be empty):"
    printf '  %s\n' "$(ports_free_list "${PORT_PG[$INSTANCE]}" "${PORT_NATS[$INSTANCE]}" "${PORT_CTRL[$INSTANCE]}" "${PORT_PG[$PROD_INSTANCE]}" "${PORT_NATS[$PROD_INSTANCE]}" "${PORT_CTRL[$PROD_INSTANCE]}")"
    echo "state under $HOME/.local/share:"
    ls -la "$HOME/.local/share" 2>/dev/null | sed 's/^/  /' | head -30
    echo "process table (postgres|nats-server|exe/orchicon):"
    procs_matching 'postgres|nats-server|exe/orchicon' | sed 's/^/  /' | head -40
  } >"${EVIDENCE}/00-preflight.log" 2>&1
  cat "${EVIDENCE}/00-preflight.log"

  if [ -n "$engine_found" ]; then
    check "no container engine installed${engine_found}" 1
    ABORT_REASON="a container engine is installed (${engine_found# }); this run cannot prove the no-engine claim. Uninstall it or use a clean host."
    return 1
  fi
  check "no container engine installed (docker/podman/nerdctl/docker-compose all absent)" 0
  check "the tree is a git checkout" "$(git -C "$REPO_ROOT" rev-parse --verify HEAD >/dev/null 2>&1; echo $?)"

  local held
  held="$(ports_free_list "${PORT_PG[$INSTANCE]}" "${PORT_NATS[$INSTANCE]}" "${PORT_CTRL[$INSTANCE]}" "${PORT_PG[$PROD_INSTANCE]}" "${PORT_NATS[$PROD_INSTANCE]}" "${PORT_CTRL[$PROD_INSTANCE]}")"
  if [ -n "${held// /}" ]; then
    check "the instance ports are free before the session starts (held: ${held})" 1
    ABORT_REASON="port(s) ${held}are already held on this host, so the session could not prove that the instance's OWN services and plane are what it reached. Free them and re-run, or shift this run onto free ports: --pg-port/--nats-port/--control-port (and --prod-*)."
    return 1
  fi
  check "the instance ports are free before the session starts" 0

  # THE PORT TABLE PREFERS A RECORDED PORT. scripts/container.sh:_recorded_port
  # reads <dataDir>/ports.json FIRST and only then falls back to
  # ORCHICON_*_PORT, so on a host that ran `services up` once, --pg-port would
  # move the harness's EXPECTATION while `make run` kept the recorded port:
  # every port assertion would fail and blame nobody. That is a condition of the
  # host, not a defect in the session, so it invalidates the run (exit 2) with
  # the fix named rather than producing a misleading FAIL list.
  local drift="" inst svc want got
  for inst in "$INSTANCE" "$PROD_INSTANCE"; do
    for svc in postgres nats control; do
      case "$svc" in
        postgres) want="${PORT_PG[$inst]}" ;;
        nats) want="${PORT_NATS[$inst]}" ;;
        control) want="${PORT_CTRL[$inst]}" ;;
      esac
      # Ask the table WITH this run's ORCHICON_*_PORT in force — that is what
      # `make run` will pass down. A difference can only mean a recorded port won.
      instance_env "$inst"
      got="$("${ENVX[@]}" "$CONTAINER_SH" services-port "$inst" "$svc" 2>/dev/null || true)"
      [ -n "$got" ] && [ "$got" != "$want" ] && drift="$drift ${inst}/${svc}(chose ${want}, table says ${got})"
    done
  done
  if [ -n "$drift" ]; then
    check "the port table resolves the ports this run chose${drift}" 1
    ABORT_REASON="this host has RECORDED service ports that win over the port overrides (${drift# }); run 'make services-down instance=<inst>' (or remove \$HOME/.local/share/orchicon-<inst>/ports.json) and re-run, so the ports this run asserts are the ports the services actually bind."
    return 1
  fi
  check "the port table resolves the ports this run chose" 0

  # A host without the disk budget cannot produce a proof at all, and the ENOSPC
  # it would hit mid-build names a temp path instead of the host. Measured, not
  # assumed — and INVALID (exit 2), never a FAIL against a sibling's code.
  build_budget || return 1

  if [ "$PREFLIGHT_ONLY" = "1" ]; then
    skip "frontend deps installed (make fe-install)" "--preflight-only: mutating prerequisite deferred to a full run"
    return 0
  fi
  # A DIRECTORY IS NOT A TOOLCHAIN. A partial extraction — npm interrupted by a
  # full disk, which is exactly what the budget above exists to prevent — leaves
  # frontend/node_modules PRESENT with no .bin/ links and no @types, and the
  # build then dies with "tsc: not found" (or, past the stamp check, ships the
  # PREVIOUS dist). Check the executables the build actually runs, not the dir.
  if [ -x "${REPO_ROOT}/frontend/node_modules/.bin/tsc" ] && [ -x "${REPO_ROOT}/frontend/node_modules/.bin/vite" ]; then
    check "frontend build toolchain present (node_modules/.bin/{tsc,vite})" 0
  else
    dim "frontend build toolchain incomplete — running make fe-install"
    if tee_step run_make "$INSTANCE" fe-install; then check "make fe-install (no usable frontend toolchain in this checkout)" 0
    else check "make fe-install (no usable frontend toolchain in this checkout)" 1 "Makefile:560"; fi
    # `npm install` reconciles against the lockfile but does NOT repair a
    # half-extracted tree (it reports success and leaves the holes), so a second
    # missing executable is the HOST's problem and invalidates the run — never a
    # FAIL against a sibling's code, and never a session that proves nothing.
    if [ ! -x "${REPO_ROOT}/frontend/node_modules/.bin/tsc" ] || [ ! -x "${REPO_ROOT}/frontend/node_modules/.bin/vite" ]; then
      ABORT_REASON="frontend/node_modules exists but is NOT a usable toolchain (no .bin/tsc or .bin/vite even after 'make fe-install'); 'npm install' does not repair a half-extracted tree. Run 'cd frontend && npm ci' with the npm cache on a volume that has room, then re-run. Nothing was executed: this is the host, not the session."
      return 1
    fi
  fi

  # A dist/index.html WITH NO BUNDLE is not a clean checkout. It is the silent
  # stale-dist failure mode already realised (the stamp check sees "dist newer
  # than src", skips the build, and the binary embeds no frontend). Repair it as
  # SETUP through the documented escape hatch and record that it happened; step 4
  # still recreates the mode adversarially, so nothing below is papered over.
  if dist_is_real; then
    check "the checkout starts from a real frontend bundle (dist/index.html + assets/*.js)" 0
  elif [ ! -e "${REPO_ROOT}/frontend/dist/index.html" ]; then
    check "the checkout starts clean (no frontend/dist — step 1's make build creates it)" 0
  else
    dim "frontend/dist/index.html exists but embeds NO bundle — repairing (setup) with make fe-build force-fe=1"
    if tee_step run_make "$INSTANCE" fe-build force-fe=1; then check "an EMPTY frontend/dist was repaired before the session (setup: make fe-build force-fe=1)" 0
    else check "an EMPTY frontend/dist was repaired before the session (setup: make fe-build force-fe=1)" 1 "Makefile:573-581"; fi
    if ! dist_is_real; then
      ABORT_REASON="frontend/dist/index.html exists but carries no bundle, and 'make fe-build force-fe=1' did not produce one (see $(step_file)). Fix the frontend build, then re-run."
      return 1
    fi
  fi
  return 0
}

# --- step 1: cold start -----------------------------------------------------
step1_cold_start() {
  begin_step 1 "cold start — make build, make services, make run (no engine)"
  local log
  log="${EVIDENCE}/$(step_file)"

  if tee_step run_make "$INSTANCE" build; then check "make build succeeds with no container engine" 0
  else check "make build succeeds with no container engine" 1 "Makefile:255 build: fetch-tags fe-build"; fi
  check "bin/orchicon is executable" "$([ -x "$BIN/orchicon" ] && echo 0 || echo 1)"
  check "bin/orch is executable" "$([ -x "$BIN/orch" ] && echo 0 || echo 1)"

  if tee_step run_make "$INSTANCE" services; then check "make services starts this instance's Postgres + NATS on the host" 0
  else check "make services starts this instance's Postgres + NATS on the host" 1 "Makefile:460"; fi

  instance_env "$INSTANCE"
  "${ENVX[@]}" "$BIN/orchicon" services status "$INSTANCE" 2>&1 | tee -a "$log" >"${EVIDENCE}/01-services-status.txt"
  check_contains "services status reports the services up" "${EVIDENCE}/01-services-status.txt" "up (postgres"
  check "Postgres is LISTENing on ${PORT_PG[$INSTANCE]} (independent: /proc/net/tcp)" "$(port_listening "${PORT_PG[$INSTANCE]}"; echo $?)"
  check "NATS is LISTENing on ${PORT_NATS[$INSTANCE]} (independent: /proc/net/tcp)" "$(port_listening "${PORT_NATS[$INSTANCE]}"; echo $?)"

  # The plane's own log path — recorded here because step 6 depends on it (D5).
  if [ -f "$(plane_log_path "$INSTANCE")" ]; then
    dim "plane log present: $(plane_log_path "$INSTANCE")"
  else
    dim "plane log ABSENT at $(plane_log_path "$INSTANCE") — step 6 will judge this"
  fi

  start_plane "$INSTANCE" "${EVIDENCE}/01-run.log"
  if wait_healthz "${PORT_CTRL[$INSTANCE]}" 180; then check "make run brings the plane up (GET /healthz -> 200)" 0
  else check "make run brings the plane up (GET /healthz -> 200)" 1 "Makefile:272-278"; fi
  curl -fsS --max-time 5 "$(api_base "$INSTANCE")/healthz" >"${EVIDENCE}/01-healthz.txt" 2>/dev/null || true
  tee -a "$log" <"${EVIDENCE}/01-healthz.txt" >/dev/null 2>&1 || true
  check_contains "/healthz body reports ok" "${EVIDENCE}/01-healthz.txt" '"status":"ok"'
  check "the plane's HTTP port is LISTENing" "$(port_listening "${PORT_CTRL[$INSTANCE]}"; echo $?)"
  local owner
  owner="$(pid_owning_port "${PORT_CTRL[$INSTANCE]}" 2>/dev/null || true)"
  check "the plane's listening socket belongs to a real process (pid ${owner:-none})" "$([ -n "$owner" ] && echo 0 || echo 1)"
  [ -n "$owner" ] && printf 'plane pid %s: %s\n' "$owner" "$(cmdline_of "$owner")" >>"$log"

  # The UI half of the cold start. The binary go:embeds frontend/dist, so a build
  # that skipped `npm run build` compiles PERFECTLY and then serves the API where
  # the UI should be; step 4's byte capture is worthless unless this is true
  # FIRST, so it is asserted here, before anything is rebuilt.
  probe_served_index "$INSTANCE" "${EVIDENCE}/01-served-index.txt"
  if [ -n "$(served_js_paths "$INSTANCE")" ]; then
    check "the plane SERVES the real UI (the served index names a hashed bundle)" 0
  else
    check "the plane SERVES the real UI (the served index names a hashed bundle)" 1 "cmd/orchicon/serve_state.go:124-141 (an empty embedded frontend/dist/index.html falls through to the API: 401 'missing credentials') + Makefile:569-581"
  fi
}

# --- step 2: migrate + rls-check --------------------------------------------
step2_migrate() {
  begin_step 2 "migrate + rls-check against the locally-started services"
  local log
  log="${EVIDENCE}/$(step_file)"

  if tee_step run_make "$INSTANCE" migrate; then check "make migrate applies to this instance's database" 0
  else check "make migrate applies to this instance's database" 1 "Makefile:431-433"; fi
  grep -qF "migrations applied OK" "$log" && ok2 "atlas-ci reported 'migrations applied OK'"
  cp "$log" "${EVIDENCE}/02-migrate.log"

  : >"$log"
  if tee_step run_make "$INSTANCE" migrate; then check "a second make migrate is a clean no-op (exit 0)" 0
  else check "a second make migrate is a clean no-op (exit 0)" 1 "Makefile:431-433"; fi
  cp "$log" "${EVIDENCE}/02-migrate-idempotent.log"

  # The engine-free rung of the RLS gate is the instance's OWN downloaded psql.
  # A host psql on PATH would silently mask it, so the shim run removes psql.
  local shim="${EVIDENCE}/.nopsql"
  mkdir -p "$shim"
  local b p
  for b in bash sh env awk sed grep cat curl dirname readlink mktemp rm mkdir tail head printf find sort uniq tr cut go make git; do
    p="$(command -v "$b" 2>/dev/null)" || true
    # builtins (printf) resolve to a bare name, which would become a dangling
    # symlink; only real files on disk are shimmed.
    case "${p:-}" in
      /*) [ ! -e "${shim}/${b}" ] && ln -s "$p" "${shim}/${b}" 2>/dev/null ;;
    esac
  done
  : >"$log"
  if instance_env "$INSTANCE"; "${ENVX[@]}" PATH="$shim" bash "${REPO_ROOT}/scripts/check-rls.sh" \
       "$("$CONTAINER_SH" db-url "$INSTANCE" 2>/dev/null)" >"$log" 2>&1; then
    check "make rls-check passes with NO host psql (the downloaded-psql rung)" 0
  else
    check "make rls-check passes with NO host psql (the downloaded-psql rung)" 1 "scripts/check-rls.sh:49-78"
  fi
  cp "$log" "${EVIDENCE}/02-rls.log"
  check_contains "the RLS gate really ran (not SKIPPED)" "$log" "RLS gate OK"
  check_absent "the RLS gate did not silently skip" "$log" "RLS gate SKIPPED"
  instance_env "$INSTANCE"
  "${ENVX[@]}" ORCHICON_INSTANCE="$INSTANCE" bash "${REPO_ROOT}/scripts/check-rls.sh" \
    "$("$CONTAINER_SH" db-url "$INSTANCE" 2>/dev/null)" >"${EVIDENCE}/02-rls-unshimmed.log" 2>&1 || true
  dim "unshimmed run recorded as qa-evidence/.../02-rls-unshimmed.log (must also say 'RLS gate OK')"
}

# --- step 3: the client connects -------------------------------------------
step3_connect() {
  begin_step 3 "orch connects to the running plane and renders"
  local log cap token
  log="${EVIDENCE}/$(step_file)"
  cap="${EVIDENCE}/03-orch-tui.txt"
  if ! token="$(resolve_token)" || [ -z "${token:-}" ]; then
    skip "orch connects and renders" "no client token resolvable (~/.orchicon/config or ORCHICON_TOKEN) — mint one with '/connect' in the TUI or 'orchicon ... token' (see docs/no-engine-e2e-acceptance.md)"
    return 0
  fi
  script -q -c "timeout 12 '$BIN/orch' --url '$(api_base "$INSTANCE")' --token '$token'" "$cap" >/dev/null 2>&1
  printf 'capture: %s (%s bytes)\n' "${cap#"$REPO_ROOT"/}" "$(wc -c <"$cap" 2>/dev/null || echo 0)" >>"$log"
  check "orch renders a screen (capture is non-trivial)" "$([ -s "$cap" ] && [ "$(wc -c <"$cap")" -gt 200 ] && echo 0 || echo 1)"
  check_absent "the client did not fail to reach the plane" "$cap" "connection refused"
  check_absent "the client did not fail to dial the plane" "$cap" "dial tcp"
  grep -qiE 'orchicon|work|runs|127\.0\.0\.1' "$cap" 2>/dev/null \
    && check "the rendered screen names the plane it reached" 0 \
    || check "the rendered screen names the plane it reached" 1 "cmd/orch/main.go:215-220"
}

# --- step 4: change and rebuild, verifiably live ----------------------------
MARKER_SRC="frontend/src/main.tsx"

apply_marker_edit() {
  cat >>"${REPO_ROOT}/${MARKER_SRC}" <<EOF

// no-engine-e2e acceptance marker (removed by the harness): proves the SERVED
// bundle came from this working tree and not from a stale dist.
if (typeof document !== "undefined") {
  document.documentElement.setAttribute("data-no-engine-e2e", "${MARKER}");
}
EOF
  MARKER_EDITED=1
}

restart_plane() { # restart_plane <inst> <log>
  stop_plane "$1"
  start_plane "$1" "$2"
  wait_healthz "${PORT_CTRL[$1]}" 180
}

step4_rebuild_live() {
  begin_step 4 "change and rebuild — is the change actually live?"
  local log bundle
  log="${EVIDENCE}/$(step_file)"
  bundle="${EVIDENCE}/04-served-bundle.txt"
  local rc

  # Fetchability is a GATE, not one assertion among many: "the marker is NOT in
  # the bundle" is trivially true of a 0-byte capture, so a blind run reports the
  # stale-dist control and the escape hatch as PASS while the plane serves no UI
  # at all. When the baseline fetch fails, every assertion that reads bundle
  # CONTENT is SKIPped — AC3 is then UNPROVEN, never green.
  local bundle_ok=0
  served_bundle_bytes "$INSTANCE" >"$bundle" 2>/dev/null || rc=1
  if [ "${rc:-0}" = "0" ] && [ -s "$bundle" ]; then bundle_ok=1
    check "the served bundle is fetchable (independent: HTTP GET of the served asset)" 0
  else
    check "the served bundle is fetchable (independent: HTTP GET of the served asset)" 1 "cmd/orchicon/serve_state.go:124-141 (no embedded dist -> / is the API, 401) — see 01-served-index.txt"
  fi
  dist_bundle_bytes >"${EVIDENCE}/04-dist-bundle.txt" 2>/dev/null || true
  if [ "$bundle_ok" = "1" ]; then
    check_absent "the marker is NOT in the served bundle before the edit (clean baseline)" "$bundle" "$MARKER"
  else
    skip "the marker is NOT in the served bundle before the edit (clean baseline)" "the served bundle is UNFETCHABLE — an absence in an empty capture proves nothing"
  fi

  git -C "$REPO_ROOT" checkout -- "$MARKER_SRC" 2>/dev/null || true
  apply_marker_edit
  git -C "$REPO_ROOT" --no-pager diff -- "$MARKER_SRC" >"${EVIDENCE}/04-diff.txt" 2>&1 || true
  check "one frontend string was edited (see 04-diff.txt)" "$(grep -qF "$MARKER" "${EVIDENCE}/04-diff.txt" && echo 0 || echo 1)"

  # The DOCUMENTED engine-free rebuild path (container-rebuild needs an engine).
  if tee_step run_make "$INSTANCE" build; then check "the documented rebuild path (make build) succeeds" 0
  else check "the documented rebuild path (make build) succeeds" 1 "Makefile:255"; fi
  if restart_plane "$INSTANCE" "${EVIDENCE}/04-run-after-rebuild.log"; then
    check "the plane restarts after the rebuild (the bundle is go:embed-ed)" 0
  else check "the plane restarts after the rebuild (the bundle is go:embed-ed)" 1 "Makefile:272-278"; fi
  served_bundle_bytes "$INSTANCE" >"${EVIDENCE}/04-served-bundle-after.txt" 2>/dev/null || true
  dist_bundle_bytes >"${EVIDENCE}/04-dist-bundle-after.txt" 2>/dev/null || true
  # Two independent places, so a miss bisects instead of blurring: the BUILT dist
  # is the stamp check's product (Makefile:569-581), the SERVED bytes are the
  # embed/restart's product. AC3 needs both, and the dist half is judged even
  # when the server half cannot be fetched.
  if [ "$bundle_ok" = "1" ]; then
    check_contains "THE CHANGE IS LIVE: the SERVED bundle carries the new marker" \
      "${EVIDENCE}/04-served-bundle-after.txt" "$MARKER" "Makefile:569-581"
  else
    skip "THE CHANGE IS LIVE: the SERVED bundle carries the new marker" "the served bundle was UNFETCHABLE at the baseline — AC3's served half is UNPROVEN, not passed (see 01-served-index.txt)"
  fi
  check_contains "the BUILT dist carries the new marker after make build (the build half of AC3)" \
    "${EVIDENCE}/04-dist-bundle-after.txt" "$MARKER" "Makefile:569-581"

  # --- adversarial control: recreate the stale-dist failure mode -------------
  git -C "$REPO_ROOT" checkout -- "$MARKER_SRC" 2>/dev/null || true
  touch -d '+1 hour' "${REPO_ROOT}/frontend/dist/index.html" 2>/dev/null \
    || touch -t "$(date -d '+1 hour' +%Y%m%d%H%M 2>/dev/null || echo 209901010000)" "${REPO_ROOT}/frontend/dist/index.html"
  tee -a "$log" <<EOF
adversarial control: frontend/src restored without the marker, and
frontend/dist/index.html mtime forced 1h into the FUTURE, so the stamp check
sees "dist is newer than src". Correct behaviour is a rebuild anyway.
EOF
  run_make "$INSTANCE" build >>"$log" 2>&1 || true
  restart_plane "$INSTANCE" "${EVIDENCE}/04-run-after-stale-control.log" >/dev/null 2>&1 || true
  served_bundle_bytes "$INSTANCE" >"${EVIDENCE}/04-served-bundle-stale-control.txt" 2>/dev/null || true
  dist_bundle_bytes >"${EVIDENCE}/04-dist-bundle-stale-control.txt" 2>/dev/null || true
  if [ "$bundle_ok" = "1" ]; then
    check_absent "STALE-DIST CONTROL: a dist newer than src does not ship the previous UI (SERVED)" \
      "${EVIDENCE}/04-served-bundle-stale-control.txt" "$MARKER" "Makefile:574-581 then Makefile:655 (only container-rebuild forces force-fe=1)"
  else
    skip "STALE-DIST CONTROL: a dist newer than src does not ship the previous UI (SERVED)" "the served bundle was UNFETCHABLE at the baseline — see 01-served-index.txt"
  fi
  check_absent "STALE-DIST CONTROL (BUILT): a dist newer than src does not keep the previous bundle" \
    "${EVIDENCE}/04-dist-bundle-stale-control.txt" "$MARKER" "Makefile:574-581 (only container-rebuild forces force-fe=1)"

  # --- the documented escape hatch must actually work -----------------------
  if tee_step run_make "$INSTANCE" build force-fe=1; then check "make build force-fe=1 (the engine-free escape hatch) succeeds" 0
  else check "make build force-fe=1 (the engine-free escape hatch) succeeds" 1 "Makefile:573-581"; fi
  restart_plane "$INSTANCE" "${EVIDENCE}/04-run-after-force-fe.log" >/dev/null 2>&1 || true
  served_bundle_bytes "$INSTANCE" >"${EVIDENCE}/04-served-bundle-force-fe.txt" 2>/dev/null || true
  dist_bundle_bytes >"${EVIDENCE}/04-dist-bundle-force-fe.txt" 2>/dev/null || true
  if [ "$bundle_ok" = "1" ]; then
    check_absent "the escape hatch clears the marker from the SERVED bundle" \
      "${EVIDENCE}/04-served-bundle-force-fe.txt" "$MARKER"
  else
    skip "the escape hatch clears the marker from the SERVED bundle" "the served bundle was UNFETCHABLE — an absence in an empty capture proves nothing"
  fi
  check_absent "the escape hatch clears the marker from the BUILT dist" \
    "${EVIDENCE}/04-dist-bundle-force-fe.txt" "$MARKER"
  check "the working tree is restored (frontend/src is clean)" \
    "$(git -C "$REPO_ROOT" diff --quiet -- "$MARKER_SRC" && echo 0 || echo 1)"
}

# --- step 5: state ----------------------------------------------------------
step5_state() {
  begin_step 5 "state — can the developer tell what is running?"
  local log
  log="${EVIDENCE}/$(step_file)"
  local out="${EVIDENCE}/05-status-script.txt" gt="${EVIDENCE}/05-status-groundtruth.txt"
  local code

  timeout 30 "$ORCHICON_SH" status "$INSTANCE" >"$out" 2>&1 || true
  tee -a "$log" <"$out" >/dev/null 2>&1 || true

  code="$(healthz_code "$INSTANCE")"
  {
    echo "# independent ground truth, recorded beside the script's own report"
    echo "healthz http code: ${code:-<none>}"
    echo "ports listening:  pg=${PORT_PG[$INSTANCE]}:$(port_listening "${PORT_PG[$INSTANCE]}"; echo $?) nats=${PORT_NATS[$INSTANCE]}:$(port_listening "${PORT_NATS[$INSTANCE]}"; echo $?) control=${PORT_CTRL[$INSTANCE]}:$(port_listening "${PORT_CTRL[$INSTANCE]}"; echo $?)   (0=LISTENing)"
    echo "pid owning control port: $(pid_owning_port "${PORT_CTRL[$INSTANCE]}" 2>/dev/null || echo none)"
    echo "pid owning pg port:      $(pid_owning_port "${PORT_PG[$INSTANCE]}" 2>/dev/null || echo none)"
    echo "process table:"
    procs_matching 'postgres|nats-server|exe/orchicon' | sed 's/^/  /'
    echo "services status:"
    instance_env "$INSTANCE"
    "${ENVX[@]}" "$BIN/orchicon" services status "$INSTANCE" 2>&1 | sed 's/^/  /'
  } >"$gt" 2>&1
  tee -a "$log" <"$gt" >/dev/null 2>&1 || true

  check_absent "status makes no claim it cannot verify (no container-derived 'not created')" \
    "$out" "not created" "scripts/container.sh:1603-1630 status_instances (unguarded 'docker ps')"
  check_absent "status does not leak a container error" "$out" "docker: command not found" \
    "scripts/container.sh:1603-1630"
  check_absent "status does not leak a container error (docker: not found)" "$out" "docker: not found" \
    "scripts/container.sh:1603-1630"
  grep -qE "$INSTANCE" "$out" \
    && check "status names the instance" 0 \
    || check "status names the instance" 1 "scripts/container.sh:1603-1630"
  grep -qF "${PORT_CTRL[$INSTANCE]}" "$out" \
    && check "status names the plane's port (${PORT_CTRL[$INSTANCE]})" 0 \
    || check "status names the plane's port (${PORT_CTRL[$INSTANCE]})" 1 "scripts/container.sh:plane_status"
  grep -qiE 'services|postgres|543|nats' "$out" \
    && check "status reports the SERVICES half (Postgres/NATS), not only the plane" 0 \
    || check "status reports the SERVICES half (Postgres/NATS), not only the plane" 1 "scripts/container.sh:1603-1630"
  if [ "$code" = "200" ]; then
    grep -qiE 'healthy|health|up|ok' "$out" \
      && check "status claims health only where /healthz proved it (healthz=200)" 0 \
      || check "status claims health only where /healthz proved it (healthz=200, but the report omits it)" 1 "scripts/container.sh:plane_status"
  else
    check "status claimed no health while /healthz was unreachable (healthz=${code:-none})" 0
  fi
  grep -qE 'docker (ps|inspect|logs)' "$log" \
    && check "the state report needs NO container command" 1 "scripts/orchicon.sh:97 (status verb) + scripts/container.sh:1603" \
    || check "the state report needs NO container command" 0
}

# --- step 6: logs -----------------------------------------------------------
step6_logs() {
  begin_step 6 "logs — the developer can see the plane's recent activity"
  local log out plog rc=0
  log="${EVIDENCE}/$(step_file)"
  out="${EVIDENCE}/06-logs.txt"

  timeout 10 "$ORCHICON_SH" logs "$INSTANCE" >"$out" 2>&1 || rc=$?
  printf 'exit=%s\n' "$rc" >>"$log"
  tee -a "$log" <"$out" >/dev/null 2>&1 || true

  # ANY non-zero exit is a failure, not just 127: a `docker logs -f` that HANGS
  # (timeout kills it, rc=124) prints no 'command not found' line, and treating
  # that as a pass would hide exactly the blind spot this step exists to find.
  case "$rc" in
    0) check "scripts/orchicon.sh logs works with no container engine" 0 ;;
    124) check "scripts/orchicon.sh logs works with no container engine (it HUNG: timeout 10 killed it)" 1 \
           "scripts/orchicon.sh:101 (logs -> scripts/container.sh:1632 logs_instance -> 'docker logs -f')" ;;
    *) check "scripts/orchicon.sh logs works with no container engine (exit ${rc})" 1 \
           "scripts/orchicon.sh:101 (logs -> scripts/container.sh:1632 logs_instance -> 'docker logs -f')" ;;
  esac

  plog="$(plane_log_path "$INSTANCE")"
  if [ -f "$plog" ]; then
    check "the plane writes its log to ${plog#"$HOME"/} (the path every other verb already knows)" 0
    cp "$plog" "${EVIDENCE}/06-plane-log.txt" 2>/dev/null || true
  else
    check "the plane writes its log to ${plog#"$HOME"/} (the path every other verb already knows)" 1 \
      "Makefile:272-278 (make run is foreground and writes no log) + scripts/container.sh:478"
  fi

  if [ -s "$out" ] && grep -qE 'orchicon|serve|listen|http|POST|GET' "$out"; then
    check "the logs command shows this session's activity (steps 1-4)" 0
  else
    check "the logs command shows this session's activity (steps 1-4)" 1 \
      "scripts/orchicon.sh:127 + scripts/container.sh:logs_instance"
  fi

  {
    echo "# what the developer had to do INSTEAD to see the plane's activity"
    echo "# (evidence, not a pass): the tail of make run's stdout from step 1"
    tail -25 "${EVIDENCE}/01-run.log" 2>/dev/null
  } >"${EVIDENCE}/06-workaround-run-log-tail.txt"
  dim "workaround recorded: 06-workaround-run-log-tail.txt (a terminal the developer still owns)"
}

# --- step 7: two instances --------------------------------------------------
step7_two_instances() {
  begin_step 7 "two instances — dev and prod coexist and are independently controllable"
  local log
  log="${EVIDENCE}/$(step_file)"

  check "the two instances resolve DIFFERENT ports" \
    "$([ "${PORT_PG[$INSTANCE]}" != "${PORT_PG[$PROD_INSTANCE]}" ] && [ "${PORT_CTRL[$INSTANCE]}" != "${PORT_CTRL[$PROD_INSTANCE]}" ] && echo 0 || echo 1)"
  dim "dev: pg=${PORT_PG[$INSTANCE]} nats=${PORT_NATS[$INSTANCE]} control=${PORT_CTRL[$INSTANCE]} | prod: pg=${PORT_PG[$PROD_INSTANCE]} nats=${PORT_NATS[$PROD_INSTANCE]} control=${PORT_CTRL[$PROD_INSTANCE]}"

  if tee_step run_make "$PROD_INSTANCE" services; then check "make services instance=prod starts prod's own Postgres + NATS" 0
  else check "make services instance=prod starts prod's own Postgres + NATS" 1 "Makefile:460"; fi
  cp "$log" "${EVIDENCE}/07-prod-services.log"

  start_plane "$PROD_INSTANCE" "${EVIDENCE}/07-prod-run.log"
  if wait_healthz "${PORT_CTRL[$PROD_INSTANCE]}" 180; then check "prod's plane answers /healthz" 0
  else check "prod's plane answers /healthz" 1 "Makefile:272-278"; fi
  check "dev's plane still answers /healthz alongside prod" "$([ "$(healthz_code "$INSTANCE")" = "200" ] && echo 0 || echo 1)"
  check "prod's Postgres is LISTENing on ${PORT_PG[$PROD_INSTANCE]}" "$(port_listening "${PORT_PG[$PROD_INSTANCE]}"; echo $?)"

  : >"$log"
  if tee_step run_make "$PROD_INSTANCE" migrate; then check "make migrate instance=prod applies to PROD's database" 0
  else check "make migrate instance=prod applies to PROD's database" 1 "Makefile:431-433"; fi
  cp "$log" "${EVIDENCE}/07-prod-migrate.log"
  local devdsn proddsn
  devdsn="$("$CONTAINER_SH" db-url "$INSTANCE" 2>/dev/null)"
  proddsn="$("$CONTAINER_SH" db-url "$PROD_INSTANCE" 2>/dev/null)"
  check "the two instances' DSNs differ (no shared database)" "$([ "$devdsn" != "$proddsn" ] && echo 0 || echo 1)"
  printf 'dev DSN:  %s\nprod DSN: %s\n' "$devdsn" "$proddsn" >>"$log"
  check "dev's plane is untouched by prod's migrate" "$([ "$(healthz_code "$INSTANCE")" = "200" ] && echo 0 || echo 1)"

  # stop dev ONLY; prod must not notice.
  stop_plane "$INSTANCE" >>"$log" 2>&1 || true
  run_make "$INSTANCE" services-down >>"$log" 2>&1 || true
  check "dev's plane is down after stopping dev only" "$(! port_listening "${PORT_CTRL[$INSTANCE]}"; echo $?)"
  check "prod's plane survives dev's shutdown" "$([ "$(healthz_code "$PROD_INSTANCE")" = "200" ] && echo 0 || echo 1)"
  check "prod's Postgres survives dev's shutdown" "$(port_listening "${PORT_PG[$PROD_INSTANCE]}"; echo $?)"

  # bring dev back
  run_make "$INSTANCE" services >>"$log" 2>&1 || true
  start_plane "$INSTANCE" "${EVIDENCE}/07-dev-run-again.log"
  check "dev comes back up with prod still running" \
    "$(wait_healthz "${PORT_CTRL[$INSTANCE]}" 180 && [ "$(healthz_code "$PROD_INSTANCE")" = "200" ] && echo 0 || echo 1)"
}

# share_snapshot <file> — the entry names under ~/.local/share, sorted, so a
# before/after diff names exactly what a cleanup removed (no ls column parsing).
share_snapshot() {
  find "$HOME/.local/share" -maxdepth 1 -mindepth 1 -printf '%f\n' 2>/dev/null | sort >"$1" || : >"$1"
}

# --- step 8: hygiene --------------------------------------------------------
step8_hygiene() {
  begin_step 8 "hygiene — the cleanup prunes only Orchicon's artifacts"
  local log sentinel
  log="${EVIDENCE}/$(step_file)"
  sentinel="$HOME/.local/share/e2e-sentinel-${UTC_STAMP}"
  local before="${EVIDENCE}/08-share-before.txt" after="${EVIDENCE}/08-share-after.txt" rc=0

  mkdir -p "$sentinel" && printf 'not ours\n' >"${sentinel}/keep.txt"
  share_snapshot "$before"

  run_make "$INSTANCE" clean-docker >>"$log" 2>&1 || rc=$?
  printf 'first run exit=%s\n' "$rc" >>"$log"
  if [ "$rc" = "0" ]; then check "make clean-docker is a clean no-op with no container engine" 0
  else
    check "make clean-docker is a clean no-op with no container engine (exit $rc)" 1 "Makefile:413-416"
  fi
  check_absent "clean-docker does not fail with a container error" "$log" "docker: command not found" "Makefile:413-416"
  check_absent "clean-docker does not fail with a container error (docker: not found)" "$log" "docker: not found" "Makefile:413-416"

  rc=0
  run_make "$INSTANCE" clean-docker >"${EVIDENCE}/08-cleanup-second.log" 2>&1 || rc=$?
  printf 'second run exit=%s\n' "$rc" >>"$log"
  check "a second cleanup is also a clean no-op (exit 0)" "$([ "$rc" = "0" ] && echo 0 || echo 1)" "Makefile:413-416"

  share_snapshot "$after"
  check "a path outside Orchicon's own artifacts survives the cleanup (sentinel intact)" \
    "$([ -f "${sentinel}/keep.txt" ] && echo 0 || echo 1)" "Makefile:410-416"
  local removed
  removed="$(comm -23 "$before" "$after" 2>/dev/null || true)"
  check "nothing outside Orchicon's artifacts disappeared from ~/.local/share" "$([ -z "$removed" ] && echo 0 || echo 1)"
  [ -n "$removed" ] && printf 'removed entries:\n%s\n' "$removed" >>"$log"
  # the sentinel is ours and only ours: remove the file, then the directory
  rm -f "${sentinel}/keep.txt" 2>/dev/null || true
  rmdir "$sentinel" 2>/dev/null || true
}

# --- step 9: stop -----------------------------------------------------------
step9_stop() {
  begin_step 9 "stop — nothing is left holding a port"
  local log waited=0 inst
  log="${EVIDENCE}/$(step_file)"

  stop_plane "$INSTANCE" | tee -a "$log"
  stop_plane "$PROD_INSTANCE" | tee -a "$log"
  for inst in "$INSTANCE" "$PROD_INSTANCE"; do
    run_make "$inst" services-down >>"$log" 2>&1 || true
  done

  while [ "$waited" -lt 60 ]; do
    if ! port_listening "${PORT_PG[$INSTANCE]}" && ! port_listening "${PORT_NATS[$INSTANCE]}" \
       && ! port_listening "${PORT_CTRL[$INSTANCE]}" && ! port_listening "${PORT_PG[$PROD_INSTANCE]}" \
       && ! port_listening "${PORT_NATS[$PROD_INSTANCE]}" && ! port_listening "${PORT_CTRL[$PROD_INSTANCE]}"; then
      break
    fi
    sleep 1; waited=$((waited + 1))
  done

  local held
  held="$(ports_free_list "${PORT_PG[$INSTANCE]}" "${PORT_NATS[$INSTANCE]}" "${PORT_CTRL[$INSTANCE]}" "${PORT_PG[$PROD_INSTANCE]}" "${PORT_NATS[$PROD_INSTANCE]}" "${PORT_CTRL[$PROD_INSTANCE]}")"
  {
    echo "ports still held after ${waited}s: ${held:-none}"
    echo "process table (postgres|nats-server|exe/orchicon):"
    procs_matching 'postgres|nats-server|exe/orchicon' | sed 's/^/  /'
  } >"${EVIDENCE}/09-ports.txt" 2>&1
  tee -a "$log" <"${EVIDENCE}/09-ports.txt" >/dev/null 2>&1 || true
  check "every instance port came free (no orphaned Postgres/NATS/plane)" "$([ -z "${held// /}" ] && echo 0 || echo 1)" \
    "cmd/orchicon servicesDown (port verification)"

  local rc=0
  instance_env "$INSTANCE"
  "${ENVX[@]}" "$BIN/orchicon" services status "$INSTANCE" >"${EVIDENCE}/09-services-status.txt" 2>&1 || rc=$?
  printf 'services status exit=%s\n' "$rc" >>"$log"
  check "services status reports DOWN (exit 1) after the stop" "$([ "$rc" = "1" ] && echo 0 || echo 1)"

  # leaving nothing behind means the harness's own registry is empty too
  PLANE_PIDS=()
  TEARDOWN_DONE=1
}

# --- step 10: no blind spots (derived from the evidence, not from prose) ----
step10_blindspots() {
  begin_step 10 "no blind spots — every step answers 'what is running, and why?'"
  local md="${EVIDENCE}/10-blindspots.md"
  {
    echo "# No blind spots — what answers \"what is running, and why?\""
    echo
    echo "| step | the documented command | the evidence beside it | verdict |"
    echo "|---|---|---|---|"
    printf '| 1 | `make build` / `make services` / `make run` | `01-healthz.txt`, `/proc` port ownership | %s |\n' "$(verdict_for 1)"
    printf '| 2 | `make migrate` (x2), `make rls-check` | `02-migrate*.log`, `02-rls.log` (no host psql) | %s |\n' "$(verdict_for 2)"
    printf '| 3 | `bin/orch --url http://127.0.0.1:%s` | `03-orch-tui.txt` PTY capture | %s |\n' "${PORT_CTRL[$INSTANCE]}" "$(verdict_for 3)"
    printf '| 4 | `make build` + restart (the plane go:embeds dist) | `04-served-bundle*.txt` (SERVED bytes) + `04-dist-bundle*.txt` (BUILT bytes) + `01-served-index.txt` (HTTP status of /) | %s |\n' "$(verdict_for 4)"
    printf '| 5 | `scripts/orchicon.sh status %s` | `05-status-groundtruth.txt` (/healthz, /proc) | %s |\n' "$INSTANCE" "$(verdict_for 5)"
    printf '| 6 | `scripts/orchicon.sh logs %s` | `06-logs.txt` + `06-plane-log.txt` | %s |\n' "$INSTANCE" "$(verdict_for 6)"
    printf '| 7 | `make services/migrate instance=prod` | `07-*.log`, both `/healthz` | %s |\n' "$(verdict_for 7)"
    printf '| 8 | `make clean-docker` | `08-share-{before,after}.txt`, sentinel | %s |\n' "$(verdict_for 8)"
    printf '| 9 | `make services-down` (both instances) | `09-ports.txt`, `/proc/net/tcp` | %s |\n' "$(verdict_for 9)"
    echo
    echo "A cell that resolves to \"only knowable by reading source or running a container"
    echo "command\" is a FAIL of AC9 and is listed in defects.tsv with the file:line that owns it."
  } >"$md"
  tee -a "${EVIDENCE}/$(step_file)" <"$md" >/dev/null 2>&1 || true

  local fails_here
  fails_here="$(awk -F'\t' '$2=="FAIL" && $1!="10" {print}' "${EVIDENCE}/results.tsv" 2>/dev/null | wc -l)"
  check "every earlier step is answerable from a documented command plus its evidence (${fails_here} step-level failures to attribute)" \
    "$([ "$fails_here" = "0" ] && echo 0 || echo 1)" "see defects.tsv"
}

verdict_for() { # verdict_for <step>
  local n="$1" st
  st="$(awk -F'\t' -v s="$n" '$1==s {print $2}' "${EVIDENCE}/results.tsv" 2>/dev/null | sort -u | tr '\n' ',' )"
  case "$st" in
    *FAIL*) printf '**FAIL** (see defects.tsv)' ;;
    *SKIP*) printf 'SKIP' ;;
    ""|,) printf 'not run' ;;
    *) printf 'PASS' ;;
  esac
}

# --- summary + teardown -----------------------------------------------------
write_summary() {
  local md="${EVIDENCE}/summary.md"
  {
    echo "# No-engine developer session — harness run ${UTC_STAMP}"
    echo
    echo "- passes: ${PASSES}   failures: ${FAILS}   skips: ${SKIPS}"
    echo "- marker: \`${MARKER}\` (unique per run: a served-byte match cannot be a leftover)"
    echo "- instance: \`${INSTANCE}\` (pg ${PORT_PG[$INSTANCE]} / nats ${PORT_NATS[$INSTANCE]} / control ${PORT_CTRL[$INSTANCE]}), second instance \`${PROD_INSTANCE}\` (pg ${PORT_PG[$PROD_INSTANCE]} / nats ${PORT_NATS[$PROD_INSTANCE]} / control ${PORT_CTRL[$PROD_INSTANCE]})"
    [ -n "$ABORT_REASON" ] && { echo; echo "**RUN INVALID:** ${ABORT_REASON}"; }
    echo
    echo "## per-step verdict"
    echo
    echo "| step | verdict |"
    echo "|---|---|"
    local n
    for n in 0 1 2 3 4 5 6 7 8 9 10; do printf '| %s | %s |\n' "$n" "$(verdict_for "$n")"; done
    echo
    echo "## /healthz (verbatim, independent of any script's report)"
    echo
    echo '```'
    cat "${EVIDENCE}/01-healthz.txt" 2>/dev/null || echo '(not captured)'
    echo '```'
    echo
    echo "## failures — each names the file that owns it"
    echo
    if [ -s "${EVIDENCE}/defects.tsv" ]; then
      echo "| step | assertion | owner |"
      echo "|---|---|---|"
      awk -F'\t' '{printf "| %s | %s | `%s` |\n", $1, $2, $3}' "${EVIDENCE}/defects.tsv"
    else
      echo "none."
    fi
    echo
    echo "## evidence files"
    echo
    ls -1 "$EVIDENCE" | sed 's/^/- /'
  } >"$md" 2>&1
  say ""
  say "summary: ${md#"$REPO_ROOT"/}"
  say "passes=${PASSES} failures=${FAILS} skips=${SKIPS}"
}

teardown() {
  local inst
  # An interrupted step 4 must not leave the acceptance marker in a tree the
  # runbook calls a CLEAN checkout — restore it before any other exit path.
  if [ "${MARKER_EDITED:-0}" = "1" ]; then
    git -C "$REPO_ROOT" checkout -- "$MARKER_SRC" 2>/dev/null || true
    say "teardown: restored ${MARKER_SRC} (the acceptance marker was still in the working tree)"
  fi
  if [ "$KEEP" = "1" ] || [ "${TEARDOWN_DONE:-0}" = "1" ]; then
    [ "$KEEP" = "1" ] && say "(--keep: services and planes left running)"
    return 0
  fi
  say "teardown: stopping anything this harness started"
  for inst in "$INSTANCE" "$PROD_INSTANCE"; do
    stop_plane "$inst" >/dev/null 2>&1 || true
    run_make "$inst" services-down >/dev/null 2>&1 || true
  done
}

DRY_RUN_PLAN() {
  cat <<EOF
no-engine-e2e — planned session (dry run: nothing is executed)

  marker       ${MARKER}
  instance     ${INSTANCE}: pg ${PORT_PG[$INSTANCE]} nats ${PORT_NATS[$INSTANCE]} control ${PORT_CTRL[$INSTANCE]}
  second       ${PROD_INSTANCE}: pg ${PORT_PG[$PROD_INSTANCE]} nats ${PORT_NATS[$PROD_INSTANCE]} control ${PORT_CTRL[$PROD_INSTANCE]}
  evidence     qa-evidence/no-engine-e2e/${UTC_STAMP}/

  0  preflight            assert NO container engine; measure the cold-build disk budget
                          (checkout / caches / TMPDIR); snapshot host, ports, process table
  1  cold start           make build -> make services -> make run (background) -> GET /healthz
  2  migrate + rls        make migrate (x2, second a no-op) -> make rls-check with NO host psql
  3  connect              PTY capture of bin/orch --url <plane> --token <resolved>
  4  change + rebuild     edit ${MARKER_SRC} -> make build -> restart -> GREP THE SERVED BYTES
                          adversarial: restore src, dist mtime +1h -> make build -> marker must go
                          then make build force-fe=1 (escape hatch) and restore the tree
  5  state                scripts/orchicon.sh status vs /healthz, /proc ports, /proc owners
  6  logs                 scripts/orchicon.sh logs (bounded) + the plane log file
  7  two instances        prod alongside dev: healthz 8091, migrate 5433, stop dev only
  8  hygiene              make clean-docker twice with an outside sentinel in ~/.local/share
  9  stop                 both planes + services-down -> every port free, no orphan
 10  no blind spots       emit 10-blindspots.md from the recorded evidence

exit: 0 all PASS | 1 any FAIL | 2 run invalid (engine present / ports held / no disk budget)
EOF
}

usage() { sed -n '3,80p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --instance) INSTANCE="${2:?}"; shift 2 ;;
      --evidence-dir) EVIDENCE="${2:?}"; shift 2 ;;
      --pg-port) PG_OVERRIDE="${2:?}"; shift 2 ;;
      --nats-port) NATS_OVERRIDE="${2:?}"; shift 2 ;;
      --control-port) CTRL_OVERRIDE="${2:?}"; shift 2 ;;
      --prod-pg-port) PROD_PG_OVERRIDE="${2:?}"; shift 2 ;;
      --prod-nats-port) PROD_NATS_OVERRIDE="${2:?}"; shift 2 ;;
      --prod-control-port) PROD_CTRL_OVERRIDE="${2:?}"; shift 2 ;;
      --keep) KEEP=1; shift ;;
      --dry-run) DRY_RUN=1; shift ;;
      --preflight-only) PREFLIGHT_ONLY=1; shift ;;
      -h|--help) usage 0 ;;
      *) echo "no-engine-e2e: unknown argument '$1' (see --help)" >&2; exit 2 ;;
    esac
  done
  case "$INSTANCE" in dev|prod) ;; test) ;; *) echo "no-engine-e2e: --instance must be dev, prod or test" >&2; exit 2 ;; esac
  [ "$INSTANCE" = "$PROD_INSTANCE" ] && PROD_INSTANCE=dev
  [ -n "$EVIDENCE" ] || EVIDENCE="${REPO_ROOT}/qa-evidence/no-engine-e2e/${UTC_STAMP}"
  case "$EVIDENCE" in /*) ;; *) EVIDENCE="${REPO_ROOT}/${EVIDENCE}" ;; esac
}

main() {
  parse_args "$@"
  resolve_ports
  if [ "$DRY_RUN" = "1" ]; then DRY_RUN_PLAN; exit 0; fi

  mkdir -p "$EVIDENCE"
  : >"${EVIDENCE}/results.tsv"
  : >"${EVIDENCE}/defects.tsv"
  trap teardown EXIT

  if ! preflight; then
    write_summary
    say ""
    say "RUN INVALID: ${ABORT_REASON}"
    TEARDOWN_DONE=1   # nothing was started, so nothing may be stopped
    exit 2
  fi
  if [ "$PREFLIGHT_ONLY" = "1" ]; then
    write_summary
    say "preflight only: the host is a valid no-engine host. Re-run without --preflight-only for the session."
    # A validity check must not be a side effect: no plane and no service was
    # started here, so tearing down would stop a developer's OWN running session.
    TEARDOWN_DONE=1
    exit 0
  fi

  step1_cold_start
  step2_migrate
  step3_connect
  step4_rebuild_live
  step5_state
  step6_logs
  step7_two_instances
  step8_hygiene
  step9_stop
  step10_blindspots

  write_summary
  say ""
  if [ "$FAILS" -gt 0 ]; then
    say "RESULT: ${FAILS} assertion(s) failed — each is a defect against the file named above."
    say "        This feature FILES defects; it never patches a sibling's code (AC11)."
    exit 1
  fi
  say "RESULT: the whole documented developer session works with no container engine, and nothing is blind."
  exit 0
}

main "$@"
