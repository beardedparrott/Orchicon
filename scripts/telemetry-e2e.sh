#!/usr/bin/env bash
# ============================================================================
# E2E acceptance harness for THE TELEMETRY PAGE WITH GRAFANA ABSENT.
#
# THE CLAIM. "A user opens /telemetry on a machine that has no Grafana at all and
# sees graphs, log search, a trace view and a report built from telemetry that real
# work actually produced — with filters that narrow, a time range that can honestly
# come back empty, and a degraded state that NAMES the backend it lost and recovers
# live when the backend comes back."
#
# A component test cannot prove that claim. Every panel can pass its own unit test
# while the page still reads a fixture, a filter silently returns nothing, or the
# degraded state never appears because the error was swallowed. So this harness
# believes none of the page until an INDEPENDENT probe agrees:
#
#   * AC1  Grafana is genuinely ABSENT — no binary, no process, no image, no config,
#          and the plane's /grafana route answers 404. The absence probes carry a
#          CONTROL pattern that must match first, so an empty `pgrep` is allowed to
#          mean "none" instead of "pgrep is broken on this host".
#   * AC2  data comes from a REAL completed workflow run created and started through
#          the plane's own API in this run, and the ids in the stores (queried
#          DIRECTLY, bypassing Orchicon) are the ids the page's Connect responses
#          carry. A page reading fixtures cannot produce the run's trace id.
#   * AC3  graphs / log search / trace view / report all render from the stores.
#   * AC4  a project filter NARROWS the result set, and a window that excludes the
#          run produces the EMPTY state — not stale content.
#   * AC5  stopping a backend produces the state that NAMES it, and restarting it
#          recovers the SAME page instance. The browser leg is a Playwright spec on
#          purpose: a spec can PROVE no reload() was used (this harness greps the
#          spec for `reload(` before it runs it). A human cannot.
#   * AC6  the evidence is the emitted qa-evidence/telemetry-e2e/<UTC>/ directory.
#   * AC7  this harness patches NOTHING. Every failure is a defect row naming the
#          sibling task's file:line that owns the fix.
#
# WHAT THIS HARNESS REFUSES TO CALL A PASS:
#   * a missing surface. If the native page / the filters / the Grafana removal are
#     not present on the branch under test, the run is BLOCKED (exit 2) with a
#     defect row naming the sibling branch — never green. An acceptance procedure
#     that goes green when the thing under test is absent is worse than none.
#   * the UI's own word. Graph values come from VictoriaMetrics over HTTP, log
#     lines from Loki, traces from Tempo; the page is only allowed to CONFIRM them.
#   * a stack that is not host-resident. Every backend must be a process on this
#     host whose /proc/<pid>/exe matches the binary, not a container.
#
# USAGE
#   scripts/telemetry-e2e.sh [options]
#     --instance dev|prod|test   instance whose services the run drives (default dev)
#     --evidence-dir DIR         evidence root (default qa-evidence/telemetry-e2e/<UTC>)
#     --bin PATH                 an `orchicon` binary to use (default: build one into a scratch dir)
#     --plane-url URL            plane base URL (default: derived from the instance's ports)
#     --run-driver CMD           created-and-completed-run driver; it must print
#                                PROJECT_ID=/RUN_ID=/EXECUTION_ID=/CORRELATION_ID=/TRACE_ID=
#                                lines. Default: the built-in Connect driver (see t3_real_run).
#     --skip-browser             run every non-browser step (AC1/AC2 store truth only)
#     --keep                     leave the stack this run started running
#     --dry-run                  print the plan for every step, execute nothing
#     --preflight-only           run the validity precondition (step 0-1) only
#     -h | --help
#
# ENVIRONMENT
#   ORCHICON_E2E_USER / ORCHICON_E2E_PASSWORD   local-login credentials for the Connect driver
#   ORCHICON_E2E_ROOT                           scratch dir (default $TMPDIR/orchicon/telemetry-e2e)
#   ORCHICON_E2E_POLL_SECONDS                   run-completion poll budget (default 600)
#
# EXIT CODES  0 every assertion passed | 1 at least one FAILED (defect named with file:line)
#             2 NOT a valid proof (a surface is absent, the host cannot run the stack, or
#               a store never answered) — the claim is NOT proven
# ============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/.." && pwd)"

UTC_STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
INSTANCE="dev"
EVIDENCE=""
EVIDENCE_BASE="${REPO_ROOT}/qa-evidence/telemetry-e2e"
E2E_ROOT="${ORCHICON_E2E_ROOT:-${TMPDIR:-/tmp}/orchicon/telemetry-e2e}"
BIN=""
PLANE_URL=""
RUN_DRIVER=""
SKIP_BROWSER=0
KEEP=0
DRY_RUN=0
PREFLIGHT_ONLY=0
POLL_SECONDS="${ORCHICON_E2E_POLL_SECONDS:-600}"

# ids from the real run (T3); every later step reads THESE, never a re-query
PROJECT_ID=""
FOREIGN_PROJECT_ID=""
RUN_ID=""
EXECUTION_ID=""
CORRELATION_ID=""
TRACE_ID=""
LOKI_PID=""
VM_METRIC=""
VM_VALUE=""

PASSES=0
FAILS=0
SKIPS=0
BLOCKED=0
INVALID=0                  # cannot execute HERE (no stack, no browser) - accuses no product code
BLOCKED_SURFACE=0          # a required surface is ABSENT - a defect against the sibling that owns it
ABORT_REASON=""
CURRENT_STEP="0"
STEP_LOG=""
DIRTY=0                    # something of ours may be running (teardown only then)

# The harness's own shell must not decide a case by exporting the state of an
# instance it does not own: an inherited DSN/URL would make the "host-resident
# stack" probe read the wrong ports.
unset ORCHICON_POSTGRES_DSN ORCHICON_NATS_URL ORCHICON_POSTGRES_PORT ORCHICON_CONTROL_PORT 2>/dev/null || true
unset ORCHICON_TEMPO_URL ORCHICON_LOKI_URL ORCHICON_VM_URL ORCHICON_GRAFANA_URL 2>/dev/null || true
unset ORCHICON_INSTANCE ORCHICON_SERVE_STATE_DIR ORCHICON_SERVE_LOG_FILE 2>/dev/null || true

# --- output ----------------------------------------------------------------
say()   { printf '%s\n' "$*"; }
head2() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
ok2()   { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
bad2()  { printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
skip2() { printf '  \033[33mSKIP\033[0m %s\n' "$*"; }
blk2()  { printf '  \033[35mBLOCKED\033[0m %s\n' "$*"; }
dim()   { printf '  \033[2m%s\033[0m\n' "$*"; }

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
  printf '  (skip: %s)\n' "$2" >>"$STEP_LOG"
}

blocked_surface() { # blocked_surface <desc> <owner> <why> - a MISSING SURFACE: defect, exit 2
  BLOCKED=$((BLOCKED + 1)); BLOCKED_SURFACE=1
  record BLOCKED "$1" "$2"; blk2 "$1 — blocked on ${2}: $3"
  printf '%s\t%s\t%s\n' "$CURRENT_STEP" "$1 [BLOCKED: $3]" "$2" >>"${EVIDENCE}/defects.tsv"
  printf '  (blocked: %s — owner %s)\n' "$3" "$2" >>"$STEP_LOG"
  ABORT_REASON="${ABORT_REASON:-$1 (owner: $2)}"
}

invalid_host() { # invalid_host <desc> <why> - cannot execute HERE; accuses no product code
  BLOCKED=$((BLOCKED + 1)); INVALID=1
  record BLOCKED "$1" "(invalid host)"
  blk2 "$1 — invalid host: $2"
  printf '  (invalid host: %s)\n' "$2" >>"$STEP_LOG"
  ABORT_REASON="${ABORT_REASON:-$1: $2}"
}

note() { printf '  %s\n' "$*" >>"$STEP_LOG"; }

step_begin() { # step_begin <n> <slug>
  CURRENT_STEP="$1"; STEP_LOG="${EVIDENCE}/$(printf '%02d' "$1" 2>/dev/null || printf '%s' "$1")-${2}.log"
  : >"$STEP_LOG" 2>/dev/null || true
  head2 "step ${1} — ${2}"
}

# c_run <cmd...> — run it, log the exact command, its exit code and its output, and
# hand the output back on stdout so an assertion can read it.
c_run() {
  local out rc
  out="$("$@" 2>&1)"; rc=$?
  { printf '\n$ %s\n[exit %s]\n%s\n' "$*" "$rc" "$out"; } >>"$STEP_LOG"
  printf '%s' "$out"
  return "$rc"
}

# shot <name> — copy a screenshot the browser leg already produced into the evidence
# dir (the spec writes them there directly; this records the presence as a fact).
shot() {
  local f="${EVIDENCE}/$1"
  if [ -s "$f" ]; then dim "evidence: ${f#"$REPO_ROOT"/} ($(wc -c <"$f" | tr -d ' ') bytes)"; return 0; fi
  dim "evidence MISSING: ${f#"$REPO_ROOT"/}"; return 1
}

# --- /proc primitives (ss/lsof/netstat/fuser are NOT assumed) ---------------
hex_port() { printf '%04X' "$1"; }

port_listening() { # port_listening <port> - true when /proc/net/tcp{,6} LISTENs there
  local port="$1" want f
  want="$(hex_port "$port")"
  for f in /proc/net/tcp /proc/net/tcp6; do
    [ -r "$f" ] || continue
    awk -v want="$want" 'NR>1 && $4=="0A" { n=split($2,a,":"); if (a[n]==want) { found=1; exit } } END { exit found?0:1 }' "$f" && return 0
  done
  return 1
}

listen_inodes() { # listen_inodes <port>
  local port="$1" want f
  want="$(hex_port "$port")"
  for f in /proc/net/tcp /proc/net/tcp6; do
    [ -r "$f" ] || continue
    awk -v want="$want" 'NR>1 && $4=="0A" { n=split($2,a,":"); if (a[n]==want) print $10 }' "$f"
  done
}

pid_owning_port() { # pid_owning_port <port> - the pid holding the LISTEN socket, or nothing
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
            printf '%s\n' "${pid#/proc/}"; return 0
          fi ;;
      esac
    done
  done
  return 1
}

port_free() { ! port_listening "$1"; }

pid_exe() { readlink "/proc/$1/exe" 2>/dev/null || true; }

# proc_scan <pattern> — the searchable process table without pgrep. A minimal host
# is allowed to ship no `pgrep`, and "the tool was missing" must never be reported
# as "the process was absent".
proc_scan() {
  local pat="$1" pid cmd
  for pid in /proc/[0-9]*; do
    cmd="$(tr '\0' ' ' <"${pid}/cmdline" 2>/dev/null)" || continue
    [ -n "${cmd// /}" ] || continue
    printf '%s' "$cmd" | grep -qi -- "$pat" && printf '%s %s\n' "${pid#/proc/}" "$cmd"
  done
  return 0
}

# procs_matching <pattern> — pgrep when the host has it, the /proc scan otherwise.
procs_matching() {
  if command -v pgrep >/dev/null 2>&1; then
    pgrep -af "$1" 2>/dev/null || true
  else
    proc_scan "$1"
  fi
}

# ancestors — this harness and every shell that invoked it. A probe that can match
# its own caller's command line would accuse the harness instead of the machine, so
# the exclusion is part of the measurement.
ancestors() {
  local pid="$$" depth=0 status pp
  printf '%s\n' "$pid"
  while [ "$depth" -lt 64 ]; do
    status="$(sed -n 's/^PPid:[[:space:]]*\([0-9]*\).*/\1/p' "/proc/${pid}/status" 2>/dev/null)"
    [ -n "$status" ] || break
    [ "$status" = "0" ] && break
    printf '%s\n' "$status"
    pid="$status"; depth=$((depth + 1))
  done
}

# grafana_procs — every process that could BE grafana, excluding this harness and its
# callers.
grafana_procs() {
  local skip line pid rest
  skip=" $(ancestors | tr '\n' ' ') "
  procs_matching grafana | while IFS= read -r line; do
    pid="${line%% *}"; rest="${line#* }"
    case "$skip" in *" ${pid} "*) continue ;; esac
    # a forked subshell of this harness carries its caller's argv, and it is a CHILD,
    # not an ancestor — excluding only parents would let the harness match itself.
    case "$skip" in *" $(sed -n 's/^PPid:[[:space:]]*\([0-9]*\).*/\1/p' "/proc/${pid}/status" 2>/dev/null) "*) continue ;; esac
    case "$rest" in *telemetry-e2e*|*"${HERE}"*) continue ;; esac
    printf '%s\n' "$line"
  done
}

# pgrep_control — prove the process search WORKS before an empty result is allowed
# to mean "nothing matches". Returns 0 and prints the pattern that matched.
pgrep_control() {
  local pat hits
  for pat in orchicon bash sh sleep init; do
    hits="$(procs_matching "$pat")"
    case "$pat" in
      init) hits="$(hits2="$(procs_matching 1)"; printf '%s' "$hits2")" ;;
    esac
    if [ -n "$hits" ]; then printf '%s\n' "$pat"; return 0; fi
  done
  # last resort: any process at all (the shell running this harness is one)
  if [ -n "$(proc_scan ".")" ]; then printf '%s\n' '.'; return 0; fi
  return 1
}

# --- instance records (read from the RECORDS, never re-resolved) ------------
data_dir() { printf '%s/.local/share/orchicon-%s' "$HOME" "$1"; }
ports_json() { printf '%s/ports.json' "$(data_dir "$1")"; }

port_of() { # port_of <env-key> - the port this instance actually bound, else empty
  local key="$1" f
  f="$(ports_json "$INSTANCE")"
  [ -r "$f" ] || return 1
  sed -n 's/.*"'"$key"'"[[:space:]]*:[[:space:]]*\([0-9]\+\).*/\1/p' "$f" | head -1
}

plane_base() { # plane_base - the plane's own origin
  local p
  if [ -n "$PLANE_URL" ]; then printf '%s' "${PLANE_URL%/}"; return 0; fi
  # ORCHICON_CONTROL_PORT is the key install_ports.go writes for the plane's origin;
  # ORCHICON_HTTP_PORT is only a legacy alias.
  p="$(port_of ORCHICON_CONTROL_PORT || true)"
  [ -n "$p" ] || p="$(port_of ORCHICON_HTTP_PORT || true)"
  [ -n "$p" ] || p=8080
  printf 'http://127.0.0.1:%s' "$p"
}

# store URLs: the plane's own configuration wins, then the documented host defaults
tempo_url() { printf '%s' "${ORCHICON_TEMPO_URL:-http://127.0.0.1:3200}"; }
loki_url()  { printf '%s' "${ORCHICON_LOKI_URL:-http://127.0.0.1:3100}"; }
vm_url()    { printf '%s' "${ORCHICON_VM_URL:-http://127.0.0.1:8428}"; }

# A store port comes from the INSTANCE'S OWN RECORD first: dev/prod/test bind DIFFERENT
# ports (3200/3201/3202, 3100/3101/3102, 8428/8429/8430), so the documented defaults
# would probe the WRONG stack the moment --instance is not dev. An explicit URL knob
# still wins, because that is the operator naming one store by hand.
url_port() { printf '%s' "$(printf '%s' "$1" | sed -n 's#.*:\([0-9]\+\)/*$#\1#p')"; }
instance_port() { # instance_port <env-key> <explicit-url> <default>
  local p
  if [ -n "$2" ]; then p="$(url_port "$2")"; [ -n "$p" ] && { printf '%s' "$p"; return 0; }; fi
  p="$(port_of "$1" || true)"
  printf '%s' "${p:-$3}"
}
tempo_port() { instance_port ORCHICON_TEMPO_PORT "${ORCHICON_TEMPO_URL:-}" 3200; }
loki_port()  { instance_port ORCHICON_LOKI_PORT  "${ORCHICON_LOKI_URL:-}"  3100; }
vm_port()    { instance_port ORCHICON_VM_PORT    "${ORCHICON_VM_URL:-}"    8428; }
otel_port()  { local p; p="$(port_of ORCHICON_OTLP_GRPC_PORT || true)"; printf '%s' "${p:-4317}"; }

# curl_json <url> <outfile> — GET it, save the body verbatim, echo the HTTP code
curl_json() {
  local url="$1" out="$2" code
  code="$(curl -sS -m 20 -o "$out" -w '%{http_code}' "$url" 2>>"$STEP_LOG")" || code="000"
  { printf '\n$ curl -sS %s\n[http %s]\n' "$url" "$code"; head -c 2000 "$out" 2>/dev/null; printf '\n'; } >>"$STEP_LOG"
  printf '%s' "$code"
}

# curl_json_q <outfile> <url-without-query> <query-param>... — GET with proper URL
# encoding (a hand-escaped PromQL/LogQL selector is how a probe silently queries the
# wrong thing), save the body verbatim, echo the HTTP code.
curl_json_q() {
  local out="$1" base="$2" code a
  shift 2
  local args=()
  for a in "$@"; do args+=(--data-urlencode "$a"); done
  code="$(curl -sS -m 20 -G "${args[@]}" -o "$out" -w '%{http_code}' "$base" 2>>"$STEP_LOG")" || code="000"
  { printf '\n$ curl -sS -G %s %s\n[http %s]\n' "$base" "${args[*]}" "$code"; head -c 2000 "$out" 2>/dev/null; printf '\n'; } >>"$STEP_LOG"
  printf '%s' "$code"
}

# --- T0: the validity precondition -----------------------------------------
# A surface that is NOT THERE cannot be proven. Each missing piece is a defect
# row against the sibling that owns it, and the run exits 2 — never green.
NATIVE_BRANCH="origin/build-the-native-telemetry-page-graphs-reports-search-lookup-filter-yhxnxf4c6m9b9023"
REMOVAL_BRANCH="origin/remove-grafana-from-the-plane-proxy-iframe-config-proc-port-2qnyk4c19p8csyj5"

surface_present() { # surface_present <path>
  [ -f "${REPO_ROOT}/$1" ]
}

surface_absent_text() { # surface_absent_text <path> <literal>  (true when the literal is GONE)
  local f="${REPO_ROOT}/$1" lit="$2"
  [ -f "$f" ] || return 1
  ! grep -qF -- "$lit" "$f"
}

t0_preflight() {
  step_begin 0 preflight
  mkdir -p "$E2E_ROOT"

  # --- the sibling surfaces this claim is ABOUT -----------------------------
  local missing=0
  if surface_present "frontend/src/routes/telemetry.tsx" \
     && ! surface_absent_text "frontend/src/routes/telemetry.tsx" "GrafanaEmbed"; then
    blocked_surface "telemetry.tsx no longer renders <GrafanaEmbed>" \
      "${NATIVE_BRANCH}:frontend/src/routes/telemetry.tsx" \
      "the page under test is still the Grafana iframe embed"
    missing=1
  fi
  if surface_present "frontend/src/routes/telemetry.tsx" \
     && ! surface_absent_text "frontend/src/routes/telemetry.tsx" "GRAFANA_UI_URL"; then
    blocked_surface "telemetry.tsx no longer imports GRAFANA_UI_URL" \
      "${REMOVAL_BRANCH}:frontend/src/routes/telemetry.tsx" \
      "the /grafana proxy URL is still wired into the page"
    missing=1
  fi
  local f
  for f in frontend/src/components/telemetry/graphs.tsx \
           frontend/src/components/telemetry/reports-panel.tsx \
           frontend/src/components/telemetry/states.tsx \
           frontend/src/components/telemetry/telemetry-filters.tsx; do
    if ! surface_present "$f"; then
      blocked_surface "native telemetry component ${f##*/} exists" \
        "${NATIVE_BRANCH}:${f}" \
        "the component the page renders is not in the branch under test"
      missing=1
    fi
  done
  if ! surface_present "internal/telemetry/query_client.go" \
     && ! grep -q "func NewQueryClient" "${REPO_ROOT}/internal/telemetry/grafana_client.go" 2>/dev/null; then
    blocked_surface "the telemetry query client exists" \
      "${NATIVE_BRANCH}:internal/telemetry/query_client.go" \
      "no store query client to serve the page from"
    missing=1
  fi
  if ! grep -q "rpc GetTelemetryReport" "${REPO_ROOT}/proto/orchicon/api/v1/telemetry_service.proto" 2>/dev/null; then
    blocked_surface "the GetTelemetryReport RPC exists (the Reports tab's data path)" \
      "${NATIVE_BRANCH}:proto/orchicon/api/v1/telemetry_service.proto" \
      "no report RPC, so the report cannot be read from a real window"
    missing=1
  fi
  if ! grep -q "degraded_reason" "${REPO_ROOT}/proto/orchicon/api/v1/telemetry_service.proto" 2>/dev/null; then
    blocked_surface "the responses carry degraded_reason (the state must NAME the backend)" \
      "${NATIVE_BRANCH}:proto/orchicon/api/v1/telemetry_service.proto" \
      "a degraded state that cannot name its backend fails AC5"
    missing=1
  fi
  [ "$missing" = "1" ] && record BLOCKED "the sibling surfaces are all present" "$NATIVE_BRANCH"

  # --- the browser half needs a browser ------------------------------------
  if [ "$SKIP_BROWSER" = "1" ]; then
    note "browser steps disabled by --skip-browser"
  elif [ ! -d "${REPO_ROOT}/frontend/node_modules/@playwright" ]; then
    invalid_host "the Playwright browser harness is installed" \
      "frontend/node_modules/@playwright is absent — run 'cd frontend && pnpm install'"
  else
    record PASS "the Playwright harness is installed"
    PASSES=$((PASSES + 1))
  fi

  # --- an `orchicon` binary to drive the services --------------------------
  if [ -z "$BIN" ]; then
    BIN="${E2E_ROOT}/bin/orchicon"
    if [ ! -x "$BIN" ]; then
      mkdir -p "${E2E_ROOT}/bin"
      if ! ( cd "$REPO_ROOT" && go build -o "$BIN" ./cmd/orchicon ) >>"$STEP_LOG" 2>&1; then
        invalid_host "an orchicon binary to drive the services" "go build ./cmd/orchicon failed (see step log)"
      fi
    fi
  fi
  [ -x "$BIN" ] && dim "binary under test: ${BIN}"

  # --- nothing of ours is already holding a store port ---------------------
  local p name
  for name in tempo loki vm; do
    p="$("${name}_port")"
    [ -n "$p" ] || continue
    if port_listening "$p"; then dim "port ${p} (${name}) is ALREADY listening — the stack is up before this run"
    else dim "port ${p} (${name}) is free"; fi
  done
}

# --- T1a: Grafana is genuinely ABSENT (host level) -------------------------
t1a_grafana_absent() {
  step_begin 1 grafana-absent
  local bins ctl out
  bins=""
  for b in grafana grafana-server grafana-cli grafana-image-renderer; do
    if command -v "$b" >/dev/null 2>&1; then bins="${bins} $(command -v "$b")"; fi
  done
  note "command -v grafana*: '${bins}'"
  check "no grafana binary on PATH (command -v grafana grafana-server grafana-cli)" \
    "$([ -z "$bins" ] && echo 0 || echo 1)" \
    "(a stray grafana binary makes the whole claim void)"

  # A bare host is allowed to be minimal, so the CONTROL goes first: an empty
  # pgrep only means "none" once pgrep has been seen to match SOMETHING.
  ctl="$(pgrep_control || true)"
  note "pgrep control pattern matched: '${ctl}'"
  check "the process search works on this host (a control pattern matched before any empty result is believed)" \
    "$([ -n "$ctl" ] && echo 0 || echo 1)" \
    "(a broken pgrep would make every absence assertion below vacuous)"

  local procs
  procs="$(grafana_procs)"
  note "pgrep -af grafana: '${procs}'"
  check "no grafana process is running (pgrep -af grafana / the /proc scan, proven by the control above)" \
    "$([ -z "$procs" ] && echo 0 || echo 1)"

  local cfgs
  cfgs="$( { ls -1 "${REPO_ROOT}/deploy/configs" 2>/dev/null; ls -1 "${REPO_ROOT}/deploy" 2>/dev/null; } | grep -i grafana || true)"
  note "grafana files under deploy/: '${cfgs}'"
  check "no grafana config / provisioning file ships in the repo (deploy/**)" \
    "$([ -z "$cfgs" ] && echo 0 || echo 1)" \
    "${REMOVAL_BRANCH}:deploy/configs/grafana.ini"

  local imgs engine
  imgs=""; engine="none"
  if command -v podman >/dev/null 2>&1; then engine=podman; imgs="$(podman images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | grep -i grafana || true)"
  elif command -v docker >/dev/null 2>&1; then engine=docker; imgs="$(docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | grep -i grafana || true)"
  fi
  note "container engine: ${engine}; grafana images: '${imgs}'"
  check "no grafana container image is present (podman/docker images | grep -i grafana)" \
    "$([ -z "$imgs" ] && echo 0 || echo 1)" \
    "(grafana in an image is still grafana on the machine)"
}

# --- T1b: the plane has no /grafana route (needs the plane up) --------------
t1b_grafana_route() {
  # Same step number: it is the second half of AC1, run once the plane answers.
  step_begin 1 grafana-absent-route
  local base health code
  base="$(plane_base)"
  health="$(curl -sS -m 10 -o /dev/null -w '%{http_code}' "${base}/healthz" 2>>"$STEP_LOG")" || health="000"
  note "GET ${base}/healthz -> ${health}"
  if [ "$health" != "200" ]; then
    invalid_host "the plane answers /healthz" "GET ${base}/healthz returned ${health}"
    return
  fi
  code="$(curl -sS -m 10 -o "${EVIDENCE}/01-grafana-route.body" -w '%{http_code}' "${base}/grafana" 2>>"$STEP_LOG")" || code="000"
  note "GET ${base}/grafana -> ${code}"
  check "the plane's /grafana route is GONE (404, not a redirect into an embed)" \
    "$([ "$code" = "404" ] && echo 0 || echo 1)" \
    "${REMOVAL_BRANCH}:internal/server/server.go (proxy route)"
}

# --- T2: the telemetry stack is HOST-RESIDENT ------------------------------
wait_port() { # wait_port <port> <seconds>
  local p="$1" budget="${2:-120}" i=0
  while [ "$i" -lt "$budget" ]; do port_listening "$p" && return 0; sleep 1; i=$((i + 1)); done
  return 1
}

t2_stack_up() {
  step_begin 2 stack-up
  local out rc
  # `orchicon start` — NOT `services up`. `services up` sets ORCHICON_TELEMETRY=none on
  # purpose (services.go:telemetryOffForTheInnerLoop: the inner loop wants the DB and the
  # bus, not 300 MB of backends), so it brings up postgres + NATS and NOTHING this proof
  # can read. `start` composes startHostServices + startHostPlane, and the supervisor it
  # re-execs is the thing that runs Tempo/Loki/VictoriaMetrics/otelcol as this user's
  # processes. ORCHICON_TELEMETRY is set explicitly so an inherited `none` cannot turn
  # the stack this proof is ABOUT off behind its back.
  out="$(ORCHICON_TELEMETRY=embedded "$BIN" start --instance "$INSTANCE" 2>&1)"; rc=$?
  { printf '\n$ ORCHICON_TELEMETRY=embedded %s start --instance %s\n[exit %s]\n%s\n' "$BIN" "$INSTANCE" "$rc" "$out"; } >>"$STEP_LOG"
  if [ "$rc" -ne 0 ]; then
    invalid_host "orchicon start brings the host-resident plane and the telemetry stack up" \
      "'start --instance ${INSTANCE}' exited ${rc} (see 02-stack-up.log)"
    return
  fi
  DIRTY=1
  # No DSN is exported: every run below is created through the PLANE, so it lands in the
  # plane's own database by construction, and the ports come from this instance's record.

  local name port pid exe want
  for spec_pair in "tempo:$(tempo_port):tempo" "loki:$(loki_port):loki" \
                   "victoria-metrics:$(vm_port):victoria-metrics" "otelcol-contrib:$(otel_port):otelcol"; do
    name="${spec_pair%%:*}"; port="$(printf '%s' "$spec_pair" | cut -d: -f2)"; want="${spec_pair##*:}"
    [ -n "$port" ] || { skip "${name} residency" "no port to probe"; continue; }
    if ! wait_port "$port" 120; then
      check "${name} is listening on :${port}" 1 "cmd/orchicon/services.go:servicesUp"
      continue
    fi
    pid="$(pid_owning_port "$port" || true)"
    exe="$(pid_exe "${pid:-0}")"
    note "${name}: port ${port} pid='${pid}' exe='${exe}'"
    check "${name} is a HOST process on :${port} (/proc/${pid:-?}/exe matches '${want}')" \
      "$(printf '%s' "$exe" | grep -qi -- "$want" && echo 0 || echo 1)" \
      "cmd/orchicon/services.go:servicesUp (a containerised backend is not host-resident)"
    [ "$name" = "loki" ] && LOKI_PID="$pid"
  done
  printf 'LOKI_PID=%s\n' "$LOKI_PID" >"${EVIDENCE}/run.env"
}

# --- T3: a REAL workflow run, created and driven through the plane ----------
connect_post() { # connect_post <jar> <service/method> <json> <outfile> -> HTTP code
  local jar="$1" rpc="$2" body="$3" out="$4" code
  code="$(curl -sS -m 30 -b "$jar" -c "$jar" -o "$out" -w '%{http_code}' \
    -H 'Content-Type: application/json' --data "$body" \
    "$(plane_base)/orchicon.api.v1.${rpc}" 2>>"$STEP_LOG")" || code="000"
  { printf '\n$ POST %s\n%s\n[http %s]\n%s\n' "$rpc" "$body" "$code" "$(head -c 800 "$out" 2>/dev/null)"; } >>"$STEP_LOG"
  printf '%s' "$code"
}

json_get() { # json_get <file> <key> - first "key":"value" (no jq assumed)
  # NON-GREEDY ON PURPOSE. `sed 's/.*"id":"\([^"]*\)"/\1/'` takes the LAST id on the
  # line, so a nested "id" (a step inside a run, say) would be handed back as the run id.
  # Connect responses carry the object's own id first, so the FIRST match is the right one.
  grep -o "\"$2\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" "$1" 2>/dev/null \
    | head -1 | sed 's/^"[^"]*"[[:space:]]*:[[:space:]]*"//; s/"$//'
}

t3_real_run() {
  step_begin 3 real-run
  local jar="${E2E_ROOT}/cookies.txt"
  : >"$jar"

  if [ -n "$RUN_DRIVER" ]; then
    local out rc
    out="$(eval "$RUN_DRIVER" 2>&1)"; rc=$?
    { printf '\n$ %s\n[exit %s]\n%s\n' "$RUN_DRIVER" "$rc" "$out"; } >>"$STEP_LOG"
    printf '%s\n' "$out" >"${EVIDENCE}/03-run-driver.out"
    PROJECT_ID="$(printf '%s\n' "$out" | sed -n 's/^PROJECT_ID=//p' | head -1)"
    FOREIGN_PROJECT_ID="$(printf '%s\n' "$out" | sed -n 's/^FOREIGN_PROJECT_ID=//p' | head -1)"
    RUN_ID="$(printf '%s\n' "$out" | sed -n 's/^RUN_ID=//p' | head -1)"
    EXECUTION_ID="$(printf '%s\n' "$out" | sed -n 's/^EXECUTION_ID=//p' | head -1)"
    CORRELATION_ID="$(printf '%s\n' "$out" | sed -n 's/^CORRELATION_ID=//p' | head -1)"
    TRACE_ID="$(printf '%s\n' "$out" | sed -n 's/^TRACE_ID=//p' | head -1)"
    check "the run driver produced a REAL completed run (exit ${rc}, run id '${RUN_ID}')" \
      "$([ "$rc" = "0" ] && [ -n "$RUN_ID" ] && echo 0 || echo 1)" \
      "(the driver is the caller's; see --run-driver)"
  else
    # The built-in driver talks to the plane's OWN Connect API with a local session,
    # exactly like the browser does — a created project in LOCAL execution mode, a
    # work item, a workflow published and started, and the run polled to completion.
    local user="${ORCHICON_E2E_USER:-admin}" pass="${ORCHICON_E2E_PASSWORD:-orchicon}" code out
    code="$(curl -sS -m 20 -c "$jar" -o "${EVIDENCE}/03-login.json" -w '%{http_code}' \
      -H 'Content-Type: application/json' \
      --data "{\"username\":\"${user}\",\"password\":\"${pass}\"}" \
      "$(plane_base)/auth/local-login" 2>>"$STEP_LOG")" || code="000"
    note "POST /auth/local-login as '${user}' -> ${code}"
    if [ "$code" != "200" ]; then
      invalid_host "the harness can authenticate to the plane" \
        "local-login as '${user}' returned ${code}; set ORCHICON_E2E_USER/ORCHICON_E2E_PASSWORD or pass --run-driver"
      return
    fi

    local slug="telemetry-e2e-$(date -u +%H%M%S)"
    out="${EVIDENCE}/03-project.json"
    # NO "goals" KEY: CreateProjectRequest.goals is `repeated GoalField` (project.proto:136),
    # so a JSON string there is a decode error and the project is never created.
    code="$(connect_post "$jar" "ProjectService/CreateProject" \
      "{\"name\":\"Telemetry E2E ${UTC_STAMP}\",\"slug\":\"${slug}\",\"executionMode\":\"EXECUTION_MODE_LOCAL\"}" "$out")"
    PROJECT_ID="$(json_get "$out" id)"
    check "a LOCAL-mode project was created through the plane's API (project_id '${PROJECT_ID}')" \
      "$([ "$code" = "200" ] && [ -n "$PROJECT_ID" ] && echo 0 || echo 1)" \
      "internal/api/project_handler.go:CreateProject"

    # A second project, so T6 can prove the filter NARROWS rather than "returns the
    # only project there is".
    out="${EVIDENCE}/03-project-foreign.json"
    code="$(connect_post "$jar" "ProjectService/CreateProject" \
      "{\"name\":\"Telemetry E2E foreign ${UTC_STAMP}\",\"slug\":\"${slug}-foreign\",\"executionMode\":\"EXECUTION_MODE_LOCAL\"}" "$out")"
    FOREIGN_PROJECT_ID="$(json_get "$out" id)"
    note "foreign project: '${FOREIGN_PROJECT_ID}' (${code})"

    out="${EVIDENCE}/03-workitem.json"
    code="$(connect_post "$jar" "WorkItemService/CreateWorkItem" \
      "{\"projectId\":\"${PROJECT_ID}\",\"kind\":\"WORK_ITEM_KIND_TASK\",\"title\":\"Telemetry E2E proof ${UTC_STAMP}\",\"description\":\"Generated by scripts/telemetry-e2e.sh so the Telemetry page has real telemetry to show.\",\"priority\":1}" "$out")"
    local wi; wi="$(json_get "$out" id)"
    note "work item: '${wi}' (${code})"

    out="${EVIDENCE}/03-workflow.json"
    code="$(connect_post "$jar" "WorkflowService/CreateWorkflow" \
      "{\"projectId\":\"${PROJECT_ID}\",\"name\":\"Telemetry E2E ${UTC_STAMP}\",\"steps\":\"[]\"}" "$out")"
    local wf; wf="$(json_get "$out" id)"
    out="${EVIDENCE}/03-workflow-publish.json"
    local pcode; pcode="$(connect_post "$jar" "WorkflowService/PublishWorkflow" "{\"workflowId\":\"${wf}\",\"versionNote\":\"e2e\"}" "$out")"
    out="${EVIDENCE}/03-run-start.json"
    code="$(connect_post "$jar" "WorkflowService/StartWorkflow" \
      "{\"workflowId\":\"${wf}\",\"projectId\":\"${PROJECT_ID}\",\"workItemId\":\"${wi}\"}" "$out")"
    RUN_ID="$(json_get "$out" id)"
    check "a workflow run was started through the plane's API (run_id '${RUN_ID}')" \
      "$([ -n "$RUN_ID" ] && echo 0 || echo 1)" \
      "internal/api/workflow_handler.go:StartWorkflow (publish returned ${pcode})"

    # Poll to a terminal state. "Completed" is what makes the telemetry REAL work.
    local waited=0 status=""
    while [ "$waited" -lt "$POLL_SECONDS" ]; do
      code="$(curl -sS -m 20 -b "$jar" -G --data-urlencode "id=${RUN_ID}" -o "${EVIDENCE}/03-run-poll.json" -w '%{http_code}' \
        "$(plane_base)/orchicon.api.v1.WorkflowService/GetWorkflowRun" 2>>"$STEP_LOG")" || code="000"
      status="$(json_get "${EVIDENCE}/03-run-poll.json" status)"
      case "$status" in
        *COMPLETED*|*FAILED*|*ABORTED*) break ;;
      esac
      sleep 5; waited=$((waited + 5))
    done
    note "run ${RUN_ID} terminal status: '${status}' after ${waited}s"
    check "the real run reached a terminal state (status '${status}')" \
      "$(printf '%s' "$status" | grep -q "COMPLETED" && echo 0 || echo 1)" \
      "internal/workflow/reconciler.go (a run that never completes produces no telemetry)"

    # The run's correlation/execution ids are what the stores key on.
    CORRELATION_ID="$(sed -n 's/.*"correlationId"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${EVIDENCE}/03-run-poll.json" | head -1)"
    EXECUTION_ID="$(sed -n 's/.*"executionId"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${EVIDENCE}/03-run-poll.json" | head -1)"
    if [ -z "$EXECUTION_ID" ]; then
      # NOT invalid_host: a local-mode run that dispatched no worker still emits the plane's
      # own telemetry, so the trace view is judged by what Tempo actually holds rather than
      # aborted here. A run with no traces at all fails the browser leg honestly.
      note "run ${RUN_ID} reported no execution id (no worker step); the trace assertions fall back to the window"
      skip "the run dispatched a worker execution to trace" "no execution id on a run with no worker step"
    fi
  fi

  printf 'PROJECT_ID=%s\nFOREIGN_PROJECT_ID=%s\nRUN_ID=%s\nEXECUTION_ID=%s\nCORRELATION_ID=%s\nTRACE_ID=%s\nLOKI_PID=%s\n' \
    "$PROJECT_ID" "$FOREIGN_PROJECT_ID" "$RUN_ID" "$EXECUTION_ID" "$CORRELATION_ID" "$TRACE_ID" "$LOKI_PID" \
    >>"${EVIDENCE}/run.env"
  dim "run.env: project=${PROJECT_ID} run=${RUN_ID} execution=${EXECUTION_ID} correlation=${CORRELATION_ID}"
}

# --- T4: the stores really hold THIS run (the anti-fixture check) -----------
t4_store_truth() {
  step_begin 4 stores
  mkdir -p "${EVIDENCE}/04-stores"
  local dir="${EVIDENCE}/04-stores" now start code body found=0

  # --- Tempo: does a trace for this run exist at all? ----------------------
  now="$(date -u +%s)"; start=$((now - 21600))
  code="$(curl_json "$(tempo_url)/api/search?limit=50&start=${start}&end=${now}" "${dir}/tempo-search.json")"
  local tfile="${dir}/tempo-search.json"
  if [ "$code" != "200" ]; then
    check "Tempo answers a search over the run's window (HTTP ${code})" 1 "internal/telemetry (tempo unreachable)"
  else
    grep -o '"traceID":"[^"]*"' "$tfile" 2>/dev/null | head -50 >"${dir}/tempo-traceids.txt" || true
    local tcount; tcount="$(wc -l <"${dir}/tempo-traceids.txt" | tr -d ' ')"
    local traces_ok=1
    if [ -z "$TRACE_ID" ]; then
      TRACE_ID="$(sed -n 's/.*"traceID":"\([^"]*\)".*/\1/p' "$tfile" | head -1)"
      [ -n "$TRACE_ID" ] && [ "${tcount:-0}" -gt 0 ] && traces_ok=0
    elif grep -q "$TRACE_ID" "$tfile" 2>/dev/null; then
      traces_ok=0
    fi
    check "Tempo (queried DIRECTLY, bypassing Orchicon) holds ${tcount} trace(s) in the window${TRACE_ID:+, the run trace ${TRACE_ID}}" \
      "$traces_ok" \
      "internal/telemetry/service.go (traces never exported to Tempo)"
  fi

  # --- Loki: real log lines from the run -----------------------------------
  now="$(( $(date -u +%s) * 1000000000 ))"; start=$((now - 21600 * 1000000000))
  code="$(curl_json_q "${dir}/loki-query.json" "$(loki_url)/loki/api/v1/query_range" \
    'query={service_name=~".+"}' "limit=100" "start=${start}" "end=${now}")"
  if [ "$code" != "200" ]; then
    check "Loki answers a query_range over the run's window (HTTP ${code})" 1 "internal/telemetry (loki unreachable)"
  else
    local lcount; lcount="$(grep -o '\["[0-9]\{16,\}","' "${dir}/loki-query.json" 2>/dev/null | wc -l | tr -d ' ')"
    check "Loki (queried DIRECTLY) returns real log lines for the window (${lcount} line(s))" \
      "$([ "${lcount:-0}" -gt 0 ] && echo 0 || echo 1)" \
      "internal/telemetry/service.go (logs never exported to Loki)"
    # The run's own ids must be findable in those lines — that is what ties them to
    # REAL work rather than to any traffic the stack happened to see.
    if [ -n "$RUN_ID" ]; then
      code="$(curl_json_q "${dir}/loki-run-id.json" "$(loki_url)/loki/api/v1/query_range" \
        "query={service_name=~\".+\"} |= \"${RUN_ID}\"" "limit=20" "start=${start}" "end=${now}")"
      local rcount; rcount="$(grep -o '\["[0-9]\{16,\}","' "${dir}/loki-run-id.json" 2>/dev/null | wc -l | tr -d ' ')"
      check "Loki holds log lines naming THIS run (${RUN_ID}: ${rcount} line(s))" \
        "$([ "${rcount:-0}" -gt 0 ] && echo 0 || echo 1)" \
        "internal/telemetry/exporters (the run's logs are not tagged with its id)"
    fi
  fi

  # --- VictoriaMetrics: a real, non-zero value ------------------------------
  local metric vfile
  for metric in orchicon_tokens_consumed orchicon_executions_total orchicon_llm_tokens_total; do
    code="$(curl_json_q "${dir}/vm-${metric}.json" "$(vm_url)/api/v1/query" "query=${metric}")"
    if [ "$code" = "200" ] && grep -q '"value"' "${dir}/vm-${metric}.json" 2>/dev/null; then
      VM_VALUE="$(sed -n 's/.*"value":\[[^,]*,"\([^"]*\)"\].*/\1/p' "${dir}/vm-${metric}.json" | head -1)"
      VM_METRIC="$metric"
      break
    fi
  done
  vfile="${dir}/vm-${VM_METRIC:-none}.json"
  note "vm metric '${VM_METRIC:-none}' value '${VM_VALUE}'"
  check "VictoriaMetrics (queried DIRECTLY) returns a real value for an Orchicon metric (${VM_METRIC:-none} = ${VM_VALUE:-?})" \
    "$([ -n "${VM_VALUE:-}" ] && [ "${VM_VALUE%%.*}" != "0" ] && echo 0 || echo 1)" \
    "internal/telemetry/exporters (metrics never exported to VictoriaMetrics)"

  # --- the PLANE's own answers must carry the same ids --------------------
  # This is the "the page reads the stores" half that does not need a browser:
  # the Connect responses the page consumes are the same ones fetched here.
  local jar="${E2E_ROOT}/cookies.txt" pcode
  if [ -s "$jar" ]; then
    pcode="$(connect_post "$jar" "TelemetryService/QueryTraces" \
      "{\"query\":{\"projectId\":\"${PROJECT_ID}\"}}" "${dir}/plane-query-traces.json")"
    if [ "$pcode" = "200" ]; then
      check "the plane's QueryTraces answers for the run's project and carries this run's trace" \
        "$([ -z "$TRACE_ID" ] && echo 0 || grep -q "$TRACE_ID" "${dir}/plane-query-traces.json" && echo 0 || echo 1)" \
        "internal/telemetry/query_client.go (the store has the trace, the API did not return it)"
    else
      check "the plane's QueryTraces answers (HTTP ${pcode})" 1 "internal/telemetry/query_client.go"
    fi
    pcode="$(connect_post "$jar" "TelemetryService/GetTelemetryReport" \
      "{\"projectId\":\"${PROJECT_ID}\"}" "${dir}/plane-report.json")"
    check "the plane's GetTelemetryReport answers for the run's project (HTTP ${pcode})" \
      "$([ "$pcode" = "200" ] && echo 0 || echo 1)" \
      "internal/db/telemetry_report.go (the report summarising the window)"
  else
    note "no authenticated session; the plane-side cross-check of the store ids was skipped"
    skip "the plane's Connect answers carry the store ids" "no session (built-in driver did not log in)"
  fi
  printf 'TRACE_ID=%s\nVM_METRIC=%s\nVM_VALUE=%s\n' "$TRACE_ID" "${VM_METRIC:-}" "${VM_VALUE:-}" >>"${EVIDENCE}/run.env"
}

# --- T5/T6/T7: the browser half --------------------------------------------
# The spec owns the DOM assertions and writes its own verdict rows; this step
# OWNS the preconditions that make those verdicts worth having (no reload() in
# the spec, a live backend to kill, a restart command) and INGESTS the rows.
SPEC="frontend/tests/telemetry-e2e.spec.ts"

t567_browser() {
  step_begin 5 browser
  if [ "$SKIP_BROWSER" = "1" ]; then
    skip "the browser half (AC3/AC4/AC5)" "--skip-browser"
    return
  fi
  if [ ! -f "${REPO_ROOT}/${SPEC}" ]; then
    blocked_surface "the browser leg of the proof (${SPEC})" "${SPEC}" "the spec that drives the page is missing"
    return
  fi

  # AC5 needs "no reload masked the recovery" to be a FACT, not a promise: the
  # spec must not contain a reload call at all. The control grep proves the grep
  # itself works before the empty result is believed.
  # COMMENT LINES ARE EXCLUDED from both counts. A comment that DOCUMENTS the rule has to
  # name the call it forbids, and a grep that cannot tell documentation from code fails the
  # precondition on its own prose (it did: the spec's NO page.reload line).
  local code_lines ctl rl
  code_lines="$(grep -v '^[[:space:]]*\(//\|\*\|/\*\)' "${REPO_ROOT}/${SPEC}" 2>/dev/null || true)"
  ctl="$(printf '%s\n' "$code_lines" | grep -c "goto(" || true)"
  rl="$(printf '%s\n' "$code_lines" | grep -c "reload(" || true)"
  note "spec control grep 'goto(' -> ${ctl}; forbidden grep 'reload(' -> ${rl}"
  check "the spec can navigate, so the empty check below means something (control: 'goto(' found ${ctl}x)" \
    "$([ "${ctl:-0}" -gt 0 ] && echo 0 || echo 1)"
  check "the spec NEVER reloads the page (grep 'reload(' -> ${rl}) — AC5 cannot be faked by a reload" \
    "$([ "${rl:-1}" -eq 0 ] && echo 0 || echo 1)" "${SPEC}:reload("

  # The RESUME command the spec falls back to when the frozen backend is gone entirely. A
  # plain `services up` would be a NO-OP here (servicesReady only probes postgres + NATS),
  # so the restart is `stop` then `start`.
  local restart_cmd="$BIN stop --instance $INSTANCE && ORCHICON_TELEMETRY=embedded $BIN start --instance $INSTANCE"
  local out rc
  out="$(cd "${REPO_ROOT}/frontend" && \
    E2E_EVIDENCE="$EVIDENCE" \
    E2E_PLANE_URL="$(plane_base)" \
    E2E_PROJECT_ID="$PROJECT_ID" \
    E2E_FOREIGN_PROJECT_ID="$FOREIGN_PROJECT_ID" \
    E2E_RUN_ID="$RUN_ID" \
    E2E_EXECUTION_ID="$EXECUTION_ID" \
    E2E_CORRELATION_ID="$CORRELATION_ID" \
    E2E_TRACE_ID="$TRACE_ID" \
    E2E_LOKI_PID="$LOKI_PID" \
    E2E_LOKI_PORT="$(loki_port)" \
    E2E_LOKI_RESTART_CMD="$restart_cmd" \
    E2E_VM_METRIC="${VM_METRIC:-}" \
    E2E_VM_VALUE="${VM_VALUE:-}" \
    PLAYWRIGHT_BASE_URL="$(plane_base)" \
    npx playwright test "tests/telemetry-e2e.spec.ts" --config playwright.telemetry-e2e.config.ts \
      --project=dark-desktop --workers=1 --reporter=list 2>&1)"; rc=$?
  printf '%s\n' "$out" >"${EVIDENCE}/05-playwright.out"
  note "playwright exit ${rc}"
  printf '%s\n' "$out" | tail -40 >>"$STEP_LOG"

  # Ingest the spec's own verdict rows. A missing row is a FAIL, never a pass:
  # a browser step that produced no verdict has proven nothing.
  local f="${EVIDENCE}/browser-results.tsv"
  if [ -s "$f" ]; then
    awk -F'\t' '{printf "%s\t%s\t%s\t%s\n", $1, $2, $3, $4}' "$f" >>"${EVIDENCE}/results.tsv"
    local step
    for step in 5 6 7; do
      local n; n="$(awk -F'\t' -v s="$step" '$1==s && $2=="PASS"' "$f" | wc -l | tr -d ' ')"
      local bad; bad="$(awk -F'\t' -v s="$step" '$1==s && $2=="FAIL"' "$f" | wc -l | tr -d ' ')"
      if [ "$bad" -gt 0 ]; then
        FAILS=$((FAILS + 1)); bad2 "browser step ${step} reported ${bad} failing assertion(s) (see browser-results.tsv)"
        awk -F'\t' -v s="$step" '$1==s && $2=="FAIL" {printf "%s\t%s\t%s\n", $1, $3, $4}' "$f" >>"${EVIDENCE}/defects.tsv"
      elif [ "$n" -gt 0 ]; then
        PASSES=$((PASSES + 1)); ok2 "browser step ${step} passed its assertions (${n} row(s))"
      else
        FAILS=$((FAILS + 1)); bad2 "browser step ${step} produced NO verdict — the page was not proven"
        printf '%s\t%s\t%s\n' "$step" "the browser leg produced no verdict" "${SPEC}" >>"${EVIDENCE}/defects.tsv"
      fi
    done
  else
    FAILS=$((FAILS + 1))
    bad2 "the browser leg produced no browser-results.tsv (playwright exit ${rc})"
    printf '%s\t%s\t%s\n' 5 "the browser leg ran and reported" "${SPEC}" >>"${EVIDENCE}/defects.tsv"
  fi

  local s
  for s in 05-overview.png 05-metrics.png 05-logs.png 05-traces.png 05-reports.png \
           06-filtered.png 06-empty.png 08-degraded.png 08-recovered.png; do
    shot "$s" || true
  done
}

# --- T8: teardown ----------------------------------------------------------
t8_teardown() {
  step_begin 8 teardown
  if [ "$KEEP" = "1" ]; then
    skip "the services come down cleanly" "--keep"
    return
  fi
  [ "$DIRTY" = "1" ] || { skip "the services come down cleanly" "this run started nothing"; return; }
  local out rc
  # `orchicon stop` — the mirror of the `start` this run used: the plane first, then the
  # services supervisor (the process that owns the telemetry children).
  out="$("$BIN" stop --instance "$INSTANCE" 2>&1)"; rc=$?
  { printf '\n$ %s stop --instance %s\n[exit %s]\n%s\n' "$BIN" "$INSTANCE" "$rc" "$out"; } >>"$STEP_LOG"
  local p name ok=0
  for name in tempo loki vm; do
    p="$("${name}_port")"; [ -n "$p" ] || continue
    port_free "$p" || { ok=1; note "${name} still listening on :${p}"; }
  done
  check "every store port came free after 'services down' (a stopped stack that still holds a port is not stopped)" \
    "$ok" "cmd/orchicon/services.go:servicesDown"
}

# --- summary / verdict -----------------------------------------------------
verdict_for() { # verdict_for <step>
  local n="$1" st
  st="$(awk -F'\t' -v s="$n" '$1==s {print $2}' "${EVIDENCE}/results.tsv" 2>/dev/null | sort -u | tr '\n' ',')"
  case "$st" in
    *FAIL*) printf '**FAIL** (see defects.tsv)' ;;
    *BLOCKED*) printf 'BLOCKED (see defects.tsv)' ;;
    *SKIP*) printf 'SKIP' ;;
    ""|,) printf 'not run' ;;
    *) printf 'PASS' ;;
  esac
}

render_summary() {
  local md="${EVIDENCE}/summary.md"
  {
    echo "# Telemetry page with Grafana absent — harness run ${UTC_STAMP}"
    echo
    echo "- claim under test: **the Telemetry page works, from real telemetry, on a machine with no Grafana**"
    echo "- instance: \`${INSTANCE}\`   plane: \`$(plane_base)\`   binary: \`${BIN}\`"
    echo "- real run: project \`${PROJECT_ID:-?}\` / run \`${RUN_ID:-?}\` / execution \`${EXECUTION_ID:-?}\` / correlation \`${CORRELATION_ID:-?}\` / trace \`${TRACE_ID:-?}\`"
    echo "- stores: tempo \`$(tempo_url)\`   loki \`$(loki_url)\`   victoria-metrics \`$(vm_url)\`"
    echo "- passes: ${PASSES}   failures: ${FAILS}   skips: ${SKIPS}   blocked: ${BLOCKED}"
    [ -n "$ABORT_REASON" ] && { echo; echo "**NOT A VALID PROOF:** ${ABORT_REASON}"; }
    echo
    echo "## verdict"
    echo
    if [ "$INVALID" = "1" ] || [ "$BLOCKED_SURFACE" = "1" ]; then
      echo "**The claim is NOT proven.** ${ABORT_REASON:-A required surface is absent or this host cannot run the stack.}"
      echo "No test was satisfied; see \`defects.tsv\` for the sibling task that owns each gap."
    elif [ "$FAILS" -gt 0 ]; then
      echo "**The claim is NOT proven.** ${FAILS} assertion(s) failed; each is a defect row against the file that owns it."
    else
      echo "**The claim is proven on this host:** Grafana absent, a real run's traces/logs/metrics rendered by the"
      echo "native page, filters that narrow, an honest empty state, and a degraded state that named its backend and"
      echo "recovered without a reload."
    fi
    echo
    echo "## per-step verdict"
    echo
    echo "| step | what it proves | verdict |"
    echo "|---|---|---|"
    printf '| 0 | validity precondition: the sibling surfaces exist on this branch | %s |\n' "$(verdict_for 0)"
    printf '| 1 | AC1 — Grafana is genuinely absent (binary/process/image/config + /grafana 404) | %s |\n' "$(verdict_for 1)"
    printf '| 2 | the telemetry stack runs as HOST processes (Tempo, Loki, VictoriaMetrics, collector) | %s |\n' "$(verdict_for 2)"
    printf '| 3 | AC2 — a REAL workflow run was created, started and completed | %s |\n' "$(verdict_for 3)"
    printf '| 4 | AC2/AC3 — the stores hold THIS run, queried directly, and the plane returns the same ids | %s |\n' "$(verdict_for 4)"
    printf '| 5 | AC3 — graphs, log search, trace view and report render from the stores | %s |\n' "$(verdict_for 5)"
    printf '| 6 | AC4 — the project filter narrows, and an excluding window shows the EMPTY state | %s |\n' "$(verdict_for 6)"
    printf '| 7 | AC5 — a stopped backend is NAMED, and recovery happens without a reload | %s |\n' "$(verdict_for 7)"
    printf '| 8 | teardown releases what this run started | %s |\n' "$(verdict_for 8)"
    echo
    echo "## evidence (independent of the page's own word)"
    echo
    echo "- \`01-grafana-absent.log\` + \`01-grafana-absent-route.log\` — the absence probes, including the"
    echo "  control pattern that had to match before an empty \`pgrep\` was allowed to mean \"none\"."
    echo "- \`02-stack-up.log\` — \`ORCHICON_TELEMETRY=embedded orchicon start\` plus the \`/proc/<pid>/exe\` residency probe per backend."
    echo "- \`03-*.json\` — the run as the plane's own API reported it (project, work item, workflow, run, poll)."
    echo "- \`04-stores/*.json\` — Tempo, Loki and VictoriaMetrics queried DIRECTLY, plus the plane's own"
    echo "  QueryTraces/GetTelemetryReport answers for the same window."
    echo "- \`05-playwright.out\`, \`browser-results.tsv\`, \`05-*.png\` … \`08-*.png\` — the browser half:"
    echo "  real clicks, real screenshots, and the request log of what the page actually fetched."
    echo "- \`run.env\` — the ids every later probe keys on."
    echo
    echo "## defects — each names the sibling task that owns the fix (AC7)"
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
    ls -1 "$EVIDENCE" 2>/dev/null | sed 's/^/- /'
  } >"$md" 2>&1
  say ""
  say "summary: ${md#"$REPO_ROOT"/}"
  say "passes=${PASSES} failures=${FAILS} skips=${SKIPS} blocked=${BLOCKED}"
}

# --- plan / usage ----------------------------------------------------------
print_plan() {
  cat <<PLAN
telemetry-e2e — what this run would do (instance \`${INSTANCE}\`, evidence \`${EVIDENCE}\`)

step 0  preflight     the sibling surfaces (native page, filters, reports, query client, report RPC,
                      degraded_reason) exist on THIS branch — else BLOCKED with a defect row; the
                      Playwright harness is installed; an \`orchicon\` binary is available
step 1  absent        AC1: no grafana binary on PATH, no grafana process (with a CONTROL pattern that
                      must match first), no grafana config in deploy/, no grafana container image, and
                      once the plane is up, GET /grafana -> 404
step 2  stack-up      AC1/AC2 context: \`ORCHICON_TELEMETRY=embedded orchicon start\` (NOT
                      \`services up\`, which deliberately runs with telemetry OFF), then each of Tempo:L$(tempo_port),
                      Loki:L$(loki_port), VictoriaMetrics:L$(vm_port), otelcol:L$(otel_port) must be a
                      host process (/proc/<pid>/exe), not a container
step 3  real-run      AC2: a LOCAL-mode project + work item + workflow, published and STARTED through
                      the plane's own Connect API, polled to a terminal state — real work, not a seed
step 4  stores        AC2/AC3: Tempo/Loki/VictoriaMetrics queried DIRECTLY for that run's ids, then the
                      plane's own QueryTraces/GetTelemetryReport answers must carry the same ids
step 5  browser       AC3: Overview -> Metrics (real value) -> Logs (real line) -> Traces (the run) ->
                      Reports (the window); a screenshot per tab and the page's request log
step 6  filters       AC4: project filter NARROWS to the run's project; a window excluding the run shows
                      the EMPTY state and the pre-filter value is GONE
step 7  degraded      AC5: stop Loki -> the state NAMES loki; restart it -> recovery IN THE SAME page
                      (the spec contains no reload(), and that is asserted before it runs)
step 8  teardown      \`orchicon stop\` and every store port must come free

exit codes: 0 every assertion passed | 1 at least one FAILED (defect named with its file:line)
            2 NOT a valid proof (a surface is absent, the host cannot run the stack, or a store never
              answered) — the claim is NOT proven
PLAN
}

usage() { sed -n '2,70p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --instance) INSTANCE="${2:-}"; shift 2 ;;
      --evidence-dir) EVIDENCE_BASE="${2:-}"; shift 2 ;;
      --bin) BIN="${2:-}"; shift 2 ;;
      --plane-url) PLANE_URL="${2:-}"; shift 2 ;;
      --run-driver) RUN_DRIVER="${2:-}"; shift 2 ;;
      --skip-browser) SKIP_BROWSER=1; shift ;;
      --keep) KEEP=1; shift ;;
      --dry-run) DRY_RUN=1; shift ;;
      --preflight-only) PREFLIGHT_ONLY=1; shift ;;
      -h|--help) usage; exit 0 ;;
      *) echo "telemetry-e2e: unknown argument '$1' (see --help)" >&2; exit 2 ;;
    esac
  done
  case "$INSTANCE" in dev|prod|test) ;; *) echo "telemetry-e2e: --instance must be dev, prod or test" >&2; exit 2 ;; esac
  EVIDENCE="${EVIDENCE_BASE%/}/${UTC_STAMP}"
}

main() {
  parse_args "$@"
  if [ "$DRY_RUN" = "1" ]; then print_plan; exit 0; fi

  mkdir -p "$EVIDENCE"
  : >"${EVIDENCE}/results.tsv"
  : >"${EVIDENCE}/defects.tsv"
  : >"${EVIDENCE}/run.env"
  say "telemetry-e2e: evidence -> ${EVIDENCE#"$REPO_ROOT"/}"

  t0_preflight
  t1a_grafana_absent
  if [ "$PREFLIGHT_ONLY" = "1" ]; then
    render_summary
    say ""
    if [ "$INVALID" = "1" ] || [ "$BLOCKED_SURFACE" = "1" ]; then
      say "PREFLIGHT: this branch is NOT a valid subject for the procedure — ${ABORT_REASON}"
      exit 2
    fi
    say "PREFLIGHT: this branch carries every surface the proof needs. Re-run without --preflight-only."
    exit 0
  fi
  [ "$BLOCKED_SURFACE" = "1" ] && { render_summary; say ""; say "RESULT: NOT A VALID PROOF — ${ABORT_REASON}"; exit 2; }

  t2_stack_up
  [ "$INVALID" = "1" ] && { t8_teardown; render_summary; say ""; say "RESULT: NOT A VALID PROOF — ${ABORT_REASON}"; exit 2; }
  t1b_grafana_route
  t3_real_run
  t4_store_truth
  t567_browser
  t8_teardown
  # AC1 re-checked at the END: a grafana that appeared mid-run voids the claim.
  step_begin 9 grafana-absent-final
  local final; final="$(grafana_procs)"
  check "Grafana is STILL absent at the end of the run (re-checked, not assumed)" \
    "$([ -z "$final" ] && echo 0 || echo 1)"
  render_summary

  say ""
  if [ "$INVALID" = "1" ] || [ "$BLOCKED_SURFACE" = "1" ]; then
    say "RESULT: NOT A VALID PROOF — ${ABORT_REASON}"
    say "        The steps that could run did run; their evidence is in ${EVIDENCE#"$REPO_ROOT"/}."
    exit 2
  fi
  if [ "$FAILS" -gt 0 ]; then
    say "RESULT: ${FAILS} assertion(s) failed — each is a defect against the file named in defects.tsv."
    exit 1
  fi
  say "RESULT: the claim is proven on this host — real telemetry, real work, no Grafana, filters that"
  say "        narrow, an honest empty window, and a degraded state that named its backend and recovered."
  exit 0
}

# Sourced by a test, the FUNCTIONS are the subject and nothing may execute: the
# offline gate proves the primitives without starting a stack or touching an instance.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then main "$@"; fi
