#!/usr/bin/env bash
# ============================================================================
# E2E acceptance harness for the PACKAGED DESKTOP APP claim:
# the installed app works on a clean machine with NO plane running, names the
# URL it tried, starts the plane itself, serves its own embedded UI, keeps a
# live execution STREAMING, and stops/starts the plane on demand.
#
# WHY THIS EXISTS. Every piece of this feature can pass its own test while the
# installed app is unusable: a package that installs but links a webview it does
# not declare, an offline panel that renders a blank window, a Start that returns
# before /healthz answers, a proxy that buffers and freezes a live execution, a
# Stop that reports success over a running plane. Only walking the whole path on
# a machine that has never had a plane on it puts those together. This is the
# acceptance procedure, not a component test.
#
# WHAT IT PROVES (installed package, NO plane at step 0).
#   1. The packaged app installs and its installed binary is the packaged one
#      (not the wails dev output under build/bin).
#   2. With no plane running, the offline state appears and NAMES the URL tried.
#   3. Its action starts the plane, waits for /healthz, and hands over to the UI.
#   4. The window shows the app's EMBEDDED build (version-coherent), and a forced
#      version mismatch is surfaced rather than swallowed.
#   5. A live execution streams and KEEPS updating — both in the window and by
#      the plane's own event count.
#   6. Unary data views load from the plane.
#   7. Stop and Start from the app both work, verified independently.
#   8. Windows 10 (WebView2) and Linux (WebKitGTK) say something precise instead
#      of showing a blank window.
#
# WHAT IS REAL HERE. The installed app, its window, the webview, the plane it
# starts, /healthz, /versionz, the PID listening on the control port and the
# plane's own API counts are all real. Every assertion records INDEPENDENT
# evidence beside whatever the app reports — the app's own word is never the
# proof.
#
# WHAT THIS HARNESS REFUSES TO CALL A PASS. An assertion that reads the CONTENT
# of a capture is gated (gated_absent/gated_contains): an EMPTY capture is SKIP,
# never PASS, because "the plane is down", "the banner is absent" and "the view
# did not freeze" are all trivially true of a capture that failed to run.
# scripts/tests/desktop-e2e-assertions/run.sh holds this harness to that rule
# offline, with no app, no plane and no display.
#
# USAGE
#   scripts/desktop-e2e.sh [options]
#     --instance dev|prod      instance the app should manage (default dev)
#     --evidence-dir DIR       evidence root (default qa-evidence/desktop-e2e/<UTC>)
#     --deb PATH               install this .deb (Linux)
#     --appimage PATH          use this AppImage (Linux, no install)
#     --portable-zip PATH      use this portable .zip (Windows manual sheet)
#     --platform linux|macos|windows   tier to prove (default: this host)
#     --workflow-id ID         workflow whose execution proves streaming
#     --control-port N         control-plane port override (default: shared table)
#     --keep                   leave the app and the plane running on exit
#     --preflight-only         run step 0 only (validity precondition), then stop
#     --dry-run                print the plan for every step, execute nothing
#     -h|--help
#
# EXIT CONTRACT
#   0  every step passed
#   1  at least one step failed (each FAIL names the file:line that owns it;
#      this item FILES defects against its siblings, it never patches them)
#   2  the run is not a valid proof of the claim (a plane is already answering,
#      the artifact is a dev build or missing, a GUI prerequisite is absent, or
#      the tree cannot produce a package) — nothing was proven
#
# This is a SESSION DRIVER, not an offline gate: it installs system packages,
# opens a GUI and binds real ports. `scripts/tests/` stays reserved for offline
# gates, and this is deliberately NOT part of `make ci`.
# ============================================================================
set -uo pipefail
# shellcheck disable=SC2155  # local x="$(...)" is deliberate: this script is -e-free

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/.." && pwd)"

UTC_STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
INSTANCE="dev"
EVIDENCE=""
ARTIFACT=""
ARTIFACT_KIND="auto"
PLATFORM=""
WORKFLOW_ID=""
CTRL_OVERRIDE=""
KEEP=0
DRY_RUN=0
PREFLIGHT_ONLY=0

PASSES=0
FAILS=0
SKIPS=0
ABORT_REASON=""
CURRENT_STEP="0"
SHELL_ORIGIN=""
PLANE_PID=""
DEFECT_SEQ=0

# The harness's own environment must not decide any case: a shell that launched a
# plane exports its ports, and a parent make hands down MAKEFLAGS.
unset MAKEFLAGS MFLAGS 2>/dev/null || true
unset ORCHICON_INSTANCE ORCHICON_CONTROL_PORT ORCHICON_POSTGRES_PORT 2>/dev/null || true
unset ORCHICON_NATS_PORT ORCHICON_URL ORCHICON_TOKEN 2>/dev/null || true

# --- output ----------------------------------------------------------------
say()   { printf '%s\n' "$*"; }
head2() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
ok2()   { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
bad2()  { printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
skip2() { printf '  \033[33mSKIP\033[0m %s\n' "$*"; }
dim()   { printf '  \033[2m%s\033[0m\n' "$*"; }

# --- evidence ---------------------------------------------------------------
record() { # record <status> <assertion> [owner] [evidence]
  printf '%s\t%s\t%s\t%s\t%s\n' "$CURRENT_STEP" "$2" "$1" "${4:-}" "${3:-}" \
    >>"${EVIDENCE}/results.tsv"
}

defect() { # defect <owning-task> <summary> [evidence]
  DEFECT_SEQ=$((DEFECT_SEQ + 1))
  printf 'D%s\t%s\t%s\t%s\n' "$DEFECT_SEQ" "$1" "${3:-}" "$2" >>"${EVIDENCE}/defects.tsv"
}

check() { # check <desc> <ok:0|1> <owner> [evidence]
  local desc="$1" ok="$2" owner="${3:-}" ev="${4:-}"
  if [ "$ok" = "0" ]; then
    PASSES=$((PASSES + 1)); record PASS "$desc" "" "$ev"; ok2 "$desc"
  else
    FAILS=$((FAILS + 1)); record FAIL "$desc" "$owner" "$ev"
    bad2 "$desc${owner:+   <- owner: $owner}"
    defect "${owner:-unowned}" "$desc" "$ev"
  fi
}

skip() { # skip <desc> <why> [owner]
  SKIPS=$((SKIPS + 1)); record SKIP "$1" "${3:-}" ""
  skip2 "$1 — $2"
  printf '  (skip reason: %s)\n' "$2" >>"${EVIDENCE}/$(step_file)"
}

check_contains() { # check_contains <desc> <file> <needle> <owner>
  local desc="$1" file="$2" needle="$3" owner="${4:-}"
  if [ -f "$file" ] && grep -qF -- "$needle" "$file" 2>/dev/null; then
    check "$desc" 0 "$owner" "${file#"$REPO_ROOT"/}"
  else
    check "$desc (missing '$needle' in ${file#"$REPO_ROOT"/})" 1 "$owner" "${file#"$REPO_ROOT"/}"
  fi
}

check_absent() { # check_absent <desc> <file> <needle> <owner>
  local desc="$1" file="$2" needle="$3" owner="${4:-}"
  if [ -f "$file" ] && grep -qF -- "$needle" "$file" 2>/dev/null; then
    check "$desc (found '$needle' in ${file#"$REPO_ROOT"/})" 1 "$owner" "${file#"$REPO_ROOT"/}"
  else
    check "$desc" 0 "$owner" "${file#"$REPO_ROOT"/}"
  fi
}

# --- content gates: an absence in an EMPTY capture is not evidence ----------
# The same hole the runtime/no-engine harnesses had: "the plane is down", "the
# banner is not shown", "the event list did not freeze" are ALL absence-shaped
# claims, and a capture that failed to run would manufacture a green proof of the
# very thing this item exists to disprove. Every content read goes through these:
# bytes get the real check, emptiness is SKIP.
# scripts/tests/desktop-e2e-assertions/run.sh holds the harness to it.
gated_absent() { # gated_absent <desc> <capture> <needle> <why-if-empty> [owner]
  local desc="$1" file="$2" needle="$3" why="$4" owner="${5:-}"
  if [ -s "$file" ]; then check_absent "$desc" "$file" "$needle" "$owner"
  else skip "$desc" "$why" "$owner"; fi
}

gated_contains() { # gated_contains <desc> <capture> <needle> <why-if-empty> [owner]
  local desc="$1" file="$2" needle="$3" why="$4" owner="${5:-}"
  if [ -s "$file" ]; then check_contains "$desc" "$file" "$needle" "$owner"
  else skip "$desc" "$why" "$owner"; fi
}

step_file() { printf 'step-%s.log' "$CURRENT_STEP"; }
step_log()  { printf '%s/%s' "$EVIDENCE" "$(step_file)"; }

begin_step() { # begin_step <n> <name>
  CURRENT_STEP="$1"
  head2 "step $1 — $2"
  : >"$(step_log)"
  dim "evidence: ${EVIDENCE}/$(step_file)"
}

tee_step() { # tee_step <cmd...> : run, tee stdout+stderr into the step log, return its status
  "$@" 2>&1 | tee -a "$(step_log)"
  return "${PIPESTATUS[0]}"
}

# --- host primitives --------------------------------------------------------
ctrl_port() { printf '%s' "${CTRL_OVERRIDE:-${ORCHICON_CONTROL_PORT:-8080}}"; }
control_url() { printf 'http://127.0.0.1:%s' "$(ctrl_port)"; }

# has_cmd <name> — 0 when the command exists on PATH.
has_cmd() { command -v "$1" >/dev/null 2>&1; }

# plane_answers — the INDEPENDENT liveness probe used everywhere. The app's own
# report is never the proof of the plane's state; this is.
plane_answers() { # plane_answers [path] [secs]
  local path="${1:-/healthz}" secs="${2:-2}"
  curl -fsS --max-time "$secs" "$(control_url)${path}" >/dev/null 2>&1
}

plane_get() { # plane_get <path> <out-file>
  curl -sS --max-time 10 -o "$2" -w '%{http_code}' "$(control_url)$1" 2>>"$(step_log)" || true
}

# listen_pid — the PID actually holding the control port. ss is not on a bare
# host, so fall back to the socket table (inode → /proc/<pid>/fd).
listen_pid() {
  local port
  port="$(ctrl_port)"
  if has_cmd ss; then
    ss -ltnp 2>/dev/null | awk -v p=":${port} " '$4 ~ p { if (match($0, /pid=[0-9]+/)) print substr($0, RSTART+4, RLENGTH-4) }' | head -1
    return 0
  fi
  local inode pid
  inode="$(awk -v want="$(printf '%04X' "$port")" 'NR>1 && $4=="0A" { n=split($2,a,":"); if (a[n]==want) { print $10; exit } }' /proc/net/tcp 2>/dev/null)"
  [ -n "$inode" ] || return 1
  for pid in /proc/[0-9]*; do
    ls -l "$pid/fd" 2>/dev/null | grep -q "socket:\[$inode\]" && { printf '%s' "${pid#/proc/}"; return 0; }
  done
  return 1
}

# --- need_* : preconditions that make the run INVALID (exit 2) ----------------
NEED_MISSING=""
need_cmd() { # need_cmd <cmd> <why>
  if has_cmd "$1"; then check "prerequisite: \`$1\` is available" 0 "" "host"; return 0; fi
  NEED_MISSING="${NEED_MISSING}${1} ($2); "
  skip "prerequisite: \`$1\` is available" "$2" "host"
  return 1
}

need_file() { # need_file <path> <why> <owner>
  if [ -e "$1" ]; then check "prerequisite: ${1#"$REPO_ROOT"/} exists" 0 "" "${1#"$REPO_ROOT"/}"; return 0; fi
  NEED_MISSING="${NEED_MISSING}${1#"$REPO_ROOT"/} ($2); "
  skip "prerequisite: ${1#"$REPO_ROOT"/} exists" "$2" "$3"
  return 1
}

# --- the desktop sources are on UNMERGED branches ---------------------------
# develop contains none of the desktop code. Everything this harness says about
# the owning SOURCE (WriteTimeout, FlushInterval, the injected SPA script) can
# only be checked in a tree that actually contains those files; in a tree that
# does not, the honest verdict is SKIP naming the unmerged dependency — never a
# PASS and never a FAIL against a file that is not there.
desktop_sources_present() { [ -f "${REPO_ROOT}/internal/desktop/proxy.go" ]; }

desktop_source_absent_why() {
  printf 'the desktop sources are not in this tree (%s); they live on the unmerged shell branch — nothing to inspect here' \
    'cmd/orchicon-desktop/, internal/desktop/'
}

src_contains() { # src_contains <desc> <repo-relative file> <needle> <owner>
  local desc="$1" rel="$2" needle="$3" owner="$4"
  if [ ! -f "${REPO_ROOT}/${rel}" ]; then
    skip "$desc" "$(desktop_source_absent_why) [${rel}]" "$owner"
    return 0
  fi
  gated_contains "$desc" "${REPO_ROOT}/${rel}" "$needle" "${rel} exists but is EMPTY" "$owner"
}

# --- the app window ---------------------------------------------------------
# A webview exposes no DOM to automation in this repo (no WebDriver/Playwright
# path for WebKitGTK), so a click is driven by COORDINATE, supplied by the
# operator with --click-map and recorded in the evidence. An assertion that
# needs a click nobody supplied is SKIP, never PASS.
CLICK_MAP=""
click_point() { # click_point <start|stop>
  local want="$1" pair
  [ -n "$CLICK_MAP" ] || return 1
  for pair in $CLICK_MAP; do
    case "$pair" in
      "$want"=*) printf '%s' "${pair#*=}"; return 0 ;;
    esac
  done
  return 1
}

gui_ready() { has_cmd xvfb-run && has_cmd xdotool && has_cmd import; }

GUI_PID=""
gui_launch() { # gui_launch <binary> [args...]
  local bin="$1"; shift
  local log
  log="$(step_log)"
  if [ -n "${DISPLAY:-}" ]; then
    "$bin" "$@" >>"$log" 2>&1 &
  else
    xvfb-run -a -s '-screen 0 1440x900x24' "$bin" "$@" >>"$log" 2>&1 &
  fi
  GUI_PID=$!
  sleep "${GUI_SETTLE_SECS:-8}"
}

gui_shot() { # gui_shot <name>
  local out="${EVIDENCE}/$1.png"
  if ! has_cmd import; then printf 'no import; screenshot %s not taken\n' "$1" >>"$(step_log)"; return 1; fi
  DISPLAY="${DISPLAY:-:99}" import -window root "$out" >>"$(step_log)" 2>&1
}

gui_click() { # gui_click <x,y>
  local xy="$1"
  xdotool mousemove "${xy%,*}" "${xy#*,}" click 1 >>"$(step_log)" 2>&1
}

gui_kill() {
  [ -n "$GUI_PID" ] || return 0
  kill "$GUI_PID" 2>/dev/null || true
  GUI_PID=""
}

# --- shell origin -----------------------------------------------------------
# The shell logs `desktop: SPA origin http://<addr> → plane <url>` before it
# serves anything; that line is how the harness learns the origin and the plane
# URL without guessing either.
shell_origin_from_log() { # shell_origin_from_log <logfile>
  [ -s "$1" ] || return 1
  grep -oE 'SPA origin http://[^ ]+' "$1" | head -1 | sed 's/.*SPA origin //'
}

plane_url_from_log() { # plane_url_from_log <logfile>
  [ -s "$1" ] || return 1
  grep -oE 'plane http://[^ ]+' "$1" | head -1 | sed 's/.*plane //'
}

wait_shell_origin() { # wait_shell_origin <logfile> <secs>
  local log="$1" secs="$2" i=0
  while [ "$i" -lt "$((secs * 2))" ]; do
    SHELL_ORIGIN="$(shell_origin_from_log "$log" || true)"
    [ -n "$SHELL_ORIGIN" ] && return 0
    sleep 0.5; i=$((i + 1))
  done
  return 1
}

# --- step 0 -----------------------------------------------------------------
step0_preflight() {
  begin_step 0 "the validity precondition (nothing proven if any of this fails)"

  # 0a. the harness's own honesty gate runs first, offline.
  local gate="${REPO_ROOT}/scripts/tests/desktop-e2e-assertions/run.sh"

  # The gate must not recurse into itself when IT is the one driving the harness.
  if [ "${ORCHICON_DESKTOP_E2E_NO_GATE:-0}" = "1" ]; then
    skip "the harness passes its own offline honesty gate" \
      "ORCHICON_DESKTOP_E2E_NO_GATE=1 (a gate run must not recurse)" "scripts/tests/desktop-e2e-assertions/run.sh"
  elif [ -f "$gate" ]; then
    if tee_step bash "$gate" >/dev/null 2>&1; then
      check "the harness passes its own offline honesty gate" 0 "" "scripts/tests/desktop-e2e-assertions/run.sh"
    else
      check "the harness passes its own offline honesty gate" 1 \
        "scripts/tests/desktop-e2e-assertions/run.sh:1" "$(step_log)"
    fi
  else
    skip "the harness passes its own offline honesty gate" "the gate is not in this tree" ""
  fi


  # 0b. NO PLANE. This is the whole premise; if one answers, the run proves nothing.
  if plane_answers /healthz 2; then
    abort_run "a plane is already answering $(control_url)/healthz — this run cannot prove the no-plane offline state"
    return 0
  fi
  check "no plane answers on $(control_url)/healthz (the premise of the claim)" 0 "" "$(control_url)/healthz"

  # 0c. the artifact is a PACKAGE, and the binary is the PACKAGED one, not a dev build.
  local bin=""
  if [ -n "$ARTIFACT" ] && [ -e "$ARTIFACT" ]; then
    check "the artifact to install is a real file" 0 "" "${ARTIFACT#"$REPO_ROOT"/}"
  elif [ -f /usr/lib/orchicon/orchicon-desktop ] || [ -f /usr/bin/orchicon-desktop ]; then
    check "an installed orchicon-desktop is present (no artifact given)" 0 "" "dpkg -L orchicon-desktop"
  else
    # No artifact and no install: the tree would have to produce one. It cannot
    # while the packaging task expects the shell at repo-root desktop/ and the
    # shell lives at cmd/orchicon-desktop/ — defect D1, filed against packaging.
    if [ -d "${REPO_ROOT}/deploy/desktop" ] && [ ! -d "${REPO_ROOT}/desktop" ] && [ -d "${REPO_ROOT}/cmd/orchicon-desktop" ]; then
      defect "desktop-packaging" "D1: packaging packages desktop/build/bin/orchicon-desktop but the shell lives at cmd/orchicon-desktop/, so make desktop-package-linux cannot produce a package" \
        "deploy/desktop/nfpm.yaml:42 · Makefile desktop targets · .github/workflows/release.yml"
      abort_run "no artifact given and the tree cannot produce one (defect D1: shell at cmd/orchicon-desktop/, packaging expects desktop/) — pass --deb/--appimage"
      return 0
    fi
    abort_run "no artifact given, nothing installed, and no packaging sources in this tree — pass --deb/--appimage/--portable-zip"
    return 0
  fi

  if [ -z "$ARTIFACT" ]; then
    bin="$(installed_binary)"
    if is_dev_build "$bin"; then
      abort_run "the installed binary is the WAILS DEV BUILD ($bin); a dev build proves nothing about packaging or the embedded UI"
      return 0
    fi
    check "the launched binary is the PACKAGED one (not a build/bin dev build)" 0 "" "$bin"
  elif [ -n "$ARTIFACT" ]; then
    if is_dev_build "$ARTIFACT"; then
      abort_run "--deb/--appimage points at the WAILS DEV BUILD ($ARTIFACT); a dev build is not admissible"
      return 0
    fi
    check "the artifact is a package, not a dev build" 0 "" "${ARTIFACT#"$REPO_ROOT"/}"
  fi

  # 0d. GUI automation for the Linux automated tier. Absent is exit 2, never a PASS.
  if [ "$PLATFORM" = "linux" ]; then
    need_cmd xvfb-run "the Linux automated tier needs a headless display"
    need_cmd xdotool "the Linux automated tier needs to click the offline action"
    need_cmd import "evidence screenshots are required (AC9)"
    local missing=""
    if [ -z "${DISPLAY:-}" ] && ! has_cmd xvfb-run; then missing="xvfb-run "; fi
    if [ -n "$NEED_MISSING" ]; then
      abort_run "the Linux GUI tier cannot run on this host: missing ${NEED_MISSING} — the webview cannot be driven, so no step below could be proven"
      return 0
    fi
  fi

  # 0e. the desktop sources themselves (unmerged ⇒ SKIP, recorded).
  if desktop_sources_present; then
    check "the desktop sources are in this tree" 0 "" "internal/desktop/proxy.go"
  else
    skip "the desktop sources are in this tree" "$(desktop_source_absent_why)" "cmd/orchicon-desktop/"
  fi
  return 0
}

abort_run() { # abort_run <why>
  ABORT_REASON="$1"
  SKIPS=$((SKIPS + 1))
  record SKIP "the run is a valid proof of this claim" "" ""
  printf '  (abort reason: %s)\n' "$1" >>"$(step_log)"
  bad2 "RUN INVALID: $1"
}

installed_binary() {
  local p
  for p in /usr/lib/orchicon/orchicon-desktop /usr/bin/orchicon-desktop \
           "${REPO_ROOT}/desktop/build/bin/orchicon-desktop" \
           "${REPO_ROOT}/cmd/orchicon-desktop/build/bin/orchicon-desktop"; do
    [ -x "$p" ] && { printf '%s' "$p"; return 0; }
  done
  return 1
}

is_dev_build() { # is_dev_build <path>
  case "$1" in
    */build/bin/*) return 0 ;;
    *) return 1 ;;
  esac
}

# --- evidence helpers for the GUI ----------------------------------------
shot_assert() { # shot_assert <png-name> <desc> <owner>
  local png="${EVIDENCE}/$1.png"
  gui_shot "$1"
  if [ -s "$png" ] && [ "$(wc -c <"$png")" -gt 1024 ]; then
    check "$2" 0 "$3" "${png#"$REPO_ROOT"/}"
  else
    skip "$2" "no screenshot was captured (no display or import, or the window never mapped)" "$3"
  fi
}

# ocr_assert — an assertion about RENDERED TEXT. Without an OCR engine the
# honest verdict is SKIP: the screenshot is evidence for a human, not a machine
# verdict, and calling it a PASS would be the exact dishonesty this gate exists
# to prevent.
ocr_assert() { # ocr_assert <png-name> <desc> <needle> <owner>
  local png="${EVIDENCE}/$1.png" text="${EVIDENCE}/$1.ocr.txt"
  if [ ! -s "$png" ]; then skip "$2" "no screenshot to read" "$3"; return 0; fi
  if ! has_cmd tesseract; then
    skip "$2" "no OCR engine on this host (tesseract absent) — the rendered text is evidenced by ${png#"$REPO_ROOT"/} for human review" "$3"
    return 0
  fi
  tesseract "$png" stdout >"$text" 2>>"$(step_log)" || true
  gated_contains "$2" "$text" "$4" "OCR produced no text" "$3"
}

manual_sheet() { # manual_sheet <os>
  local os="$1" out="${EVIDENCE}/${CURRENT_STEP}-manual-${os}.md"
  {
    printf '# Manual evidence sheet — %s\n\n' "$os"
    printf 'This harness cannot install or drive a %s machine from here. Every step below is\n' "$os"
    printf 'recorded SKIP until a human attaches the screenshots. Do NOT mark them PASS.\n\n'
    printf 'Run, on a clean %s machine with NO plane running:\n\n' "$os"
    case "$os" in
      windows)
        printf '1. Install the packaged app from the `.exe` (WebView2 Evergreen bootstrapper path —\n   `deploy/desktop/windows/README-WINDOWS.txt`). On a Windows 10 host with WebView2\n   ABSENT, capture the dialog: a blank window is a FAIL, a precise instruction is a PASS.\n'
        printf '2. Launch from *Apps \u0026 features* (per-user install), NOT a dev build.\n'
        printf '3. Offline state: screenshot the panel; it must name the URL it tried.\n'
        printf '4. Click Start; wait for the UI. `curl http://127.0.0.1:<port>/healthz` must answer.\n'
        printf '5. Start a workflow; screenshot the live view at t0 / t0+10s / t0+20s — it must\n   keep updating, not freeze.\n'
        printf '6. Open Projects and Work Items.\n'
        printf '7. Stop from the app: the plane must go down (Task Manager / `curl` fails), then Start.\n' ;;
      macos)
        printf '1. Install `/Applications/Orchicon.app` from the `.dmg`; open it (no right-click\n   ceremony, or a precise Gatekeeper instruction — `deploy/desktop/macos/sign-and-notarize.sh`).\n'
        printf '2. With no plane running, screenshot the offline state (must name the URL).\n'
        printf '3. Click Start; UI hands over; `curl http://127.0.0.1:<port>/healthz` answers.\n'
        printf '4. Start a workflow; screenshots at t0 / t0+10s / t0+20s must differ.\n'
        printf '5. Projects / Work Items load.\n'
        printf '6. Stop then Start from the app, each verified with `curl` and `pgrep`.\n' ;;
      linux) printf '1. Run the launcher on a host WITHOUT WebKitGTK: it must print a precise message.\n' ;;
    esac
    printf '\nAttach the screenshots to the work item and paste the `curl` output verbatim (AC9).\n'
  } >"$out"
  skip "the ${os} tier was proven on a real ${os} machine" "this harness cannot install or drive ${os} from here — see ${out#"$REPO_ROOT"/}" "deploy/desktop/"
}

# --- step 1 -----------------------------------------------------------------
step1_install() {
  begin_step 1 "install the PACKAGED app (not a dev build) (AC1)"
  case "$PLATFORM" in
    macos|windows)
      manual_sheet "$PLATFORM"
      return 0 ;;
  esac

  if [ "$ARTIFACT_KIND" = "deb" ] && [ -n "$ARTIFACT" ]; then
    if tee_step env DEBIAN_FRONTEND=noninteractive apt-get install -y "$ARTIFACT"; then
      check "the .deb installs without error" 0 "" "${ARTIFACT#"$REPO_ROOT"/}"
    else
      check "the .deb installs without error" 1 "deploy/desktop/nfpm.yaml:1" "${ARTIFACT#"$REPO_ROOT"/}"
    fi
  fi

  dpkg -L orchicon-desktop >"${EVIDENCE}/01-dpkg-L.txt" 2>&1 || true
  gated_contains "the package owns the launcher /usr/bin/orchicon-desktop" \
    "${EVIDENCE}/01-dpkg-L.txt" "/usr/bin/orchicon-desktop" \
    "dpkg -L produced nothing — the package is not installed" "deploy/desktop/nfpm.yaml:38-45"
  gated_contains "the package owns the shell binary /usr/lib/orchicon/orchicon-desktop" \
    "${EVIDENCE}/01-dpkg-L.txt" "/usr/lib/orchicon/orchicon-desktop" \
    "dpkg -L produced nothing — the package is not installed" "deploy/desktop/nfpm.yaml:38-45"

  dpkg -s orchicon-desktop >"${EVIDENCE}/01-dpkg-s.txt" 2>&1 || true
  gated_contains "dpkg reports an installed version for orchicon-desktop" \
    "${EVIDENCE}/01-dpkg-s.txt" "Version:" "dpkg -s produced nothing" "deploy/desktop/nfpm.yaml:1"

  local bin; bin="$(installed_binary || true)"
  if [ -n "$bin" ]; then
    sha256sum "$bin" >"${EVIDENCE}/01-sha256.txt" 2>&1 || true
    check "the installed binary's sha256 is recorded" 0 "" "${EVIDENCE}/01-sha256.txt"
    check "the installed binary is not the wails dev build" "$(is_dev_build "$bin" && echo 1 || echo 0)" \
      "cmd/orchicon-desktop/main.go:1" "$bin"
  else
    skip "the installed binary is present" "no /usr/lib/orchicon/orchicon-desktop on this host" "deploy/desktop/nfpm.yaml:38-45"
  fi

  ldconfig -p >"${EVIDENCE}/01-ldconfig.txt" 2>/dev/null || true
  gated_contains "the host provides the WebKitGTK soname the package declares" \
    "${EVIDENCE}/01-ldconfig.txt" "libwebkit2gtk-4." \
    "the loader cache could not be read" "deploy/desktop/nfpm.yaml:32-34"
  return 0
}

# --- step 2 -----------------------------------------------------------------
step2_offline_state() {
  begin_step 2 "with NO plane running, the offline state names the URL it tried (AC2)"
  if [ "$PLATFORM" != "linux" ]; then manual_sheet "$PLATFORM"; return 0; fi
  if ! gui_ready; then
    skip "the offline state appears and names the URL" "the GUI prerequisites are absent (xvfb-run/xdotool/import)" "frontend/src/routes/manage-server.tsx:250-254"
    return 0
  fi

  local bin log; bin="$(installed_binary || true)"; log="$(step_log)"
  if [ -z "$bin" ]; then
    skip "the offline state appears and names the URL" "nothing is installed to launch" "deploy/desktop/nfpm.yaml:38-45"
    return 0
  fi

  gui_launch "$bin"
  if ! wait_shell_origin "$log" 20; then
    check "the shell logged its SPA origin and the plane URL it will use" 1 \
      "internal/desktop/shell.go:150" "$(step_log)"
    return 0
  fi
  check "the shell logged its SPA origin and the plane URL it will use" 0 "" "${SHELL_ORIGIN}"

  local tried; tried="$(plane_url_from_log "$log" || true)"
  printf 'shell origin: %s\nplane url tried: %s\n' "$SHELL_ORIGIN" "${tried:-<not logged>}" >>"$log"

  # (1) the proxy's own failure body names the URL it could not reach.
  local body="${EVIDENCE}/02-proxy-healthz.txt"
  curl -sS --max-time 5 -o "$body" -w '%{http_code}\n' "${SHELL_ORIGIN}/healthz" \
    >"${EVIDENCE}/02-proxy-healthz.status" 2>>"$log" || true
  gated_contains "the proxy's failure body names the plane URL it tried" "$body" \
    "${tried:-127.0.0.1}" "the proxy returned no body at all" "internal/desktop/proxy.go:74-79"
  gated_contains "the proxy answers 502 rather than hanging or returning an empty page" \
    "${EVIDENCE}/02-proxy-healthz.status" "502" "the proxy was never reached" "internal/desktop/proxy.go:74-79"

  # (2) the shell's own log says the plane is unreachable.
  gated_contains "the shell logged that the plane version was unavailable" "$log" \
    "plane version unavailable" "the shell logged nothing" "internal/desktop/shell.go:83-87"

  # (3) the rendered panel, for human review (and machine-read when OCR exists).
  shot_assert "02-offline-panel" "a screenshot of the offline state was captured" "frontend/src/routes/manage-server.tsx:250-254"
  ocr_assert "02-offline-panel" "the offline panel names the URL it tried" "${tried:-http://127.0.0.1}" \
    "frontend/src/routes/manage-server.tsx:250-254"
  return 0
}

# --- step 3 -----------------------------------------------------------------
step3_start_from_app() {
  begin_step 3 "the offline action starts the plane, waits for /healthz, hands over (AC3)"
  if [ "$PLATFORM" != "linux" ]; then manual_sheet "$PLATFORM"; return 0; fi

  local pt
  if ! pt="$(click_point start)"; then
    skip "the Start action was driven from the window" \
      "no --click-map start=X,Y was supplied and a webview exposes no DOM to this repo's automation" \
      "frontend/src/routes/manage-server.tsx:308-315"
    return 0
  fi
  gui_click "$pt"
  printf 'drove Start by coordinate click at %s\n' "$pt" >>"$(step_log)"
  check "the Start action was driven from the window" 0 "" "click $pt"

  local i=0 up=1
  while [ "$i" -lt 120 ]; do
    if plane_answers /healthz 2; then up=0; break; fi
    sleep 0.5; i=$((i + 1))
  done
  check "$(control_url)/healthz answers after the app's Start action" "$up" \
    "internal/planebind/service.go:182" "$(control_url)/healthz"
  [ "$up" = 0 ] || return 0

  plane_get /versionz "${EVIDENCE}/03-versionz.txt" >"${EVIDENCE}/03-versionz.status" 2>>"$(step_log)" || true
  gated_contains "the plane reports its own version (/versionz)" "${EVIDENCE}/03-versionz.txt" \
    "version" "the plane answered /versionz with nothing" "internal/api/api.go:193"

  PLANE_PID="$(listen_pid || true)"
  printf '%s\n' "${PLANE_PID:-<no pid found>}" >"${EVIDENCE}/03-listen-pid.txt"
  if [ -n "$PLANE_PID" ]; then
    check "a PID actually holds the control port (independent of the app)" 0 "" "pid ${PLANE_PID}"
  else
    skip "a PID actually holds the control port" "ss is absent and the socket table had no match" "internal/controlport"
  fi

  shot_assert "03-handover" "the window handed over to the app UI after Start" "frontend/src/routes/manage-server.tsx:308-315"
  return 0
}

# --- step 4 -----------------------------------------------------------------
step4_embedded_ui() {
  begin_step 4 "the window serves the app's EMBEDDED build, and a mismatch would be surfaced (AC4)"
  if [ "$PLATFORM" != "linux" ]; then manual_sheet "$PLATFORM"; return 0; fi
  if [ -z "$SHELL_ORIGIN" ]; then
    skip "the shell origin is known" "the app was never launched (see step 2)" "internal/desktop/shell.go:150"
    return 0
  fi

  curl -sS --max-time 10 -o "${EVIDENCE}/04-shell-index.html" "${SHELL_ORIGIN}/" 2>>"$(step_log)" || true
  gated_contains "the shell injects __ORCHICON_DESKTOP__ into index.html (embedded build)" \
    "${EVIDENCE}/04-shell-index.html" "__ORCHICON_DESKTOP__" "the shell served nothing" "internal/desktop/spa.go:60-62"

  local plane; plane="$(plane_url_from_log "$(step_log)" || true)"
  if [ -n "$plane" ]; then
    curl -sS --max-time 10 -o "${EVIDENCE}/04-plane-index.html" "${plane}/" 2>>"$(step_log)" || true
    gated_absent "the plane's own / does NOT inject the desktop marker (it is not the app's UI)" \
      "${EVIDENCE}/04-plane-index.html" "__ORCHICON_DESKTOP__" "the plane served nothing" "internal/desktop/spa.go:88-110"
  else
    skip "the plane's own / does NOT inject the desktop marker" "the shell never logged the plane URL" "internal/desktop/spa.go:88-110"
  fi

  src_contains "the shell's info JSON carries the app tag" internal/desktop/info.go "appTag" "internal/desktop/info.go:45"
  src_contains "a version mismatch is only set when both tags are known and differ" \
    internal/desktop/info.go "VersionMismatch" "internal/desktop/info.go:76-84"

  # A mismatch must be SURFACED, and proving it requires FORCING one.
  if [ -n "${ORCHICON_DESKTOP_E2E_OTHER_PLANE:-}" ]; then
    local bin; bin="$(installed_binary || true)"
    gui_kill
    gui_launch "$bin" --plane "${ORCHICON_DESKTOP_E2E_OTHER_PLANE}"
    curl -sS --max-time 10 -o "${EVIDENCE}/04-mismatch-index.html" "${SHELL_ORIGIN}/" 2>>"$(step_log)" || true
    gated_contains "a forced version mismatch sets versionMismatch:true" \
      "${EVIDENCE}/04-mismatch-index.html" '"versionMismatch":true' \
      "the shell served nothing after the relaunch" "internal/desktop/info.go:76-84"
    shot_assert "04-version-mismatch-banner" "the version-mismatch banner was captured" \
      "frontend/src/components/version-mismatch-banner.tsx:14-26"
  else
    skip "a forced version mismatch sets versionMismatch:true" \
      "no ORCHICON_DESKTOP_E2E_OTHER_PLANE was given — a run that only ever sees matching tags proves nothing here" \
      "internal/desktop/info.go:76-84"
  fi
  return 0
}

# --- step 5 -----------------------------------------------------------------
step5_streaming() {
  begin_step 5 "a live execution STREAMS and keeps updating (AC5 — the transport risk)"

  # (1) the static invariants that make streaming possible at all.
  src_contains "the shell's HTTP server has NO write timeout (a deadline truncates a stream)" \
    internal/desktop/shell.go "WriteTimeout: 0" "internal/desktop/shell.go:137-142"
  src_contains "the proxy flushes every write instead of buffering the stream" \
    internal/desktop/proxy.go "FlushInterval: -1" "internal/desktop/proxy.go:73"

  # (2) the Go-level live proof, when the sources and a reachable plane exist.
  if desktop_sources_present && plane_answers /healthz 2; then
    if ORCHICON_DESKTOP_PLANE_URL="$(control_url)" \
       tee_step go test -tags desktop_e2e ./internal/desktop -run TestLive -v -count=1; then
      check "the Go transport streams three events spread over time" 0 "" "internal/desktop/e2e_live_test.go:68-105"
    else
      check "the Go transport streams three events spread over time" 1 "internal/desktop/e2e_live_test.go:68-105" "$(step_log)"
    fi
  else
    skip "the Go transport streams three events spread over time" \
      "the desktop sources are not in this tree and/or no plane is answering" "internal/desktop/e2e_live_test.go:68-105"
  fi

  # (3) the webview-level proof: the VISIBLE view must keep changing while the
  #     plane's own event feed also keeps producing. Both must grow; a view that
  #     froze is the specific failure this step exists to catch.
  if [ -z "$WORKFLOW_ID" ] && [ -z "${EXECUTION_ID:-}" ]; then
    skip "the visible event list keeps updating during a live execution" \
      "no --workflow-id/--execution-id was given, so there was no live execution to watch" \
      "frontend/src/ (execution view)"
    return 0
  fi
  if ! plane_answers /healthz 2; then
    skip "the visible event list keeps updating during a live execution" \
      "no plane is answering, so no execution could stream" "frontend/src/ (execution view)"
    return 0
  fi

  # the plane's OWN feed: events must keep arriving over the window, not one lump.
  local feed="${EVIDENCE}/05-plane-events.txt"
  curl -sS -N --max-time 20 \
    -H 'Content-Type: application/json' \
    -d "{\"executionId\":\"${EXECUTION_ID:-}\",\"workflowId\":\"${WORKFLOW_ID}\"}" \
    "$(control_url)/orchicon.api.v1.ExecutionService/StreamExecutionEvents" \
    >"$feed" 2>>"$(step_log)" || true
  local lines; lines="$(wc -l <"$feed" 2>/dev/null || echo 0)"
  printf 'plane event stream lines in 20s: %s\n' "$lines" >>"$(step_log)"
  if [ -s "$feed" ]; then
    check "the plane's own event feed produced more than one event in 20s" \
      "$([ "$lines" -gt 1 ] && echo 0 || echo 1)" \
      "internal/desktop/proxy.go:73" "${feed#"$REPO_ROOT"/}"
  else
    skip "the plane's own event feed produced more than one event in 20s" \
      "the stream returned nothing (no such execution, or the plane refused)" "internal/desktop/proxy.go:73"
  fi

  # the WINDOW's view: three samples, each a different frame, or the view froze.
  if ! gui_ready; then
    skip "the visible event list keeps updating" "the GUI prerequisites are absent" "frontend/src/ (execution view)"
    return 0
  fi
  local a b c
  gui_shot "05-stream-t0"
  sleep 10; gui_shot "05-stream-t10"
  sleep 10; gui_shot "05-stream-t20"
  a="$(sha256sum "${EVIDENCE}/05-stream-t0.png" 2>/dev/null | cut -d' ' -f1)"
  b="$(sha256sum "${EVIDENCE}/05-stream-t10.png" 2>/dev/null | cut -d' ' -f1)"
  c="$(sha256sum "${EVIDENCE}/05-stream-t20.png" 2>/dev/null | cut -d' ' -f1)"
  printf 'frame hashes t0=%s t10=%s t20=%s\n' "${a:-none}" "${b:-none}" "${c:-none}" >>"$(step_log)"
  if [ -z "$a" ] || [ -z "$b" ] || [ -z "$c" ]; then
    skip "the visible view changed between t0, t0+10s and t0+20s" "no screenshots were captured" "frontend/src/ (execution view)"
  else
    check "the visible view changed between t0, t0+10s and t0+20s (it did not freeze)" \
      "$([ "$a" != "$b" ] && [ "$b" != "$c" ] && echo 0 || echo 1)" \
      "internal/desktop/proxy.go:73" "05-stream-t0.png, 05-stream-t10.png, 05-stream-t20.png"
  fi
  return 0
}

# --- step 6 -----------------------------------------------------------------
step6_unary_views() {
  begin_step 6 "unary data views load from the plane (AC6)"
  if ! plane_answers /healthz 2; then
    skip "the projects and work-items views load real rows" "no plane is answering" "frontend/src/api/"
    return 0
  fi

  local projects="${EVIDENCE}/06-plane-projects.json"
  curl -sS --max-time 20 -H 'Content-Type: application/json' \
    -d "{\"pageSize\":50}" \
    "$(control_url)/orchicon.api.v1.ProjectService/ListProjects" \
    -o "$projects" 2>>"$(step_log)" || true
  gated_contains "the plane answers ListProjects (the same unary call the view makes)" \
    "$projects" "projects" "ListProjects returned nothing" "frontend/src/api/"

  local rows; rows="$(grep -o '"id"' "$projects" 2>/dev/null | wc -l | tr -d ' ')"
  printf 'plane project rows: %s\n' "${rows:-0}" >>"$(step_log)"
  if [ "${rows:-0}" -gt 0 ]; then
    shot_assert "06-projects-view" "the projects view was captured for comparison with the plane's rows" "frontend/src/api/"
  else
    skip "the projects view rendered rows the plane actually has" \
      "the plane itself has zero projects — an empty table would prove nothing" "frontend/src/api/"
  fi

  # Work items: the same shape, and the same refusal to call an empty table a PASS.
  local wis="${EVIDENCE}/06-plane-work-items.json"
  curl -sS --max-time 20 -H 'Content-Type: application/json' -d "{\"pageSize\":50}" \
    "$(control_url)/orchicon.api.v1.WorkItemService/ListWorkItems" -o "$wis" 2>>"$(step_log)" || true
  gated_contains "the plane answers ListWorkItems" "$wis" "workItems" "ListWorkItems returned nothing" "frontend/src/api/"
  local wrows; wrows="$(grep -o '"id"' "$wis" 2>/dev/null | wc -l | tr -d ' ')"
  if [ "${wrows:-0}" -gt 0 ]; then
    shot_assert "06-work-items-view" "the work-items view was captured for comparison with the plane's rows" "frontend/src/api/"
  else
    skip "the work-items view rendered rows the plane actually has" \
      "the plane itself has zero work items — an empty table would prove nothing" "frontend/src/api/"
  fi
  return 0
}

# --- step 7 -----------------------------------------------------------------
step7_control_both_ways() {
  begin_step 7 "Stop and Start from the app both work, verified independently (AC7)"
  if [ "$PLATFORM" != "linux" ]; then manual_sheet "$PLATFORM"; return 0; fi
  local pt
  if ! pt="$(click_point stop)"; then
    skip "Stop from the window takes the plane down" \
      "no --click-map stop=X,Y was supplied and a webview exposes no DOM to this repo's automation" \
      "frontend/src/routes/manage-server.tsx:323-329"
    return 0
  fi

  gui_click "$pt"
  printf 'drove Stop by coordinate click at %s\n' "$pt" >>"$(step_log)"
  local i=0 down=1
  while [ "$i" -lt 60 ]; do
    if ! plane_answers /healthz 1; then down=0; break; fi
    sleep 0.5; i=$((i + 1))
  done
  check "$(control_url)/healthz stops answering after Stop" "$down" \
    "internal/planecontrol/planecontrol.go:365" "$(control_url)/healthz"

  if [ -n "$PLANE_PID" ]; then
    if kill -0 "$PLANE_PID" 2>/dev/null; then
      check "the PID that held the port is GONE (independent of the app's report)" 1 \
        "internal/planecontrol/planecontrol.go:365" "pid ${PLANE_PID} still alive"
    else
      check "the PID that held the port is GONE (independent of the app's report)" 0 "" "pid ${PLANE_PID}"
    fi
  else
    skip "the PID that held the port is GONE" "no PID was recorded in step 3" "internal/planecontrol/planecontrol.go:365"
  fi

  local start_pt
  if ! start_pt="$(click_point start)"; then
    skip "Start from the window brings the plane back" "no --click-map start=X,Y was supplied" \
      "frontend/src/routes/manage-server.tsx:308-315"
    return 0
  fi
  gui_click "$start_pt"
  i=0; local up=1
  while [ "$i" -lt 120 ]; do
    if plane_answers /healthz 2; then up=0; break; fi
    sleep 0.5; i=$((i + 1))
  done
  check "$(control_url)/healthz answers again after Start" "$up" \
    "internal/planecontrol/planecontrol.go:284-298" "$(control_url)/healthz"
  return 0
}

# --- step 8 -----------------------------------------------------------------
step8_platform_variants() {
  begin_step 8 "Windows 10 (WebView2) and Linux (WebKitGTK) say something precise (AC8)"
  case "$PLATFORM" in
    windows|macos)
      manual_sheet "$PLATFORM"
      return 0 ;;
  esac

  # Linux: a host WITHOUT WebKitGTK must be told so clearly, not die in the loader.
  local launcher="${REPO_ROOT}/deploy/desktop/linux/orchicon-desktop"
  if [ -f "$launcher" ]; then
    # Hide WebKitGTK from the loader's view with a `ldconfig` shim on PATH, then run
    # the launcher: it must NAME WebKitGTK rather than die in a loader error.
    mkdir -p "${EVIDENCE}/shim"
    cp "${REPO_ROOT}/scripts/tests/desktop-e2e-assertions/cases/fake-ldconfig-empty" "${EVIDENCE}/shim/ldconfig" 2>/dev/null || true
    chmod +x "${EVIDENCE}/shim/ldconfig" 2>/dev/null || true
    PATH="${EVIDENCE}/shim:${PATH}" LD_LIBRARY_PATH=/nonexistent \
      sh "$launcher" >"${EVIDENCE}/08-launcher-nowebkit.txt" 2>&1 || true
    gated_contains "the Linux launcher names WebKitGTK when it is absent" \
      "${EVIDENCE}/08-launcher-nowebkit.txt" "WebKitGTK" "the launcher printed nothing" "deploy/desktop/linux/orchicon-desktop:34-51"
  else
    skip "the Linux launcher names WebKitGTK when it is absent" \
      "deploy/desktop/linux/orchicon-desktop is not in this tree (unmerged packaging branch)" \
      "deploy/desktop/linux/orchicon-desktop:34-51"
  fi

  # Windows: the preflight dialog must name the remedy URL, never a blank window.
  local preflight="${REPO_ROOT}/internal/desktoppreflight/preflight.go"
  if [ -f "$preflight" ]; then
    gated_contains "the Windows preflight names a WebView2 remedy URL" "$preflight" "http" \
      "the preflight file is empty" "internal/desktoppreflight/preflight.go:38"
    gated_contains "the Windows preflight reports, rather than failing silently" "$preflight" "func (r Report) String" \
      "the preflight file is empty" "internal/desktoppreflight/preflight.go:213-229"
  else
    skip "the Windows preflight names a WebView2 remedy URL" \
      "internal/desktoppreflight/ is not in this tree (unmerged packaging branch)" "internal/desktoppreflight/preflight.go:38"
  fi
  return 0
}

# --- summary + driver -------------------------------------------------------
write_summary() {
  local verdict
  if [ -n "$ABORT_REASON" ]; then verdict="RUN INVALID — nothing was proven"
  elif [ "$FAILS" -gt 0 ]; then verdict="FAIL — ${FAILS} assertion(s) failed"
  else verdict="PASS — ${PASSES} assertion(s) passed"; fi
  {
    printf '# desktop-e2e — packaged app, clean machine, no plane, streaming\n\n'
    printf 'Run: `%s` · instance `%s` · platform `%s` · control port `%s`\n\n' "$UTC_STAMP" "$INSTANCE" "$PLATFORM" "$(ctrl_port)"
    printf '**Verdict: %s**\n\n' "$verdict"
    printf '| status | count |\n|---|---|\n| PASS | %s |\n| FAIL | %s |\n| SKIP | %s |\n\n' "$PASSES" "$FAILS" "$SKIPS"
    printf 'A SKIP is UNPROVEN, never a pass.\n\n'
    if [ -s "${EVIDENCE}/results.tsv" ]; then
      printf '## Per-step\n\n| step | PASS | FAIL | SKIP |\n|---|---|---|---|\n'
      awk -F'\t' 'NR>1 { k=$1; if ($3=="PASS") p[k]++; else if ($3=="FAIL") f[k]++; else s[k]++ }
        END { for (k in p) seen[k]=1; for (k in f) seen[k]=1; for (k in s) seen[k]=1;
              for (k in seen) printf "| %s | %d | %d | %d |\n", k, p[k], f[k], s[k] }' \
        "${EVIDENCE}/results.tsv" | sort -t'|' -k2 -n
    fi
    printf '\n## Defects filed (against the OWNING sibling, never patched here)\n\n'
    if [ -s "${EVIDENCE}/defects.tsv" ]; then
      awk -F'\t' 'NR>1 { printf "- **%s** %s — %s (`%s`)\n", $1, $2, $4, $3 }' "${EVIDENCE}/defects.tsv"
    else
      printf 'None.\n'
    fi
    if [ -n "$ABORT_REASON" ]; then printf '\n## Why this run is not a proof\n\n%s\n' "$ABORT_REASON"; fi
  } >"${EVIDENCE}/summary.md"
}

write_notes_template() {
  [ -f "${EVIDENCE}/NOTES.md" ] && return 0
  {
    printf '# What this evidence is — and what it is not\n\n'
    printf 'Produced by `scripts/desktop-e2e.sh` at %s on `%s`.\n\n' "$UTC_STAMP" "$(uname -sr 2>/dev/null || echo unknown)"
    printf 'Read `summary.md` for the verdict and `results.tsv` for every row. A SKIP is\nUNPROVEN: most claims in this item are absence-shaped and are trivially true of a\ncapture that failed to run, which is why `make desktop-e2e-assertions-test` pins the\nrule that an empty capture can never produce a PASS.\n\n'
    printf '## Known limits of a harness run from inside this repo\n\n'
    printf -- '- macOS and Windows tiers are manual evidence sheets; they are SKIP here.\\n'
    printf -- '- A webview exposes no DOM to the automation this repo has, so clicks are driven\n  by `--click-map` coordinates; without one the click assertions are SKIP.\n'
    printf -- '- Rendered-text assertions need an OCR engine (`tesseract`); without one the\n  screenshot is evidence for a human, not a machine verdict.\n'
  } >"${EVIDENCE}/NOTES.md"
}

setup_evidence() {
  mkdir -p "$EVIDENCE" || { say "cannot create evidence dir $EVIDENCE"; exit 2; }
  printf 'step\tassertion\tstatus\tevidence\towner\n' >"${EVIDENCE}/results.tsv"
  printf 'id\towning-task\tevidence\tsummary\n' >"${EVIDENCE}/defects.tsv"
  : >"${EVIDENCE}/step-0.log"
}

print_plan() {
  say "PLAN (nothing executed)"
  say "  evidence dir: ${EVIDENCE}"
  say "  platform:     ${PLATFORM}   instance: ${INSTANCE}   control port: $(ctrl_port)"
  say "  artifact:     ${ARTIFACT:-<none — will look for an installed package>} (${ARTIFACT_KIND})"
  say ""
  say "  step 0  validity precondition: the offline honesty gate, NO plane answering,"
  say "          a packaged (not dev) binary, the GUI prerequisites, the sources"
  say "  step 1  install the .deb; dpkg -L both packaged paths; sha256sum the binary"
  say "  step 2  launch with no plane; proxy /healthz 502 names the URL; shell log line; screenshot"
  say "  step 3  click Start (--click-map start=X,Y); /healthz 200, /versionz, listening PID"
  say "  step 4  __ORCHICON_DESKTOP__ in the shell's index and absent from the plane's; forced mismatch"
  say "  step 5  WriteTimeout 0 + FlushInterval -1; Go live test; plane feed and window frames grow"
  say "  step 6  ListProjects / ListWorkItems counts vs the rendered views (empty is SKIP)"
  say "  step 7  click Stop: /healthz fails and the PID is gone; click Start: /healthz answers"
  say "  step 8  Linux launcher names WebKitGTK when absent; Windows preflight names the remedy"
  say ""
  say "  exit 0 all passed · 1 a step failed (defect filed against its owner) · 2 not a valid proof"
}

usage() { sed -n '1,120p' "${BASH_SOURCE[0]}" | sed -n '/^# USAGE/,/^# ===/p' | sed 's/^# \{0,1\}//'; }

need_arg() { [ -n "${2:-}" ] || { say "$1 needs a value"; exit 2; }; }

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --instance) need_arg "$1" "${2:-}"; INSTANCE="$2"; shift 2 ;;
      --evidence-dir) need_arg "$1" "${2:-}"; EVIDENCE="$2"; shift 2 ;;
      --deb) need_arg "$1" "${2:-}"; ARTIFACT="$2"; ARTIFACT_KIND="deb"; shift 2 ;;
      --appimage) need_arg "$1" "${2:-}"; ARTIFACT="$2"; ARTIFACT_KIND="appimage"; shift 2 ;;
      --portable-zip) need_arg "$1" "${2:-}"; ARTIFACT="$2"; ARTIFACT_KIND="zip"; shift 2 ;;
      --platform) need_arg "$1" "${2:-}"; PLATFORM="$2"; shift 2 ;;
      --workflow-id) need_arg "$1" "${2:-}"; WORKFLOW_ID="$2"; shift 2 ;;
      --execution-id) need_arg "$1" "${2:-}"; EXECUTION_ID="$2"; shift 2 ;;
      --control-port) need_arg "$1" "${2:-}"; CTRL_OVERRIDE="$2"; shift 2 ;;
      --click-map) need_arg "$1" "${2:-}"; CLICK_MAP="$2"; shift 2 ;;
      --keep) KEEP=1; shift ;;
      --preflight-only) PREFLIGHT_ONLY=1; shift ;;
      --dry-run) DRY_RUN=1; shift ;;
      -h|--help) usage; exit 0 ;;
      *) say "unknown option: $1 (see --help)"; exit 2 ;;
    esac
  done
  if [ -z "$PLATFORM" ]; then
    case "$(uname -s 2>/dev/null)" in
      Linux) PLATFORM=linux ;; Darwin) PLATFORM=macos ;; MINGW*|MSYS*|CYGWIN*) PLATFORM=windows ;; *) PLATFORM=linux ;;
    esac
  fi
  if [ -z "$EVIDENCE" ]; then EVIDENCE="${REPO_ROOT}/qa-evidence/desktop-e2e/${UTC_STAMP}"; fi
  case "$EVIDENCE" in /*) ;; *) EVIDENCE="${REPO_ROOT}/${EVIDENCE}" ;; esac
  [ -n "$ARTIFACT" ] && case "$ARTIFACT" in /*) ;; *) ARTIFACT="${REPO_ROOT}/${ARTIFACT}" ;; esac
}

finish() { # finish <want-exit>
  write_notes_template
  write_summary
  if [ "$KEEP" != 1 ]; then gui_kill; fi
  say ""
  say "evidence: ${EVIDENCE}"
  say "defects:  $([ -s "${EVIDENCE}/defects.tsv" ] && tail -n +2 "${EVIDENCE}/defects.tsv" | wc -l || echo 0)"
  exit "$1"
}

main() {
  parse_args "$@"
  setup_evidence
  if [ "$DRY_RUN" = 1 ]; then print_plan; write_notes_template; exit 0; fi

  step0_preflight
  if [ -n "$ABORT_REASON" ]; then write_notes_template; write_summary; say ""; say "RUN INVALID: ${ABORT_REASON}"; finish 2; fi
  if [ "$PREFLIGHT_ONLY" = 1 ]; then
    say ""; say "preflight only: ${PASSES} passed, ${FAILS} failed, ${SKIPS} skipped"; finish 0
  fi

  step1_install
  step2_offline_state
  step3_start_from_app
  step4_embedded_ui
  step5_streaming
  step6_unary_views
  step7_control_both_ways
  step8_platform_variants

  if [ "$FAILS" -gt 0 ]; then finish 1; fi
  finish 0
}

# Sourced (by the offline honesty gate) this file is a library; executed, it drives.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then main "$@"; fi
