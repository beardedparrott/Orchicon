#!/usr/bin/env bash
# ============================================================================
# E2E acceptance harness for the RUNTIME-CONTAINER claim:
# the SAME workflow runs with NO engine present, then runs INSIDE a container
# after opting the project in.
#
# WHY THIS EXISTS. Every component of this feature can pass its own test while
# the claim itself is unreachable: a container that is created but never
# dispatched into, an opt-in that does not change where work runs, a
# provisioning step that reports success over a broken engine. Only walking the
# whole path — local run on an engine-free host, opt in, the SAME workflow in a
# container, a pristine second run, warm reuse, a custom image, the image gate,
# and the all-native path — puts those together. This is the acceptance
# procedure, not a component test.
#
# WHAT IT PROVES (a real machine, with NO container engine at step 0).
#   1. A workflow completes with the project in `local` mode and ZERO engine
#      invocations — on a host where no engine exists at all.
#   2. Opting the project into `runtime` provisions podman VISIBLY (install
#      command printed, six rungs, verify-before-ready) and `/enginez` reports
#      availability only once the engine actually works.
#   3. The SAME workflow then runs INSIDE a container: the step's own hostname
#      equals the container's hostname and differs from the host's.
#   4. State created in run A is ABSENT in run B on the same environment.
#   5. A second run reuses the pooled container; a changed host input does not.
#   6. A custom image builds with a streaming log, re-deploys idempotently, and
#      a run using it starts.
#   7. The image gate REJECTS a non-conforming image — proven by attempting one.
#   8. An all-native workflow creates NO container.
#
# WHAT IS REAL HERE. The plane, Postgres, NATS, the engine, the containers, the
# pool, the build stream and the gate are all real. Every assertion also records
# INDEPENDENT evidence — the engine's own `podman ps` / `podman images`, the
# container's own filesystem, an in-run hostname — beside whatever the control
# plane reports. The control plane's own report is never the proof.
#
# WHAT THIS HARNESS REFUSES TO CALL A PASS. An assertion that reads the CONTENT
# of a capture is gated (`gated_absent`/`gated_contains`): an empty capture is
# SKIP, never PASS, because "no container exists" is trivially true of a capture
# that failed to run. scripts/tests/runtime-e2e-assertions/run.sh holds this
# harness to that rule offline.
#
# USAGE
#   scripts/runtime-e2e.sh [options]
#     --instance dev|prod      instance to drive (default dev)
#     --evidence-dir DIR       evidence root (default qa-evidence/runtime-e2e/<UTC>)
#     --engine BIN             override ${ORCHICON_ENGINE} (default podman)
#     --workflow-id ID         workflow to run; default: create a one-step
#                              all-native workflow in the project
#     --base-image REF         image the gate must accept (default: the stock base)
#     --nonconforming-image R  image the gate must REJECT (step 7)
#     --pg-port / --nats-port / --control-port N   port overrides
#     --keep                   leave the plane and services running on exit
#     --preflight-only         run step 0 only (validity precondition), then stop
#     --dry-run                print the plan for every step, execute nothing
#     -h|--help
#
# EXIT CONTRACT
#   0  every step passed
#   1  at least one step failed (each FAIL names the file:line that owns it;
#      this item FILES defects against its siblings, it never patches them)
#   2  the run is not a valid proof of the claim (an engine was already present
#      at step 0, the tree is not a checkout, or a required port is held)
#
# This is a SESSION DRIVER, not an offline gate: it installs an engine, pulls
# images and binds real ports. `scripts/tests/` stays reserved for offline
# gates, and this is deliberately NOT part of `make ci`.
# ============================================================================
set -uo pipefail
# shellcheck disable=SC2155  # local x="$(...)" is deliberate: this script is -e-free

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/.." && pwd)"
BIN="${REPO_ROOT}/bin"

UTC_STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
MARKER="RUNTIME-E2E-${UTC_STAMP}-$$"
INSTANCE="dev"
EVIDENCE=""
KEEP=0
DRY_RUN=0
PREFLIGHT_ONLY=0
WORKFLOW_ID=""
IMAGE_ID=""
CUSTOM_IMAGE_REF=""
BASE_IMAGE="${ORCHICON_RUNTIME_IMAGE:-orchicon-runtime:local}"
NONCONFORMING_IMAGE="${ORCHICON_E2E_NONCONFORMING_IMAGE:-docker.io/library/alpine:3.20}"
ENGINE_OVERRIDE=""
PG_OVERRIDE=""
NATS_OVERRIDE=""
CTRL_OVERRIDE=""

PASSES=0
FAILS=0
SKIPS=0
ABORT_REASON=""
CURRENT_STEP="0"
RUN_ID=""
PROJECT_ID=""
PROJECT_SLUG="runtime-e2e-${UTC_STAMP}"
CONTAINER_NAME=""

# The harness's own environment must not decide any case: a shell that launched
# a plane exports its DSN/instance/state dir, and a parent make hands down
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

# --- content gates: an absence in an EMPTY capture is not evidence ----------
# The no-engine acceptance harness reported AC3 as verified on a plane serving no
# UI, because "the marker is NOT in the capture" is trivially true of a 0-byte
# capture. The same hole is wider here: "no container was created", "the gate
# rejected it", "the second run reuses the pool" are ALL absence-shaped claims,
# and an engine capture that failed to run would manufacture a green proof of the
# very thing this item exists to disprove. So every assertion reading capture
# CONTENT goes through these: bytes get the real check, emptiness is SKIP.
# scripts/tests/runtime-e2e-assertions/run.sh holds the harness to it.
gated_absent() { # gated_absent <desc> <capture> <needle> <why-if-empty> [owner]
  local desc="$1" file="$2" needle="$3" why="$4" owner="${5:-}"
  if [ -s "$file" ]; then check_absent "$desc" "$file" "$needle" "$owner"
  else skip "$desc" "$why"; fi
}

gated_contains() { # gated_contains <desc> <capture> <needle> <why-if-empty> [owner]
  local desc="$1" file="$2" needle="$3" why="$4" owner="${5:-}"
  if [ -s "$file" ]; then check_contains "$desc" "$file" "$needle" "$owner"
  else skip "$desc" "$why"; fi
}

step_file() { printf 'step-%s.log' "$CURRENT_STEP"; }

begin_step() { # begin_step <n> <name>
  CURRENT_STEP="$1"
  head2 "step $1 — $2"
  : >"${EVIDENCE}/$(step_file)"
  dim "evidence: qa-evidence/runtime-e2e/${UTC_STAMP}/$(step_file)"
}

tee_step() { # tee_step <cmd...> : run, tee stdout+stderr into the step log, return its status
  "$@" 2>&1 | tee -a "${EVIDENCE}/$(step_file)"
  return "${PIPESTATUS[0]}"
}

# --- host primitives --------------------------------------------------------
# ss/lsof/netstat/fuser are not on a bare host; the socket table is.
hex_port() { printf '%04X' "$1"; }

port_listening() { # port_listening <port>
  local port="$1" want f
  want="$(hex_port "$port")"
  for f in /proc/net/tcp /proc/net/tcp6; do
    [ -r "$f" ] || continue
    awk -v want="$want" 'NR>1 && $4=="0A" { n=split($2,a,":"); if (a[n]==want) { found=1; exit } } END { exit found?0:1 }' "$f" && return 0
  done
  return 1
}

resolved_engine() { printf '%s' "${ENGINE_OVERRIDE:-${ORCHICON_ENGINE:-podman}}"; }

# engines_present — every container-engine CLI this host already has. AC1's
# validity precondition is that this prints NOTHING.
engines_present() {
  local e
  for e in docker podman nerdctl docker-compose podman-remote; do
    if command -v "$e" >/dev/null 2>&1; then printf '%s(%s) ' "$e" "$(command -v "$e")"; fi
  done
}

# engine_bin — the engine CLI to use ONCE one is provisioned, or "" when absent.
engine_bin() {
  local want
  want="$(resolved_engine)"
  command -v "$want" 2>/dev/null || command -v docker 2>/dev/null || true
}

# engine_capture <file> <args...> — run the ENGINE and capture its own output.
# Returns the engine's status; 127 when no engine exists. A capture written by a
# run that never happened stays EMPTY on purpose — that is what the gated_*
# functions turn into SKIP instead of a manufactured PASS.
engine_capture() { # engine_capture <file> <args...>
  local out="$1"; shift
  local bin
  bin="$(engine_bin)"
  if [ -z "$bin" ]; then : >"$out"; return 127; fi
  "$bin" "$@" >"$out" 2>&1
}

# --- plane / API primitives -------------------------------------------------
# The plane is a real Orchicon plane driven over Connect-JSON (the same unary
# HTTP transport the GUI uses). Nothing here is a mock: a wrong field name is a
# 400, not a silent pass.
instance_pg_port()   { case "$1" in dev) echo "${PG_OVERRIDE:-5432}" ;; prod) echo "${PG_OVERRIDE:-5433}" ;; *) echo "${PG_OVERRIDE:-5434}" ;; esac; }
instance_nats_port() { case "$1" in dev) echo "${NATS_OVERRIDE:-4222}" ;; prod) echo "${NATS_OVERRIDE:-4223}" ;; *) echo "${NATS_OVERRIDE:-4224}" ;; esac; }
instance_ctrl_port() { case "$1" in dev) echo "${CTRL_OVERRIDE:-8080}" ;; prod) echo "${CTRL_OVERRIDE:-8091}" ;; *) echo "${CTRL_OVERRIDE:-8092}" ;; esac; }

api_base() { printf 'http://127.0.0.1:%s' "$(instance_ctrl_port "$INSTANCE")"; }

resolve_token() { # prints a token if the documented client config has one
  if [ -n "${ORCHICON_TOKEN:-}" ]; then printf '%s' "$ORCHICON_TOKEN"; return 0; fi
  local cfg="${ORCHICON_CONFIG_DIR:-$HOME/.orchicon}/config"
  [ -f "$cfg" ] || return 1
  grep -oE 'oc_[A-Za-z0-9._-]{8,}' "$cfg" 2>/dev/null | head -1
}

plane_log_path() { printf '%s/.local/share/orchicon-%s/serve/logs/orchicon.log' "$HOME" "$1"; }

# api_post <procedure> <json> [out-file] — Connect unary POST. Prints the HTTP
# status when an out-file is given (the body lands in the file), else the body.
api_post() {
  local proc="$1" body="$2" out="${3:-}"
  local url tok
  url="$(api_base)${proc}"
  tok="$(resolve_token || true)"
  local args=(-sS --max-time 120 -X POST -H 'Content-Type: application/json')
  [ -n "$tok" ] && args+=(-H "Authorization: Bearer ${tok}")
  if [ -n "$out" ]; then
    curl "${args[@]}" -o "$out" -w '%{http_code}' -d "$body" "$url" 2>>"${EVIDENCE}/$(step_file)"
  else
    curl "${args[@]}" -d "$body" "$url" 2>>"${EVIDENCE}/$(step_file)"
  fi
}

# json_field <file> <key> — the scalar value of "key" in a JSON capture. jq is
# not on a bare host and adding a dependency to an acceptance run is a way to
# fail for the wrong reason, so this reads the encoder's own output.
json_field() { # json_field <file> <key>
  [ -s "$1" ] || return 1
  grep -oE "\"$2\":(\"[^\"]*\"|[0-9]+|true|false)" "$1" 2>/dev/null \
    | head -1 | sed -e 's/^"[^"]*"://' -e 's/^"//' -e 's/"$//'
}

# wait_http <path> <secs> — poll a plane endpoint until it answers non-000.
wait_http() {
  local path="$1" secs="$2" i code
  i=0
  while [ "$i" -lt "$((secs * 2))" ]; do
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "$(api_base)${path}" 2>/dev/null || true)"
    [ -n "$code" ] && [ "$code" != "000" ] && return 0
    sleep 0.5; i=$((i + 1))
  done
  return 1
}

start_plane() { # start_plane <log-file>
  tee_step ./scripts/orchicon.sh start "$INSTANCE" >"$1" 2>&1
}

stop_plane() {
  ./scripts/orchicon.sh stop "$INSTANCE" >/dev/null 2>&1 || true
}

project_body() { # project_body <mode-enum>
  printf '{"tenantId":"","name":"Runtime E2E %s","slug":"%s","gitStrategy":"GIT_STRATEGY_NONE","executionMode":"%s","requestId":"%s"}' \
    "$UTC_STAMP" "$PROJECT_SLUG" "$1" "$MARKER"
}

# --- step 0: preflight ------------------------------------------------------
step0_preflight() {
  begin_step 0 "preflight — is this run a valid proof of the claim?"
  local engines
  engines="$(engines_present)"
  {
    echo "date:      $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "host:      $(uname -a)"
    echo "hostname:  $(hostname)"
    echo "repo:      $REPO_ROOT"
    echo "marker:    $MARKER"
    echo "engines:   ${engines:-none found (GOOD — this is the claim being proven)}"
    echo "wanted:    $(resolved_engine)"
    echo "head:      $(git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null || echo 'not a git checkout')"
    echo "branch:    $(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD 2>/dev/null || echo '?')"
    echo "dirty:"
    git -C "$REPO_ROOT" status --porcelain 2>/dev/null | sed 's/^/  /'
    echo "go:        $(go version 2>/dev/null || echo ABSENT)"
    echo "node/npm:  $(node --version 2>/dev/null || echo ABSENT) / $(npm --version 2>/dev/null || echo ABSENT)"
    echo "frontend:  $( [ -x "${REPO_ROOT}/frontend/node_modules/.bin/vite" ] && echo 'node_modules present' || echo 'node_modules MISSING (the Runtime Images page cannot be driven)')"
    echo "ports:     ${INSTANCE} pg=$(instance_pg_port "$INSTANCE") nats=$(instance_nats_port "$INSTANCE") control=$(instance_ctrl_port "$INSTANCE")"
    echo "held now (must be empty):"
    local p; for p in "$(instance_pg_port "$INSTANCE")" "$(instance_nats_port "$INSTANCE")" "$(instance_ctrl_port "$INSTANCE")"; do
      port_listening "$p" && echo "  $p HELD"
    done
    echo "disk:"
    df -Pm "$REPO_ROOT" 2>/dev/null | sed 's/^/  /'
  } >"${EVIDENCE}/00-preflight.log" 2>&1
  cat "${EVIDENCE}/00-preflight.log"

  if [ -n "$engines" ]; then
    check "no container engine is installed at step 0 (${engines# })" 1
    ABORT_REASON="a container engine is already installed (${engines% }); AC1 (a local run on an engine-free host) cannot be proven here. Uninstall it or use a clean machine."
    return 1
  fi
  check "no container engine is installed (docker/podman/nerdctl/docker-compose all absent)" 0
  check "the tree is a git checkout" "$(git -C "$REPO_ROOT" rev-parse --verify HEAD >/dev/null 2>&1; echo $?)"

  local held=""
  local p
  for p in "$(instance_pg_port "$INSTANCE")" "$(instance_nats_port "$INSTANCE")" "$(instance_ctrl_port "$INSTANCE")"; do
    port_listening "$p" && held="${held}${p} "
  done
  if [ -n "${held// /}" ]; then
    # A held port is the HOST's condition, not a defect against a sibling file:
    # the run is invalid (exit 2), so this is recorded as UNPROVEN, never as a
    # defect row that names an innocent owner.
    skip "the instance ports are free before the session starts" "held by another process: ${held}"
    ABORT_REASON="port(s) ${held}are already held, so this run cannot prove that the instance's OWN plane is what it drove. Free them, or shift the run with --pg-port/--nats-port/--control-port."
    return 1
  fi
  check "the instance ports are free before the session starts" 0

  local free_mb
  free_mb="$(df -Pm "$REPO_ROOT" 2>/dev/null | awk 'NR==2 {print $4}')"
  check "at least 10 GiB free for engine + images + images-to-build (have ${free_mb:-?} MiB)" \
    "$( [ -n "${free_mb:-}" ] && [ "$free_mb" -ge 10240 ] && echo 0 || echo 1 )" "docs/runtime-e2e-acceptance.md (validity precondition)"
  check "the frontend dependencies are installed (the Runtime Images page is driven in step 6)" \
    "$( [ -x "${REPO_ROOT}/frontend/node_modules/.bin/vite" ] && echo 0 || echo 1 )" "Makefile fe-install (run \`make fe-install\`)"
  return 0
}

# --- step 1: the baseline local run, with NO engine -------------------------
step1_local_run() {
  begin_step 1 "local run on an engine-free host — the baseline (AC1)"
  local log="${EVIDENCE}/01-local-run.txt"
  local code

  start_plane "${EVIDENCE}/01-plane-start.log" || true
  if wait_http /healthz 90; then check "the plane answers /healthz with no engine on the host" 0
  else check "the plane answers /healthz with no engine on the host" 1 "scripts/orchicon.sh start (host services) — see 01-plane-start.log"; return; fi

  # The project is created in LOCAL mode through the same call the UI makes.
  code="$(api_post /orchicon.api.v1.ProjectService/CreateProject "$(project_body EXECUTION_MODE_LOCAL)" "${EVIDENCE}/01-project-create.json")"
  check "ProjectService/CreateProject accepted a local-mode project (HTTP ${code})" \
    "$( [ "$code" = "200" ] && echo 0 || echo 1 )" "proto/orchicon/api/v1/project.proto:125-143"
  PROJECT_ID="$(json_field "${EVIDENCE}/01-project-create.json" id)"
  if [ -n "$PROJECT_ID" ]; then
    api_post /orchicon.api.v1.ProjectService/GetProject "{\"id\":\"${PROJECT_ID}\"}" "${EVIDENCE}/01-project.json" >/dev/null 2>&1 || true
    gated_contains "the project really is in local mode (the mode is read back, not echoed)" \
      "${EVIDENCE}/01-project.json" "EXECUTION_MODE_LOCAL" \
      "the GetProject capture is EMPTY — the mode was never read back" \
      "internal/db/project.go:66-79"
  else
    skip "the project mode is read back" "CreateProject returned no project id"
  fi

  # The workflow: supplied, or one all-native step created here so the SAME
  # definition can be re-run in step 8.
  if [ -z "$WORKFLOW_ID" ] && [ -n "$PROJECT_ID" ]; then
    # ONE STEP, AND WORKER-FREE. STEP_KIND_TASK with no `ref` is a definition error
    # the reconciler rejects outright ("worker step %q has no worker ref",
    # workflow_reconciler.go:2345-2347), so a task step here would fail the run on a
    # CORRECT system and file a defect against the plane. STEP_KIND_END is the
    # terminal sink that "always succeeds when deps satisfied" with NO dispatch
    # (workflow_reconciler.go:2781-2794): all-native, workerless, and the SAME
    # definition is what steps 3-8 re-run.
    code="$(api_post /orchicon.api.v1.WorkflowService/CreateWorkflow \
      "$(printf '{"projectId":"%s","name":"runtime-e2e %s","steps":"[{\\"id\\":\\"probe\\",\\"name\\":\\"probe\\",\\"kind\\":\\"STEP_KIND_END\\",\\"config\\":\\"{}\\",\\"dependsOn\\":[]}]","requestId":"%s-wf"}' "$PROJECT_ID" "$MARKER" "$MARKER")" \
      "${EVIDENCE}/01-workflow-create.json")"
    WORKFLOW_ID="$(json_field "${EVIDENCE}/01-workflow-create.json" id)"
    check "a one-step all-native workflow was created (HTTP ${code})" \
      "$( [ "$code" = "200" ] && echo 0 || echo 1 )" "proto/orchicon/api/v1/workflow_service.proto:180-193"
  fi
  if [ -z "$WORKFLOW_ID" ]; then
    skip "the same workflow runs in local mode" "no --workflow-id was given and none could be created"
    return
  fi

  code="$(api_post /orchicon.api.v1.WorkflowService/StartWorkflow \
    "$(printf '{"workflowId":"%s","projectId":"%s","runContext":"{\\"marker\\":\\"%s\\"}"}' "$WORKFLOW_ID" "$PROJECT_ID" "$MARKER")" \
    "${EVIDENCE}/01-run-start.json")"
  RUN_ID="$(json_field "${EVIDENCE}/01-run-start.json" id)"
  check "StartWorkflow accepted the run in local mode (HTTP ${code})" \
    "$( [ "$code" = "200" ] && echo 0 || echo 1 )" "proto/orchicon/api/v1/workflow_service.proto:300-309"
  [ -n "$RUN_ID" ] && wait_run_terminal "$RUN_ID" "${EVIDENCE}/01-run-final.json" 900

  gated_contains "the run reached a terminal state (read back from the plane)" \
    "${EVIDENCE}/01-run-final.json" "WORKFLOW_RUN_STATUS_COMPLETED" \
    "the run capture is EMPTY — the run's fate was never read, so AC1 is UNPROVEN" \
    "internal/execution/service.go:1365-1409"
  # THE MODE IS READ FROM THE PROJECT, NOT FROM THE RUN. WorkflowRun carries no
  # execution_mode field (workflow.proto:82-105), so asserting one on the run
  # capture would fail on a correct system AND name an innocent owner. What the
  # bridge gates on is the PROJECT's mode at dispatch time, so that is read back
  # AFTER the run and asserted local.
  api_post /orchicon.api/v1.ProjectService/GetProject "{\"id\":\"${PROJECT_ID}\"}" \
    "${EVIDENCE}/01-project-after-run.json" >/dev/null 2>&1 || true
  gated_contains "the project was still in LOCAL mode across the run (the gate that keeps work in-process)" \
    "${EVIDENCE}/01-project-after-run.json" "EXECUTION_MODE_LOCAL" \
    "the post-run project capture is EMPTY — the mode the run was gated on was never read" \
    "internal/scheduler/bridge.go:60-68"

  # ZERO engine invocations is proven on the host, not by the plane's report:
  # there is no engine binary at all, and the plane log must carry no lease.
  check "still no container engine exists on the host after the local run" \
    "$( [ -z "$(engines_present)" ] && echo 0 || echo 1 )" "docs/runtime-e2e-acceptance.md (AC1)"
  engine_capture "${EVIDENCE}/01-engine-ps.txt" ps -a >/dev/null 2>&1 || true
  gated_contains "the engine was never invoked for the local run (no container exists)" \
    "${EVIDENCE}/01-engine-ps.txt" "orchicon.workflow" \
    "there is no engine to ask (step 1 runs on an engine-free host) — the absence is proven by 00-preflight.log instead" \
    "internal/runtime/lifecycle.go:105-107"
  plane_log_path "$INSTANCE" >/dev/null 2>&1 || true
  local plog; plog="$(plane_log_path "$INSTANCE")"
  if [ -s "$plog" ]; then
    tail -n 4000 "$plog" >"${EVIDENCE}/01-plane-log-tail.txt" 2>/dev/null || true
  else
    : >"${EVIDENCE}/01-plane-log-tail.txt"
  fi
  gated_absent "the plane log carries no container lease for the local run" \
    "${EVIDENCE}/01-plane-log-tail.txt" "workflow runtime ensured" \
    "the plane log is EMPTY or unwritten — an absence there proves nothing (see 01-plane-start.log)" \
    "internal/runtime/lifecycle.go:105-107"
  : >"$log"
}

# wait_run_terminal <run-id> <out-file> <secs> — poll the run until it is
# terminal, then leave the LAST capture in <out-file>. A poll that never gets an
# answer leaves the file empty on purpose (the gated assertions then say SKIP).
wait_run_terminal() { # wait_run_terminal <run-id> <out> <secs>
  local id="$1" out="$2" secs="$3" i=0 st
  while [ "$i" -lt "$secs" ]; do
    api_post /orchicon.api.v1.WorkflowService/GetWorkflowRun "{\"id\":\"${id}\"}" "$out" >/dev/null 2>&1 || true
    st="$(json_field "$out" status)"
    case "$st" in
      WORKFLOW_RUN_STATUS_COMPLETED|WORKFLOW_RUN_STATUS_FAILED|WORKFLOW_RUN_STATUS_ABORTED) return 0 ;;
    esac
    sleep 3; i=$((i + 3))
  done
  return 1
}

# --- step 2: opt in — provisioning, and availability only after verify ------
step2_opt_in() {
  begin_step 2 "opt in to runtime — podman is provisioned VISIBLY (AC2)"
  local code
  if [ -z "$PROJECT_ID" ]; then
    skip "the project is switched to runtime" "step 1 created no project"
  else
    code="$(api_post /orchicon.api.v1.ProjectService/UpdateProject \
      "$(printf '{"id":"%s","executionMode":"EXECUTION_MODE_RUNTIME","requestId":"%s-mode"}' "$PROJECT_ID" "$MARKER")" \
      "${EVIDENCE}/02-project-update.json")"
    check "the project's execution mode is switched to runtime (HTTP ${code})" \
      "$( [ "$code" = "200" ] && echo 0 || echo 1 )" "internal/db/project.go:321-332"
    gated_contains "the mode change is echoed back as runtime (read back, not assumed)" \
      "${EVIDENCE}/02-project-update.json" "EXECUTION_MODE_RUNTIME" \
      "the UpdateProject capture is EMPTY — the new mode was never read back" \
      "internal/db/project.go:321-332"
  fi

  # PROVISIONING IS THE INSTALL PATH. `orchicon` resolves the engine on start-up:
  # detect -> opt-in install (command printed first) -> minimum -> machine ->
  # VERIFY (a real container must exit 0) -> ready. The output of that start is
  # the "visibly" half of AC2, so it is captured verbatim.
  ORCHICON_PROVISION_ENGINE="${ORCHICON_PROVISION_ENGINE:-1}" \
    ./scripts/orchicon.sh restart "$INSTANCE" >"${EVIDENCE}/02-install.log" 2>&1 || true
  cat "${EVIDENCE}/02-install.log" | sed 's/^/  | /'
  gated_contains "the installer DETECTED that no engine was present" \
    "${EVIDENCE}/02-install.log" "checking the container engine" \
    "the install log is EMPTY — nothing was recorded, so AC2 is UNPROVEN" \
    "cmd/orchicon/install_engine.go:101"
  gated_contains "the installer VERIFIED the engine by running a container before claiming ready" \
    "${EVIDENCE}/02-install.log" "verifying the container engine" \
    "the install log is EMPTY — see 02-install.log" \
    "internal/containerengine/verify.go:18-62"
  gated_contains "the install claims ready only at the end of its rungs" \
    "${EVIDENCE}/02-install.log" "container engine ready" \
    "the install log is EMPTY — see 02-install.log" \
    "cmd/orchicon/install_engine.go:148-160"
  # ORDER, NOT JUST PRESENCE. "Availability only after verifying the engine works"
  # is a claim about SEQUENCE: `container engine ready` must come after the verify
  # phase in the same run's output, else "ready" could precede any verification.
  local verify_at ready_at
  verify_at="$(grep -nF "verifying the container engine" "${EVIDENCE}/02-install.log" 2>/dev/null | head -1 | cut -d: -f1)"
  ready_at="$(grep -nF "container engine ready" "${EVIDENCE}/02-install.log" 2>/dev/null | head -1 | cut -d: -f1)"
  check "ready is claimed AFTER the verify phase, in sequence (not merely present)" \
    "$( [ -n "${verify_at:-}" ] && [ -n "${ready_at:-}" ] && [ "${ready_at:-0}" -gt "${verify_at:-0}" ] && echo 0 || echo 1 )" \
    "cmd/orchicon/install_engine.go:148-160"
  check "an engine binary now exists on the host (the install really happened)" \
    "$( [ -n "$(engine_bin)" ] && echo 0 || echo 1 )" "internal/containerengine/provision.go:76-103"
  engine_capture "${EVIDENCE}/02-engine-version.txt" --version >/dev/null 2>&1 || true
  gated_contains "the engine reports its own version (independent of the plane)" \
    "${EVIDENCE}/02-engine-version.txt" "version" \
    "the engine version capture is EMPTY — the engine did not answer" \
    "internal/containerengine/detect.go"

  # THE AVAILABILITY SURFACE the UI renders under the mode select. /enginez is
  # served by containerengine.Inspect, i.e. the same package the installer
  # decided with — so the UI cannot claim an availability the install did not
  # produce.
  curl -sS --max-time 15 "$(api_base)/enginez?refresh=1" >"${EVIDENCE}/02-enginez.json" 2>>"${EVIDENCE}/$(step_file)" || true
  gated_contains "/enginez reports the engine AVAILABLE now (AC2)" \
    "${EVIDENCE}/02-enginez.json" '"available":true' \
    "the /enginez capture is EMPTY — the availability surface never answered" \
    "internal/api/engine_status.go:13-64"
  gated_contains "/enginez reports state=ready (not merely \"detected\")" \
    "${EVIDENCE}/02-enginez.json" '"state":"ready"' \
    "the /enginez capture is EMPTY — see 02-enginez.json" \
    "internal/api/engine_status.go:49"

  # ADVERSARIAL CONTROL (D4), PART 1 — the INSTALL. Force the verify to fail and
  # prove the install does NOT claim ready over a broken engine. Without this,
  # "ready" is a claim nobody ever tried to falsify.
  ORCHICON_PROVISION_ENGINE=1 ORCHICON_ENGINE_VERIFY_IMAGE="${ORCHICON_E2E_BROKEN_IMAGE:-docker.io/library/definitely-not-an-image:0}" \
    ./scripts/orchicon.sh restart "$INSTANCE" >"${EVIDENCE}/02-install-adversarial.log" 2>&1 || true
  gated_absent "ADVERSARIAL: a broken verify image does NOT produce a ready claim" \
    "${EVIDENCE}/02-install-adversarial.log" "container engine ready" \
    "the adversarial install log is EMPTY — the control never ran, so the claim stays unproven" \
    "internal/containerengine/verify.go:26"
  gated_contains "ADVERSARIAL: the verify phase really ran before refusing (not a skipped install)" \
    "${EVIDENCE}/02-install-adversarial.log" "verifying the container engine" \
    "the adversarial install log is EMPTY — the verify phase never ran, so a refusal there proves nothing" \
    "cmd/orchicon/install_engine.go:148-156"

  # PART 2 — the AVAILABILITY SURFACE. /enginez is containerengine.Inspect: a cheap
  # probe that DELIBERATELY never runs a container (that is Verify, the installer's
  # job — detect.go:343-345). A broken verify IMAGE is therefore invisible to it by
  # design, so asserting otherwise would file a defect against an innocent owner.
  # The honest control is an engine the probe cannot DRIVE at all: a stub `podman`
  # that fails every call. /enginez must then read unavailable, never ready, and
  # PROVISIONING IS OFF so the control can never install over the stub.
  local stub="${EVIDENCE}/stub-bin"
  mkdir -p "$stub"
  printf '#!/bin/sh\necho "stub engine: the acceptance control broke me" >&2\nexit 1\n' >"${stub}/podman"
  printf '#!/bin/sh\necho "stub engine: the acceptance control broke me" >&2\nexit 1\n' >"${stub}/docker"
  chmod +x "${stub}/podman" "${stub}/docker"
  PATH="${stub}:${PATH}" ORCHICON_PROVISION_ENGINE=0 \
    ./scripts/orchicon.sh restart "$INSTANCE" >"${EVIDENCE}/02-install-undriveable-engine.log" 2>&1 || true
  wait_http /healthz 90 || say "  (the plane did not answer after the undriveable-engine restart — see 02-install-undriveable-engine.log)"
  gated_absent "ADVERSARIAL: the install does not claim ready over an engine it cannot drive" \
    "${EVIDENCE}/02-install-undriveable-engine.log" "container engine ready" \
    "the undriveable-engine install log is EMPTY — the control never ran" \
    "internal/containerengine/detect.go:369-372"
  curl -sS --max-time 15 "$(api_base)/enginez?refresh=1" >"${EVIDENCE}/02-enginez-adversarial.json" 2>>"${EVIDENCE}/$(step_file)" || true
  gated_absent "ADVERSARIAL: /enginez does not report ready for an engine it cannot drive" \
    "${EVIDENCE}/02-enginez-adversarial.json" '"state":"ready"' \
    "the adversarial /enginez capture is EMPTY — the surface never answered" \
    "internal/api/engine_status.go:49 + internal/containerengine/detect.go:369-372"

  # Restore a working plane for steps 3-8 (the control above deliberately broke it).
  ORCHICON_PROVISION_ENGINE=1 ./scripts/orchicon.sh restart "$INSTANCE" >>"${EVIDENCE}/02-install.log" 2>&1 || true
  wait_http /healthz 90 && check "the plane is healthy again after the controls" 0 \
    || check "the plane is healthy again after the controls" 1 "scripts/orchicon.sh start"
}

# --- step 3: the SAME workflow, now INSIDE a container ----------------------
step3_container_run() {
  begin_step 3 "the same workflow, now inside a container (AC3)"
  local code before after
  engine_capture "${EVIDENCE}/03-engine-ps-before.txt" ps -a --format '{{.Names}}' >/dev/null 2>&1 || true
  if [ -z "$WORKFLOW_ID" ]; then
    skip "the same workflow runs in a container" "step 1 resolved no workflow"
    return
  fi
  code="$(api_post /orchicon.api.v1.WorkflowService/StartWorkflow \
    "$(printf '{"workflowId":"%s","projectId":"%s","runContext":"{\\"marker\\":\\"%s\\"}"}' "$WORKFLOW_ID" "$PROJECT_ID" "$MARKER")" \
    "${EVIDENCE}/03-run-start.json")"
  RUN_ID="$(json_field "${EVIDENCE}/03-run-start.json" id)"
  check "StartWorkflow accepted the SAME workflow with the project in runtime mode (HTTP ${code})" \
    "$( [ "$code" = "200" ] && echo 0 || echo 1 )" "internal/execution/service.go:1365-1409"
  [ -n "$RUN_ID" ] && wait_run_terminal "$RUN_ID" "${EVIDENCE}/03-run-final.json" 900
  gated_contains "the run reached a terminal state in runtime mode" \
    "${EVIDENCE}/03-run-final.json" "WORKFLOW_RUN_STATUS_COMPLETED" \
    "the run capture is EMPTY — the run's fate was never read, so AC3 is UNPROVEN" \
    "internal/scheduler/bridge.go:60-68"

  # INDEPENDENT, ENGINE-SIDE: a container that did not exist before this run
  # exists now, and it carries our ownership labels.
  after="${EVIDENCE}/03-engine-ps-after.txt"
  engine_capture "$after" ps -a --format '{{.Names}}' >/dev/null 2>&1 || true
  gated_contains "the engine lists a container after the runtime-mode run" \
    "$after" "orchicon" \
    "the engine's own ps capture is EMPTY — the engine did not answer, so residency is UNPROVEN" \
    "internal/runtime/lifecycle.go:958-988"
  CONTAINER_NAME="$(comm -13 <(sort -u "${EVIDENCE}/03-engine-ps-before.txt" 2>/dev/null) <(sort -u "$after" 2>/dev/null) 2>/dev/null | head -1)"
  check "a container NEWLY appeared for this run (engine ground truth, diffed before/after)" \
    "$( [ -n "$CONTAINER_NAME" ] && echo 0 || echo 1 )" "internal/runtime/lifecycle.go:958-988"
  engine_capture "${EVIDENCE}/03-engine-ps-labelled.txt" ps -a --filter label=orchicon.workflow --format '{{.Names}}' >/dev/null 2>&1 || true
  gated_contains "the container carries the orchicon.workflow ownership label" \
    "${EVIDENCE}/03-engine-ps-labelled.txt" "orchicon" \
    "the labelled ps capture is EMPTY — the engine did not answer" \
    "internal/runtime/daemon.go:1373-1375"
  if [ -n "$CONTAINER_NAME" ]; then
    engine_capture "${EVIDENCE}/03-container-inspect.txt" \
      inspect --format '{{.Config.Hostname}} {{.Config.Image}} {{.State.Status}}' "$CONTAINER_NAME" >/dev/null 2>&1 || true
    gated_contains "the container is a real, running container (engine inspect)" \
      "${EVIDENCE}/03-container-inspect.txt" "running" \
      "the inspect capture is EMPTY — the container could not be inspected" \
      "internal/runtime/daemon.go:152"
    # NOT MERELY THAT A CONTAINER EXISTS: this is the in-run half. The step's own
    # output must carry the CONTAINER's hostname, which cannot equal the host's.
    # THE MARKER IS INTERPOLATED INTO THE IN-CONTAINER COMMAND on purpose: with a
    # single-quoted command the run marker never reaches the container, so the
    # "is it OUR container, and did something execute inside it" half of AC3 would
    # fail on a correct system. As written, the marker can only appear if this exec
    # really ran inside the container this run created.
    engine_capture "${EVIDENCE}/03-in-container-hostname.txt" \
      exec "$CONTAINER_NAME" sh -c "hostname; echo \"runtime-e2e marker=${MARKER}\"" >/dev/null 2>&1 || true
    local chost hhost
    chost="$(awk 'NR==1 {print $1}' "${EVIDENCE}/03-in-container-hostname.txt" 2>/dev/null)"
    hhost="$(hostname)"
    gated_contains "the in-container hostname differs from the HOST's hostname (the work ran inside)" \
      "${EVIDENCE}/03-in-container-hostname.txt" "$MARKER" \
      "the in-container capture is EMPTY — nothing executed inside the container, so AC3's \"ran inside\" half is UNPROVEN" \
      "internal/runtime/agent.go + internal/claude/adapter.go:317-332"
    check "the container's hostname ($chost) is NOT the host's ($hhost)" \
      "$( [ -n "$chost" ] && [ "$chost" != "$hhost" ] && echo 0 || echo 1 )" "internal/runtime/daemon.go:802-811"
  fi
  local plog; plog="$(plane_log_path "$INSTANCE")"
  if [ -s "$plog" ]; then grep -F "workflow runtime ensured" "$plog" | tail -50 >"${EVIDENCE}/03-plane-lease.txt" 2>/dev/null || : >"${EVIDENCE}/03-plane-lease.txt"
  else : >"${EVIDENCE}/03-plane-lease.txt"; fi
  gated_contains "the plane recorded the container lease for the run (control-plane half)" \
    "${EVIDENCE}/03-plane-lease.txt" "workflow runtime ensured" \
    "the plane log carries no lease line and/or is unwritten — see 01-plane-start.log" \
    "internal/runtime/lifecycle.go:958-988"
  : >"${EVIDENCE}/03-container-residency.txt"
  { echo "container: ${CONTAINER_NAME:-<none>}"; cat "${EVIDENCE}/03-container-inspect.txt" 2>/dev/null; } >>"${EVIDENCE}/03-container-residency.txt"
}

# --- step 4: pristine reset -------------------------------------------------
step4_pristine_reset() {
  begin_step 4 "state created in run A is ABSENT in run B (AC4)"
  local a="$CONTAINER_NAME"
  if [ -z "$a" ]; then skip "the pristine-reset guarantee" "step 3 observed no container"; return; fi
  engine_capture "${EVIDENCE}/04-state-write.txt" \
    exec "$a" sh -c "echo ${MARKER} > /tmp/${MARKER}; mkdir -p /tmp/${MARKER}-dir; echo written" >/dev/null 2>&1 || true
  gated_contains "state was really created inside run A's container (engine exec)" \
    "${EVIDENCE}/04-state-write.txt" "written" \
    "the exec capture is EMPTY — no state was created, so the reset proves nothing" \
    "internal/runtime/pool.go:312-375"

  # A SECOND run on the SAME environment (same project, same inputs).
  api_post /orchicon.api.v1.WorkflowService/StartWorkflow \
    "$(printf '{"workflowId":"%s","projectId":"%s","runContext":"{\\"marker\\":\\"%s-b\\"}"}' "$WORKFLOW_ID" "$PROJECT_ID" "$MARKER")" \
    "${EVIDENCE}/04-run-start.json" >/dev/null 2>&1 || true
  local rid; rid="$(json_field "${EVIDENCE}/04-run-start.json" id)"
  [ -n "$rid" ] && wait_run_terminal "$rid" "${EVIDENCE}/04-run-final.json" 900
  engine_capture "${EVIDENCE}/04-engine-ps-after.txt" ps -a --format '{{.Names}}' >/dev/null 2>&1 || true
  local b; b="$(comm -13 <(sort -u "${EVIDENCE}/03-engine-ps-after.txt" 2>/dev/null) <(sort -u "${EVIDENCE}/04-engine-ps-after.txt" 2>/dev/null) 2>/dev/null | head -1)"
  [ -n "$b" ] || b="$(tail -1 "${EVIDENCE}/04-engine-ps-after.txt" 2>/dev/null)"
  { echo "run A container: $a"; echo "run B container: ${b:-<none>}"; } >"${EVIDENCE}/04-pristine-reset.txt"
  engine_capture "${EVIDENCE}/04-state-probe.txt" \
    exec "${b:-$a}" sh -c "test -e /tmp/${MARKER} && echo PRESENT || echo ABSENT; test -e /tmp/${MARKER}-dir && echo DIR-PRESENT || echo DIR-ABSENT" >/dev/null 2>&1 || true
  cat "${EVIDENCE}/04-state-probe.txt" >>"${EVIDENCE}/04-pristine-reset.txt" 2>/dev/null || true
  gated_contains "run B's container does NOT carry run A's /tmp file (the pristine reset)" \
    "${EVIDENCE}/04-state-probe.txt" "ABSENT" \
    "the probe capture is EMPTY — the filesystem was never read, so AC4 is UNPROVEN" \
    "internal/runtime/pool.go:312-375"
  gated_absent "run B's container really did not keep the directory run A made" \
    "${EVIDENCE}/04-state-probe.txt" "DIR-PRESENT" \
    "the probe capture is EMPTY — the filesystem was never read" \
    "internal/runtime/pool.go:312-375"
  check "run B was served by a container that is not exactly run A's lease" \
    "$( [ -n "${b:-}" ] && [ "${b:-}" != "$a" ] && echo 0 || echo 1 )" "internal/runtime/pool.go:312-375"
}

# --- step 5: warm-pool reuse, and a changed host input forcing a fresh one --
step5_pool_reuse() {
  begin_step 5 "a second run reuses the pool; a changed host input forces a fresh one (AC5)"
  local before after reuse
  engine_capture "${EVIDENCE}/05-ps-before.txt" ps -a --format '{{.Names}}' >/dev/null 2>&1 || true
  api_post /orchicon.api.v1.WorkflowService/StartWorkflow \
    "$(printf '{"workflowId":"%s","projectId":"%s","runContext":"{}"}' "$WORKFLOW_ID" "$PROJECT_ID")" \
    "${EVIDENCE}/05-run-start-a.json" >/dev/null 2>&1 || true
  local rid; rid="$(json_field "${EVIDENCE}/05-run-start-a.json" id)"
  [ -n "$rid" ] && wait_run_terminal "$rid" "${EVIDENCE}/05-run-final-a.json" 900
  engine_capture "${EVIDENCE}/05-ps-after.txt" ps -a --format '{{.Names}}' >/dev/null 2>&1 || true
  reuse="$(comm -13 <(sort -u "${EVIDENCE}/05-ps-before.txt" 2>/dev/null) <(sort -u "${EVIDENCE}/05-ps-after.txt" 2>/dev/null) 2>/dev/null | head -1)"
  { echo "before:"; cat "${EVIDENCE}/05-ps-before.txt" 2>/dev/null; echo "after:"; cat "${EVIDENCE}/05-ps-after.txt" 2>/dev/null; } >"${EVIDENCE}/05-pool-reuse.txt"
  check "the run was served WITHOUT creating a new container (warm reuse)" \
    "$( [ -s "${EVIDENCE}/05-ps-after.txt" ] && { [ -z "$reuse" ] && echo 0 || echo 1; } || echo 1 )" \
    "internal/runtime/pool.go:98-155"
  gated_absent "WARM REUSE: no new container name appeared for an unchanged environment" \
    "${EVIDENCE}/05-pool-reuse.txt" "$MARKER" \
    "the engine captures are EMPTY — the engine did not answer, so reuse is UNPROVEN" \
    "internal/runtime/pool.go:98-155"

  # CHANGE A HOST INPUT: git_strategy is part of the pool env key (f(image,
  # mounts, serve config, git strategy, adapter demand, host fingerprint)), so a
  # change must force a FRESH container rather than re-lease the warm one.
  api_post /orchicon.api.v1.ProjectService/UpdateProject \
    "$(printf '{"id":"%s","gitStrategy":"GIT_STRATEGY_LOCAL","requestId":"%s-gs"}' "$PROJECT_ID" "$MARKER")" \
    "${EVIDENCE}/05-project-gitstrategy.json" >/dev/null 2>&1 || true
  engine_capture "${EVIDENCE}/05-ps-before-change.txt" ps -a --format '{{.Names}}' >/dev/null 2>&1 || true
  api_post /orchicon.api.v1.WorkflowService/StartWorkflow \
    "$(printf '{"workflowId":"%s","projectId":"%s","runContext":"{}"}' "$WORKFLOW_ID" "$PROJECT_ID")" \
    "${EVIDENCE}/05-run-start-b.json" >/dev/null 2>&1 || true
  rid="$(json_field "${EVIDENCE}/05-run-start-b.json" id)"
  [ -n "$rid" ] && wait_run_terminal "$rid" "${EVIDENCE}/05-run-final-b.json" 900
  engine_capture "${EVIDENCE}/05-ps-after-change.txt" ps -a --format '{{.Names}}' >/dev/null 2>&1 || true
  { echo "before the host-input change:"; cat "${EVIDENCE}/05-ps-before-change.txt" 2>/dev/null
    echo "after the host-input change:"; cat "${EVIDENCE}/05-ps-after-change.txt" 2>/dev/null; } >>"${EVIDENCE}/05-pool-reuse.txt"
  after="$(comm -13 <(sort -u "${EVIDENCE}/05-ps-before-change.txt" 2>/dev/null) <(sort -u "${EVIDENCE}/05-ps-after-change.txt" 2>/dev/null) 2>/dev/null | head -1)"
  check "a changed host input (git_strategy) forced a FRESH container" \
    "$( [ -n "$after" ] && echo 0 || echo 1 )" "internal/runtime/pool.go:98-155 + internal/runtime/hostfp.go"
}

# --- step 6: the image path -------------------------------------------------
# The image is CREATED on the Runtime Images page (its spec editor is a UI, not
# an API contract this harness should fork). The harness then drives the half
# that must be machine-checked: the BUILD STREAM, the idempotent re-deploy, and a
# run that actually uses the image.
step6_image_path() {
  begin_step 6 "a custom image builds with a streaming log, re-deploys idempotently, and runs (AC6)"
  if [ -z "${IMAGE_ID:-}" ]; then
    skip "the custom image's build log streams" "no --image-id was given: create the image on the Runtime Images page first (docs/runtime-e2e-acceptance.md step 6)"
    skip "a re-deploy is idempotent" "no --image-id was given"
    skip "a run using the custom image starts" "no --image-id was given"
    return
  fi
  connect_stream /orchicon.api.v1.RuntimeImageService/BuildRuntimeImage \
    "$(printf '{"id":"%s"}' "$IMAGE_ID")" "${EVIDENCE}/06-image-build.log"
  # A stream is a sequence of Connect envelopes: count DATA frames, not LINES. The
  # frames are concatenated with no separator, so a whole streamed build log is ONE
  # line and a line count would deny a stream that really did stream.
  local frames
  frames="$(connect_frames "${EVIDENCE}/06-image-build.log")"
  gated_contains "the build log STREAMS (the first chunk arrives before the build ends)" \
    "${EVIDENCE}/06-image-build.log" "{" \
    "the build stream capture is EMPTY — the build never streamed, so AC6 is UNPROVEN" \
    "internal/runtimeimage/service.go:496-556"
  check "the build stream delivered multiple chunks (${frames} line(s))" \
    "$( [ "${frames:-0}" -ge 2 ] && echo 0 || echo 1 )" "internal/runtimeimage/service.go:496-556"
  connect_stream /orchicon.api.v1.RuntimeImageService/BuildRuntimeImage \
    "$(printf '{"id":"%s"}' "$IMAGE_ID")" "${EVIDENCE}/06-image-redeploy.txt"
  gated_contains "a re-deploy is IDEMPOTENT: an up-to-date image is not rebuilt" \
    "${EVIDENCE}/06-image-redeploy.txt" "already up to date" \
    "the re-deploy capture is EMPTY — idempotency was never exercised" \
    "internal/runtimeimage/service.go:557-747"
  # A run that names the image must start and pin it.
  api_post /orchicon.api.v1.ProjectService/UpdateProject \
    "$(printf '{"id":"%s","defaultRuntimeImage":"%s","requestId":"%s-img"}' "$PROJECT_ID" "${CUSTOM_IMAGE_REF:-}" "$MARKER")" \
    "${EVIDENCE}/06-project-image.json" >/dev/null 2>&1 || true
  api_post /orchicon.api.v1.WorkflowService/StartWorkflow \
    "$(printf '{"workflowId":"%s","projectId":"%s","runContext":"{}"}' "$WORKFLOW_ID" "$PROJECT_ID")" \
    "${EVIDENCE}/06-run-start.json" >/dev/null 2>&1 || true
  local rid; rid="$(json_field "${EVIDENCE}/06-run-start.json" id)"
  check "a run using the custom image starts" \
    "$( [ -n "$rid" ] && echo 0 || echo 1 )" "internal/runtimeimage/service.go:496-556"
  [ -n "$rid" ] && wait_run_terminal "$rid" "${EVIDENCE}/06-run-final.json" 900
  # An EMPTY needle matches every capture, i.e. a vacuous PASS — so the tag is
  # required for this assertion rather than silently satisfied.
  if [ -n "${CUSTOM_IMAGE_REF:-}" ]; then
    gated_contains "the run pinned the custom image (the run's own runtime_image field)" \
      "${EVIDENCE}/06-run-final.json" "${CUSTOM_IMAGE_REF}" \
      "the run capture is EMPTY — the image the run used was never read" \
      "internal/runtimeimage/service.go:496-556"
  else
    skip "the run pinned the custom image" "no --custom-image tag was given, so there is no needle to search for (an empty one would match anything)"
  fi
}

# --- step 7: the image gate -------------------------------------------------
step7_gate_check() {
  begin_step 7 "the image gate REJECTS a non-conforming image (AC7)"
  # The daemon's socket dir is ORCHICON_RUNTIME_SOCKET_DIR (install.go:51);
  # ORCHICON_RUNTIME_SOCKET is accepted as a whole-path override.
  local sock="${ORCHICON_RUNTIME_SOCKET:-${ORCHICON_RUNTIME_SOCKET_DIR:-/tmp/orchicon-runtime}/runtime.sock}"
  if [ ! -S "$sock" ]; then
    skip "the gate rejects a non-conforming image" "no runtime daemon socket at ${sock} — run scripts/runtime.sh daemon-start first"
    return
  fi
  curl -sS --max-time 60 --unix-socket "$sock" -X POST -H 'Content-Type: application/json' \
    -d "$(printf '{"workflow_id":"%s-gate","image":"%s"}' "$MARKER" "$NONCONFORMING_IMAGE")" \
    "http://localhost/v1/runtimes/" >"${EVIDENCE}/07-gate-reject.txt" 2>>"${EVIDENCE}/$(step_file)" || true
  gated_contains "the gate REJECTED the create with 'image not allowed'" \
    "${EVIDENCE}/07-gate-reject.txt" "image not allowed" \
    "the daemon capture is EMPTY — the create was never attempted, so AC7 is UNPROVEN" \
    "internal/runtime/daemon.go:540-552"
  gated_absent "no container was created for the rejected image (engine ground truth)" \
    "${EVIDENCE}/07-gate-reject.txt" "$NONCONFORMING_IMAGE\"created" \
    "the daemon capture is EMPTY — the create was never attempted" \
    "internal/runtime/images.go:86-121"
  engine_capture "${EVIDENCE}/07-engine-ps.txt" ps -a --filter "label=orchicon.workflow" --format '{{.Names}} {{.Image}}' >/dev/null 2>&1 || true
  gated_absent "the engine lists NO container running the rejected image" \
    "${EVIDENCE}/07-engine-ps.txt" "$NONCONFORMING_IMAGE" \
    "the engine's ps capture is EMPTY — the engine did not answer, so the gate's effect is UNPROVEN" \
    "internal/runtime/images.go:106-121"
}

# --- step 8: the all-native path -------------------------------------------
step8_all_native() {
  begin_step 8 "an all-native workflow creates NO container (AC8)"
  # (a) WHERE THE CLAIM IS TRUE TODAY: local mode. The reconciler skips
  # EnsureForRun when the project is local, so an all-native workflow must leave
  # the engine untouched.
  api_post /orchicon.api.v1.ProjectService/UpdateProject \
    "$(printf '{"id":"%s","executionMode":"EXECUTION_MODE_LOCAL","requestId":"%s-local"}' "$PROJECT_ID" "$MARKER")" \
    "${EVIDENCE}/08-project-local.json" >/dev/null 2>&1 || true
  engine_capture "${EVIDENCE}/08-ps-before.txt" ps -a --format '{{.Names}}' >/dev/null 2>&1 || true
  api_post /orchicon.api.v1.WorkflowService/StartWorkflow \
    "$(printf '{"workflowId":"%s","projectId":"%s","runContext":"{\\"native\\":true}"}' "$WORKFLOW_ID" "$PROJECT_ID")" \
    "${EVIDENCE}/08-run-start.json" >/dev/null 2>&1 || true
  local rid; rid="$(json_field "${EVIDENCE}/08-run-start.json" id)"
  [ -n "$rid" ] && wait_run_terminal "$rid" "${EVIDENCE}/08-run-final.json" 900
  engine_capture "${EVIDENCE}/08-ps-after.txt" ps -a --format '{{.Names}}' >/dev/null 2>&1 || true
  { echo "all-native run (LOCAL mode) — before:"; cat "${EVIDENCE}/08-ps-before.txt" 2>/dev/null
    echo "after:"; cat "${EVIDENCE}/08-ps-after.txt" 2>/dev/null; } >"${EVIDENCE}/08-all-native.txt"
  local new; new="$(comm -13 <(sort -u "${EVIDENCE}/08-ps-before.txt" 2>/dev/null) <(sort -u "${EVIDENCE}/08-ps-after.txt" 2>/dev/null) 2>/dev/null | head -1)"
  # GATED, because "no container was created" is an ABSENCE: an engine whose ps
  # failed would manufacture exactly the pass this step exists to falsify.
  gated_absent "ALL-NATIVE IN LOCAL MODE: no NEW container appeared for the run" \
    "${EVIDENCE}/08-all-native.txt" "created: ${new:-none-new}" \
    "the engine captures are EMPTY — the engine did not answer, so \"no container\" is UNPROVEN" \
    "internal/scheduler/bridge.go:60-68"
  if [ -s "${EVIDENCE}/08-ps-after.txt" ] && [ -n "$new" ]; then
    check "ALL-NATIVE IN LOCAL MODE: no container was created (engine ps diff)" 1 \
      "internal/scheduler/bridge.go:60-68 + internal/runtime/lifecycle.go:105-107"
  elif [ -s "${EVIDENCE}/08-ps-after.txt" ]; then
    check "ALL-NATIVE IN LOCAL MODE: no container was created (engine ps diff)" 0
  else
    skip "ALL-NATIVE IN LOCAL MODE: no container was created (engine ps diff)" \
      "the engine's ps capture is EMPTY — the engine did not answer"
  fi

  # (b) WHERE THE MERGED DESIGN SAYS IT CANNOT BE TRUE. The always-container rule
  # (lifecycle.go:958-975) creates a container for EVERY runtime-mode run, so the
  # same all-native workflow in runtime mode DOES get one. That is recorded as a
  # contradiction against the owning file — never reinterpreted into a pass and
  # never patched from here (AC10).
  api_post /orchicon.api.v1.ProjectService/UpdateProject \
    "$(printf '{"id":"%s","executionMode":"EXECUTION_MODE_RUNTIME","requestId":"%s-rt"}' "$PROJECT_ID" "$MARKER")" \
    "${EVIDENCE}/08-project-runtime.json" >/dev/null 2>&1 || true
  engine_capture "${EVIDENCE}/08-ps-before-rt.txt" ps -a --format '{{.Names}}' >/dev/null 2>&1 || true
  api_post /orchicon.api.v1.WorkflowService/StartWorkflow \
    "$(printf '{"workflowId":"%s","projectId":"%s","runContext":"{\\"native\\":true}"}' "$WORKFLOW_ID" "$PROJECT_ID")" \
    "${EVIDENCE}/08-run-start-rt.json" >/dev/null 2>&1 || true
  rid="$(json_field "${EVIDENCE}/08-run-start-rt.json" id)"
  [ -n "$rid" ] && wait_run_terminal "$rid" "${EVIDENCE}/08-run-final-rt.json" 900
  engine_capture "${EVIDENCE}/08-ps-after-rt.txt" ps -a --format '{{.Names}}' >/dev/null 2>&1 || true
  { echo "all-native run (RUNTIME mode) — before:"; cat "${EVIDENCE}/08-ps-before-rt.txt" 2>/dev/null
    echo "after:"; cat "${EVIDENCE}/08-ps-after-rt.txt" 2>/dev/null; } >>"${EVIDENCE}/08-all-native.txt"
  local newrt; newrt="$(comm -13 <(sort -u "${EVIDENCE}/08-ps-before-rt.txt" 2>/dev/null) <(sort -u "${EVIDENCE}/08-ps-after-rt.txt" 2>/dev/null) 2>/dev/null | head -1)"
  if [ ! -s "${EVIDENCE}/08-ps-after-rt.txt" ]; then
    skip "ALL-NATIVE IN RUNTIME MODE: no container is created (AC8)" \
      "the engine's ps capture is EMPTY — the engine did not answer, so this is UNPROVEN either way"
  elif [ -n "$newrt" ]; then
    check "ALL-NATIVE IN RUNTIME MODE: no container is created (AC8)" 1 \
      "internal/runtime/lifecycle.go:958-975 (always-container: a container is created for EVERY runtime-mode run, so AC8 as written cannot hold there)"
  else
    check "ALL-NATIVE IN RUNTIME MODE: no container is created (AC8)" 0
  fi
}

# --- server-streaming over Connect (the build log) --------------------------
# BuildRuntimeImage is a SERVER STREAM. A plain unary POST would deliver the
# final message only — which is exactly the "it reported success, nobody saw it
# stream" failure AC6 exists to catch. So the request goes out in the Connect
# streaming envelope (1 flag byte + 4 length bytes, big endian) and the response
# is captured as it arrives, frame by frame.
connect_stream() { # connect_stream <procedure> <json> <out-file>
  local proc="$1" body="$2" out="$3" n tok env
  n=${#body}
  env="${out}.envelope"
  {
    printf '\000'
    printf "\\$(printf '%03o' $((n >> 24 & 255)))\\$(printf '%03o' $((n >> 16 & 255)))\\$(printf '%03o' $((n >> 8 & 255)))\\$(printf '%03o' $((n & 255)))"
    printf '%s' "$body"
  } >"$env"
  tok="$(resolve_token || true)"
  local args=(-sS -N --max-time 1800 -X POST -H 'Content-Type: application/connect+json' --data-binary "@${env}")
  [ -n "$tok" ] && args+=(-H "Authorization: Bearer ${tok}")
  curl "${args[@]}" "$(api_base)${proc}" >"$out" 2>>"${EVIDENCE}/$(step_file)" || true
}

# connect_frames <capture> — how many DATA envelopes a Connect server-stream
# capture carries. The body is [flag u8][length u32 BE][payload] repeated, with the
# end-of-stream trailer flagged 0x02, so a byte walk is the only honest way to say
# "more than one chunk arrived". Line counting is NOT: the frames are concatenated
# without separators, so an entire streamed build log is a single line.
connect_frames() { # connect_frames <capture>
  od -An -v -tu1 "$1" 2>/dev/null | awk '
    { for (i = 1; i <= NF; i++) b[++n] = $i }
    END {
      i = 1; frames = 0
      while (i <= n) {
        f = b[i]; i++
        if (f >= 2) break                                  # trailer / end-of-stream
        if (i + 3 > n) break                               # truncated frame
        len = b[i] * 16777216 + b[i+1] * 65536 + b[i+2] * 256 + b[i+3]
        i += 4 + len
        frames++
      }
      print frames + 0
    }'
}

# --- summary ---------------------------------------------------------------
verdict_for() { # verdict_for <step>
  local n="$1" st
  st="$(awk -F'\t' -v s="$n" '$1==s {print $2}' "${EVIDENCE}/results.tsv" 2>/dev/null | sort -u | tr '\n' ',')"
  case "$st" in
    *FAIL*) printf '**FAIL** (see defects.tsv)' ;;
    *SKIP*) printf 'SKIP' ;;
    ""|,) printf 'not run' ;;
    *) printf 'PASS' ;;
  esac
}

write_summary() {
  local md="${EVIDENCE}/summary.md"
  {
    echo "# Runtime-container E2E acceptance — harness run ${UTC_STAMP}"
    echo
    echo "- passes: ${PASSES}   failures: ${FAILS}   skips: ${SKIPS}   (a SKIP is NOT a pass)"
    echo "- marker: \`${MARKER}\` (unique per run, so a match cannot be a leftover)"
    echo "- instance: \`${INSTANCE}\` (pg $(instance_pg_port "$INSTANCE") / nats $(instance_nats_port "$INSTANCE") / control $(instance_ctrl_port "$INSTANCE"))"
    echo "- project: \`${PROJECT_ID:-<none>}\`  workflow: \`${WORKFLOW_ID:-<none>}\`"
    [ -n "$ABORT_REASON" ] && { echo; echo "**RUN INVALID:** ${ABORT_REASON}"; }
    echo
    echo "## per-AC verdict"
    echo
    echo "| AC | verdict |"
    echo "|---|---|"
    local n
    for n in 0 1 2 3 4 5 6 7 8; do printf '| %s | %s |\n' "$n" "$(verdict_for "$n")"; done
    echo
    echo "## evidence (independent ground truth)"
    echo
    echo '```'
    cat "${EVIDENCE}/00-preflight.log" 2>/dev/null || echo '(not captured)'
    echo '```'
    echo
    echo "## failures — each names the file that owns it (AC10)"
    echo
    if [ -s "${EVIDENCE}/defects.tsv" ]; then
      echo "| step/AC | assertion | owner |"
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
  [ "${TEARDOWN_DONE:-0}" = "1" ] && return 0
  if [ "$KEEP" = "1" ]; then
    say "teardown: --keep, leaving the plane and services running"
    return 0
  fi
  stop_plane
  say "teardown: instance ${INSTANCE} stopped (runtime containers are the daemon's to reap)"
}

# --- dry run ---------------------------------------------------------------
DRY_RUN_PLAN() {
  cat <<EOF
runtime-e2e — planned session (dry run: nothing is executed)

  marker     ${MARKER}
  instance   ${INSTANCE}: pg $(instance_pg_port "$INSTANCE") nats $(instance_nats_port "$INSTANCE") control $(instance_ctrl_port "$INSTANCE")
  engine     $(resolved_engine) (must be ABSENT at step 0, PROVISIONED by step 2)
  evidence   qa-evidence/runtime-e2e/${UTC_STAMP}/

  0  preflight       assert NO container engine; ports free; disk >= 10 GiB; frontend deps
  1  local run       project execution_mode=local -> StartWorkflow -> COMPLETED
                     zero engine invocations (there is no engine at all)      [AC1]
  2  opt in          mode -> runtime; install rungs printed (detect/install/min/machine/VERIFY/ready)
                     /enginez available+ready; ADVERSARIAL: broken verify image must NOT read ready [AC2]
  3  container run   SAME workflow: a NEW container appears (engine ps diff), labelled,
                     in-container hostname != host hostname                     [AC3]
  4  pristine reset  write /tmp/${MARKER} in run A; run B must not see it       [AC4]
  5  warm reuse      unchanged env re-leases; git_strategy change forces fresh  [AC5]
  6  image path      BuildRuntimeImage stream -> re-deploy "already up to date" -> run [AC6]
  7  gate check      create naming ${NONCONFORMING_IMAGE} must be REJECTED     [AC7]
  8  all-native      no container in LOCAL mode; runtime mode recorded, not reinterpreted [AC8]

exit: 0 all PASS | 1 any FAIL (defect filed against the owner) | 2 run invalid (engine present / ports held)
EOF
}

usage() { sed -n '2,68p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --instance) INSTANCE="${2:?}"; shift 2 ;;
      --evidence-dir) EVIDENCE="${2:?}"; shift 2 ;;
      --engine) ENGINE_OVERRIDE="${2:?}"; shift 2 ;;
      --workflow-id) WORKFLOW_ID="${2:?}"; shift 2 ;;
      --image-id) IMAGE_ID="${2:?}"; shift 2 ;;
      --custom-image) CUSTOM_IMAGE_REF="${2:?}"; shift 2 ;;
      --base-image) BASE_IMAGE="${2:?}"; shift 2 ;;
      --nonconforming-image) NONCONFORMING_IMAGE="${2:?}"; shift 2 ;;
      --pg-port) PG_OVERRIDE="${2:?}"; shift 2 ;;
      --nats-port) NATS_OVERRIDE="${2:?}"; shift 2 ;;
      --control-port) CTRL_OVERRIDE="${2:?}"; shift 2 ;;
      --keep) KEEP=1; shift ;;
      --dry-run) DRY_RUN=1; shift ;;
      --preflight-only) PREFLIGHT_ONLY=1; shift ;;
      -h|--help) usage 0 ;;
      *) echo "runtime-e2e: unknown argument '$1' (see --help)" >&2; exit 2 ;;
    esac
  done
  case "$INSTANCE" in dev|prod|test) ;; *) echo "runtime-e2e: --instance must be dev, prod or test" >&2; exit 2 ;; esac
  [ -n "$EVIDENCE" ] || EVIDENCE="${REPO_ROOT}/qa-evidence/runtime-e2e/${UTC_STAMP}"
  case "$EVIDENCE" in /*) ;; *) EVIDENCE="${REPO_ROOT}/${EVIDENCE}" ;; esac
}

main() {
  parse_args "$@"
  if [ "$DRY_RUN" = "1" ]; then DRY_RUN_PLAN; exit 0; fi

  mkdir -p "$EVIDENCE"
  : >"${EVIDENCE}/results.tsv"
  : >"${EVIDENCE}/defects.tsv"
  trap teardown EXIT

  if ! step0_preflight; then
    write_summary
    say ""
    say "RUN INVALID: ${ABORT_REASON}"
    TEARDOWN_DONE=1
    exit 2
  fi
  if [ "$PREFLIGHT_ONLY" = "1" ]; then
    write_summary
    say "preflight only: this is a valid no-engine host. Re-run without --preflight-only for the session."
    TEARDOWN_DONE=1
    exit 0
  fi

  step1_local_run
  step2_opt_in
  step3_container_run
  step4_pristine_reset
  step5_pool_reuse
  step6_image_path
  step7_gate_check
  step8_all_native

  write_summary
  say ""
  if [ "$FAILS" -gt 0 ]; then
    say "RESULT: ${FAILS} assertion(s) failed — each is a defect against the file named above."
    say "        This item FILES defects against its siblings; it never patches them (AC10)."
    exit 1
  fi
  say "RESULT: the same workflow ran with no engine, then inside a container after opting in."
  exit 0
}

# Sourced (scripts/tests/runtime-e2e-assertions/run.sh) the FUNCTIONS are the
# subject and nothing may execute: the offline gate proves the vacuous-PASS rule
# without provisioning an engine, pulling an image, or binding a port.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then main "$@"; fi
