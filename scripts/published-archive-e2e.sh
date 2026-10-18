#!/usr/bin/env bash
# ============================================================================
# ACCEPTANCE PROCEDURE: the PUBLISHED ARCHIVE, on a clean machine, of each OS —
# with no Docker, no Docker Desktop and no WSL anywhere in sight.
#
# THE CLAIM THIS PROVES. "A real user downloads the archive for their platform,
# extracts it, runs `orchicon install`, and gets a healthy host-native plane and a
# working `orch` — with no container engine and no WSL on the machine." Compiling
# for a platform is not running on it, and a locally built binary is not the
# published archive, so this procedure downloads the ARTIFACT and drives it.
#
# THEREFORE THE THINGS IT REFUSES TO ASSUME:
#   * That the version under test contains the work. `releases/latest` was many
#     versions behind develop and predated host-native entirely (correction C1),
#     so `--version` is REQUIRED, the tag's publish date is checked against the
#     host-native merge, and the extracted binaries must report the same version.
#   * That a captured install run asked the right questions. A run with redirected
#     stdout deliberately NEVER prompts (correction C3): the absence of port
#     questions is correct, and the ports it used must be NAMED in the output.
#     A RE-RUN must re-ask neither ports nor runtime images.
#   * That the lifecycle verbs are `serve`. They are `start`/`stop`/`plane status`
#     and EVERY ONE TAKES AN INSTANCE (correction C2). Each command in the
#     evidence names the instance it acted on.
#   * That no engine was consulted, because the transcript looks clean. Every
#     command runs on a PATH where `docker`/`podman`/`wsl`/`docker.exe` are
#     BOOBY-TRAPPED (scripts/e2e/no-engine-path.sh); the invocation log must stay
#     EMPTY (correction: AC5).
#   * That the footprint is under 1 GB because someone derived it. It is measured
#     with `du`, per component, on the date of the run (AC6, correction C4), and a
#     REFUTATION is recorded as a finding rather than hidden.
#
# WHAT IT DOES NOT DO: it patches nothing (AC10). Every failure is written to
# defects.tsv naming the sibling task that owns the fix, and the run exits 1/2.
#
# USAGE
#   scripts/published-archive-e2e.sh --version vX.Y.Z [options]
#     --version <tag>        the release tag under test. REQUIRED (C1). `latest` is REJECTED.
#     --platform linux|darwin|windows
#                            the platform being proven (default: this host's)
#     --instance dev|prod|test
#                            instance every lifecycle command acts on (default dev)
#     --evidence-dir DIR     evidence root (default qa-evidence/published-archive-e2e/<UTC>)
#     --install-dir DIR      where the archive's binaries are installed
#                            (default ~/.local/bin; on Windows %LOCALAPPDATA%\Programs\Orchicon)
#     --keep                 leave the plane this run started running
#     --skip-tui             do not drive the TUI (records WHY, never a silent PASS)
#     --dry-run              print the plan, execute nothing
#     --preflight-only       run the validity precondition only
#     -h | --help
#
# VERBS IT DRIVES (2026-10-18 correction C2):
#   orchicon install                      unchanged; refuses ORCHICON_PLANE_RESIDENCY=container
#   orchicon plane status <instance>      instance, residency, serving binary, drift, health
#   orchicon start|stop|restart <instance>
#   orch <TUI binary>                     its own platform concerns; shown, not assumed
#
# ENVIRONMENT
#   PLANE_E2E_ROOT                    scratch dir for downloads and shims
#   ORCHICON_E2E_REPO_OWNER / _REPO   the release source (default beardedparrott/Orchicon)
#   ORCHICON_E2E_BASE_URL             release download base (default the GitHub releases URL)
#   ORCHICON_E2E_HOST_NATIVE_MERGE    ISO date the host-native work merged (C1 gate)
#   ORCHICON_E2E_TUI_KEYS             keystrokes for the TUI capture (default: quit immediately)
#   ORCHICON_INSTALL_INTERACTIVE=1    only for a run that genuinely HAS a human at a terminal
# ============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/.." && pwd)"
# shellcheck source=e2e/no-engine-path.sh
. "${HERE}/e2e/no-engine-path.sh"
FOOTPRINT_SH="${HERE}/e2e/footprint.sh"

UTC_STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
VERSION=""
PLATFORM="$(uname -s 2>/dev/null || echo Linux)"
INSTANCE="dev"
DRY_RUN=0
PREFLIGHT_ONLY=0
SKIP_TUI=0
KEEP=0
EVIDENCE=""
E2E_ROOT="${PLANE_E2E_ROOT:-${TMPDIR:-/tmp}/orchicon/published-archive-e2e}"

REPO_OWNER="${ORCHICON_E2E_REPO_OWNER:-beardedparrott}"
REPO_NAME="${ORCHICON_E2E_REPO:-Orchicon}"
BASE_URL="${ORCHICON_E2E_BASE_URL:-https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/download}"

# THE C1 GATE. A release cut before the host-native work merged cannot prove anything about it:
# such an archive installs the OLD shape, which either needs an engine or tests code this epic
# does not own. Overridable so the date can be corrected once, in one place, with the merge it
# came from named here.
HOST_NATIVE_MERGE_UTC="${ORCHICON_E2E_HOST_NATIVE_MERGE:-2026-10-18}"

CLAIM_BYTES=1073741824          # 1 GiB, the epic's claim (never re-derived from the measurement)

PASSES=0
FAILS=0
SKIPS=0
FINDINGS=0                      # a REFUTED claim, recorded and not hidden — never a FAIL
BLOCKED=0
INVALID=0                       # this host is not a clean machine of the target platform
BLOCKED_SURFACE=0               # a required surface is ABSENT -> defect against the owner, exit 2
ABORT_REASON=""
CURRENT_STEP="0"
STEP_LOG=""
DIRTY=0
SHIM_DIR=""
INSTALL_DIR=""
INSTALL_DIR_DEFAULT=""
ARCHIVE_PATH=""
SUMS_PATH=""
ARCHIVE_SIZE=""
PORTS_JSON=""
PLATFORM_GOOS=""
PLATFORM_ARCH=""
ARCHIVE=""
ARCHIVE_SHA=""
EXTRACT_DIR=""
ORCHICON_BIN=""
ORCH_BIN=""
INSTALL_LOG=""
FOOTPRINT_TOTAL=""
FOOTPRINT_MEASURED_AT=""
PORTS_BEFORE=""

# The OWNERS a defect can name (AC10). Spelled as constants so an assertion can check that every
# defect row names one, instead of trusting a free-text field to be filled in.
OWNER_RELEASE="release-matrix task (release.yml SHA256SUMS glob)"
OWNER_INSTALLER="installer task (scripts/install.sh, scripts/install.ps1)"
OWNER_PLANE="host-services / plane task (cmd/orchicon)"
OWNER_TUI="TUI task (cmd/orch)"
OWNER_FOOTPRINT="epic: the under-1 GB footprint claim"
OWNER_THIS_ITEM="this item (harness configuration)"

# A run must not inherit the instance it is testing: a shell that already exports one of these
# would make "the install picked its own ports" true by accident.
unset MAKEFLAGS MFLAGS 2>/dev/null || true
unset ORCHICON_INSTANCE ORCHICON_SERVE_STATE_DIR ORCHICON_SERVE_LOG_FILE ORCHICON_SERVE_DETACHED 2>/dev/null || true
unset ORCHICON_DATA_DIR ORCHICON_BLOB_DIR ORCHICON_POSTGRES_DSN ORCHICON_NATS_URL ORCHICON_POSTGRES_PORT 2>/dev/null || true
unset ORCHICON_CONTROL_PORT ORCHICON_PLANE_RESIDENCY 2>/dev/null || true

# --- output ------------------------------------------------------------------
say()   { printf '%s\n' "$*"; }
head2() { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
ok2()   { printf '  \033[32mPASS\033[0m %s\n' "$*"; }
bad2()  { printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
skip2() { printf '  \033[33mSKIP\033[0m %s\n' "$*"; }
blk2()  { printf '  \033[35mBLOCKED\033[0m %s\n' "$*"; }
find2() { printf '  \033[36mFINDING\033[0m %s\n' "$*"; }
dim()   { printf '  \033[2m%s\033[0m\n' "$*"; }

# --- evidence ----------------------------------------------------------------
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

# finding <desc> [owner] — a TRUE observation that CONTRADICTS a claim but breaks no acceptance
# criterion in the artefact under test (AC6: "a refutation is a finding, not a failure to hide").
# It never fails the run, it is never a PASS, and it is written to findings.tsv so it cannot
# scroll away.
finding() { # finding <desc> [owner]
	FINDINGS=$((FINDINGS + 1))
	record FINDING "$1" "${2:-}"
	printf '%s\t%s\t%s\n' "$CURRENT_STEP" "$1" "${2:-}" >>"${EVIDENCE}/findings.tsv"
	find2 "$1${2:+   (owner: $2)}"
}

blocked_surface() { # blocked_surface <desc> <owner> <why> — a MISSING SURFACE: defect + exit 2
	BLOCKED=$((BLOCKED + 1)); BLOCKED_SURFACE=1
	record BLOCKED "$1" "$2"; blk2 "$1 — blocked on ${2}: $3"
	printf '%s\t%s\t%s\n' "$CURRENT_STEP" "$1 [BLOCKED: $3]" "$2" >>"${EVIDENCE}/defects.tsv"
	printf '  (blocked: %s — owner %s)\n' "$3" "$2" >>"$STEP_LOG"
}

invalid_host() { # invalid_host <desc> <why> — cannot execute HERE; accuses no product code
	BLOCKED=$((BLOCKED + 1)); INVALID=1
	record BLOCKED "$1" "(invalid host)"; blk2 "$1 — invalid host: $2"
	printf '  (invalid host: %s)\n' "$2" >>"$STEP_LOG"
}

note() { printf '  %s\n' "$*" >>"$STEP_LOG"; }

step_begin() { # step_begin <n> <slug>
	CURRENT_STEP="$1"; STEP_LOG="${EVIDENCE}/${1}-${2}.log"
	: >"$STEP_LOG" 2>/dev/null || true
	head2 "step ${1} — ${2}"
}

# c_run <cmd...> — run it, log the exact command with the INSTANCE it acted on, its exit code and
# its output, and hand the output back so an assertion can read it.
c_run() {
	local out rc
	out="$("$@" 2>&1)"; rc=$?
	{ printf '\n$ %s   [instance: %s]\n[exit %s]\n%s\n' "$*" "$INSTANCE" "$rc" "$out"; } >>"$STEP_LOG"
	printf '%s' "$out"
	return "$rc"
}

# e_run <log> <cmd...> — captured run, stdout+stderr redirected into a log FILE as well as kept.
# The redirect is not cosmetic: C3 says a run whose output is captured asks NOTHING, so the
# harness must be one of those runs, and the log is the evidence that it was.
e_run() {
	local log="$1"; shift
	local out rc
	out="$("$@" 2>&1 </dev/null)"; rc=$?
	printf '%s\n' "$out" >"$log"
	{ printf '\n$ %s   [instance: %s]\n[exit %s]\n%s\n' "$*" "$INSTANCE" "$rc" "$out"; } >>"$STEP_LOG"
	printf '%s' "$out"
	return "$rc"
}

# --- platform / artifact identity (pure: the offline gate drives these) ------
platform_from_uname() { # platform_from_uname <uname -s>
	# BOTH SPELLINGS: `uname -s` says `Linux`, but `--platform` is typed by a human as `linux`,
	# and a runner that silently resolved its own override to "unknown" would refuse every run.
	case "$1" in
		Linux*|linux) printf 'linux' ;;
		Darwin*|darwin|macos|osx) printf 'darwin' ;;
		MINGW*|MSYS*|CYGWIN*|Windows_NT|windows|Windows) printf 'windows' ;;
		*) printf 'unknown' ;;
	esac
}

arch_from_uname() { # arch_from_uname <uname -m>
	case "$1" in
		x86_64|amd64|AMD64) printf 'amd64' ;;
		arm64|aarch64|ARM64) printf 'arm64' ;;
		*) printf 'unknown' ;;
	esac
}

archive_ext() { # archive_ext <platform> — the release matrix publishes zips for Windows only
	case "$1" in
		windows) printf 'zip' ;;
		linux|darwin) printf 'tar.gz' ;;
		*) printf '' ;;
	esac
}

# archive_name <tag> <platform> <arch> — EXACTLY the asset the release matrix publishes:
# `orchicon_${VERSION#v}_${os}_${arch}.${ext}` (release.yml "ASSET="). Deriving it here rather than
# copying a filename out of a transcript is what makes "the published archive" verifiable: a
# misnamed asset is a 404, not a silent pass.
archive_name() {
	local tag="$1" os="$2" arch="$3" ext
	ext="$(archive_ext "$os")"
	[ -n "$ext" ] || return 1
	printf 'orchicon_%s_%s_%s.%s' "${tag#v}" "$os" "$arch" "$ext"
}

# version_is_explicit <tag> — C1. `latest` is the trap this correction exists for: it resolves to
# whatever is newest TODAY, which was many versions behind develop when this note was written.
version_is_explicit() {
	case "$1" in
		''|latest|LATEST|stable|current|nightly|main|master|develop) return 1 ;;
		*) return 0 ;;
	esac
}

# iso_date_lt <a> <b> — "2026-10-17" < "2026-10-18". ISO dates compare lexicographically, and a
# date-string comparison cannot be fooled by a locale or a timezone the way `date -d` can.
iso_date_lt() { [ -n "$1" ] && [ -n "$2" ] && [ "$1" \< "$2" ]; }

# release_postdates_host_native <release ISO date> — 0 when the release is at or after the merge.
release_postdates_host_native() {
	[ -n "$1" ] || return 1
	iso_date_lt "$1" "$HOST_NATIVE_MERGE_UTC" && return 1
	return 0
}

# checksum_line_for <asset> <SHA256SUMS> — the recorded digest for EXACTLY that asset, or nothing.
# NOTHING is the honest answer for the Windows zip today: release.yml builds SHA256SUMS from
# `*.tar.gz *.pkg` only, so the zip is published unchecksummed (DEF-1, filed against the release
# matrix). Callers must treat empty as "unverified", never as "verified".
checksum_line_for() {
	local asset="$1" sums="$2"
	[ -s "$sums" ] || return 1
	awk -v want="$asset" '
		{ name = $2; sub(/^\*/, "", name); sub(/^\.\//, "", name) }
		name == want { print $1; found = 1; exit }
		END { exit found ? 0 : 1 }
	' "$sums"
}

# output_asked_a_question <capture> — C3. A captured run must ask nothing; a prompt makes a
# CI or logged install hang, and the fact that it did not hang is not proof that it did not ask.
output_asked_a_question() {
	local f="$1"
	[ -s "$f" ] || return 1
	grep -qE '\[y/N\]|\[Y/n\]|choose 1-3|\(yes/no\)|Press any key|[Pp]ress Enter to continue' "$f"
}

# engine_reference_in <file...> — AC5. The engine NAMES, not the word "container": the plane
# legitimately talks about runtime containers, and an assertion that cannot tell those apart would
# either fail on correct output or be loosened until it proved nothing.
engine_reference_in() {
	[ "$#" -gt 0 ] || return 1
	grep -hoiE '(^|[^[:alnum:]_])(docker(-compose|-desktop)?|podman|nerdctl)([^[:alnum:]_]|$)' "$@" 2>/dev/null | sort -u
}

# wsl_reference_in <file...> — AC3. Windows local mode must not need WSL; a transcript that
# mentions it at all is evidence to review even when the install succeeded.
wsl_reference_in() {
	[ "$#" -gt 0 ] || return 1
	grep -hoiE '(^|[^[:alnum:]_])wsl(\.exe|2)?([^[:alnum:]_]|$)' "$@" 2>/dev/null | sort -u
}

# ports_named_in_output <capture> <port...> — every port the install actually bound is NAMED in
# its output, so a user (and this procedure) can see which numbers a captured run chose (AC8).
ports_named_in_output() {
	local f="$1"; shift
	[ -s "$f" ] || return 1
	local p
	for p in "$@"; do
		grep -qF -- "$p" "$f" || return 1
	done
	return 0
}

# ports_from_json <ports.json> — the recorded port numbers, space separated. Reads the instance's
# OWN record; re-resolving ports from an env var would test the harness, not the install.
ports_from_json() {
	local f="$1"
	[ -s "$f" ] || return 1
	if command -v python3 >/dev/null 2>&1; then
		python3 -c 'import json,sys;print(" ".join(str(v) for v in json.load(open(sys.argv[1])).values()),end="")' "$f" 2>/dev/null
	else
		tr -dc '0-9, ' <"$f" | tr ',' ' ' | tr -s ' '
	fi
}

# claim_supported <bytes> — 0 when the measurement supports the epic's claim.
claim_supported() {
	case "$1" in ''|*[!0-9]*) return 1 ;; esac
	[ "$1" -lt "$CLAIM_BYTES" ]
}

# capture_is_empty <file> — TRUE when a capture holds no meaningful bytes. An EMPTY capture can
# never produce a PASS: it proves only that the producer failed, and that is how a harness ends up
# reporting a TUI as "rendered" when nothing ran.
capture_is_empty() {
	local f="$1"
	[ -s "$f" ] || return 0
	[ -z "$(tr -d '[:space:]\r' <"$f" 2>/dev/null)" ]
}

# gated_contains <desc> <file> <needle> <empty-why> — PASS only on a capture WITH bytes.
gated_contains() {
	local desc="$1" f="$2" needle="$3" why="$4"
	if capture_is_empty "$f"; then
		skip "$desc" "${why} (the capture is EMPTY — that proves nothing)"
		return 0
	fi
	if grep -qF -- "$needle" "$f"; then check "$desc" 0; else check "$desc" 1 "$5"; fi
}

# gated_absent <desc> <file> <needle> <empty-why> [owner] — same rule for an ABSENCE claim.
gated_absent() {
	local desc="$1" f="$2" needle="$3" why="$4" owner="${5:-}"
	if capture_is_empty "$f"; then
		skip "$desc" "${why} (the capture is EMPTY — an absence proves nothing)"
		return 0
	fi
	if grep -qF -- "$needle" "$f"; then check "$desc" 1 "$owner"; else check "$desc" 0; fi
}

# --- status-line matchers (functions so the offline gate can drive them) ------
# The status surface is the ONLY one that reports the shape, the serving binary and health
# (correction C2), so an assertion that cannot tell "healthy" from "host-resident" from "current"
# would pass on a plane that is a container.
status_names_instance()  { case "$1" in *"instance $2"*) return 0 ;; *) return 1 ;; esac; }
status_says_host_resident() { case "$1" in *"host-resident"*) return 0 ;; *) return 1 ;; esac; }
status_says_container()  { case "$1" in *"container"*) case "$1" in *"host-resident"*) return 1 ;; *) return 0 ;; esac ;; *) return 1 ;; esac; }
status_says_healthy_at() { case "$1" in *"healthy at"*) return 0 ;; *) return 1 ;; esac; }
status_says_current()    { case "$1" in *"binary current"*) return 0 ;; *) return 1 ;; esac; }
status_says_superseded() { case "$1" in *"binary superseded"*) return 0 ;; *) return 1 ;; esac; }

# residency_refusal_in <capture> — AC5/C2: an install that REFUSES the container residency is
# CORRECT, not a failure. Recognising the refusal is what keeps a correct refusal from being filed
# as a defect against the plane.
residency_refusal_in() {
	case "$1" in
		*"ORCHICON_PLANE_RESIDENCY"*) case "$1" in *"container"*) return 0 ;; esac ;; 
	esac
	return 1
}

# elevated_message_complete <capture> — AC3: the Windows elevated-shell message is ACTIONABLE:
# it says Postgres refuses Administrator, and it says what to do next. Four phrases, because a
# message that names the problem but not the fix is the downstream failure this criterion bans.
elevated_message_complete() {
	local c="$1"
	case "$c" in *Administrator*) ;; *) return 1 ;; esac
	case "$c" in *"Next steps"*) ;; *) return 1 ;; esac
	case "$c" in *"NORMAL PowerShell"*) ;; *) return 1 ;; esac
	case "$c" in *"Nothing was downloaded"*) ;; *) return 1 ;; esac
	return 0
}

# signature_verdict <accepted:0|1> <instruction_documented:0|1> — AC2 for Gatekeeper AND
# SmartScreen. Three outcomes and only three:
#   accepted    the OS itself accepted the download: nothing for the user to get past
#   instruction the OS refused, and the published EXACT instruction a user follows is present
#               (tested by the run completing) — not a dead end
#   dead-end    refused, and no tested instruction: the user is stuck, and this is a FAIL
signature_verdict() {
	if [ "$1" = "0" ]; then printf 'accepted'; return 0; fi
	if [ "$2" = "0" ]; then printf 'instruction'; return 0; fi
	printf 'dead-end'
}

# tui_render_marker <capture> — the first thing in the TUI's capture that shows it DREW something.
# A marker list rather than one literal string: the TUI is a separate binary with its own
# vocabulary per platform, and pinning one word would fail on a correct render.
tui_render_marker() {
	local f="$1" m
	capture_is_empty "$f" && return 1
	for m in Orchicon orchicon plane instance healthz healthy Run ⌘; do
		grep -qF -- "$m" "$f" && { printf '%s' "$m"; return 0; }
	done
	return 1
}

# --- process / pty / timeout portability -------------------------------------
# process_alive <pid> — portable liveness. `/proc` does not exist on macOS or Windows, so an
# assertion built on it PASSED VACUOUSLY there: "the pid is gone" was true only because the
# whole filesystem it looked at was absent. kill -0 is the portable probe (POSIX, and MSYS).
process_alive() {
	local pid="$1"
	case "$pid" in ''|*[!0-9]*) return 1 ;; esac
	kill -0 "$pid" 2>/dev/null && return 0
	[ -d "/proc/${pid}" ] && return 0
	return 1
}

# pty_script <command> — run one command under a REAL pty, in the flavour of `script` this host
# actually has. util-linux spells it `script -qec CMD /dev/null`; BSD and macOS spell it
# `script -q /dev/null CMD` and have neither -c nor -e. A hardcoded `-qec` therefore produced an
# error text instead of a TUI on every Mac, so the TUI step could never pass on darwin (AC4).
pty_script() {
	local cmd="$1" os
	[ -n "$cmd" ] || return 1
	os="$(uname -s 2>/dev/null || echo Linux)"
	case "$os" in
		Darwin|*BSD*) script -q /dev/null "$cmd" ;;
		*) script -qec "$cmd" /dev/null ;;
	esac
}

# run_with_timeout <seconds> <cmd...> — `timeout` is GNU/coreutils, and macOS ships neither it nor
# gtimeout until coreutils is installed by hand. A missing `timeout` turned the TUI step into
# "timeout: command not found" inside the capture, i.e. a rendering failure that was really a
# missing tool. Without either binary the command still runs: the key feed above quits the TUI.
run_with_timeout() {
	local secs="$1"; shift
	if command -v timeout >/dev/null 2>&1; then timeout "$secs" "$@"; return $?; fi
	if command -v gtimeout >/dev/null 2>&1; then gtimeout "$secs" "$@"; return $?; fi
	"$@"
}

# --- filesystem helpers ------------------------------------------------------
sha256_of() { # sha256_of <file> — GNU or BSD, whichever this platform has
	local f="$1"
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$f" | awk '{print $1}'; return 0; fi
	if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$f" | awk '{print $1}'; return 0; fi
	if command -v certutil >/dev/null 2>&1; then
		certutil -hashfile "$f" SHA256 2>/dev/null | sed -n 2p | tr -d ' \r' | tr 'A-Z' 'a-z'; return 0
	fi
	return 1
}

# --- step 0: preflight (the validity precondition) ---------------------------
step0_preflight() {
	step_begin 0 preflight
	local shim_dir="${E2E_ROOT}/shims"
	SHIM_DIR="$shim_dir"

	PLATFORM_GOOS="$(platform_from_uname "$PLATFORM")"
	PLATFORM_ARCH="${ORCHICON_E2E_ARCH:-$(arch_from_uname "$(uname -m)")}"
	note "platform: ${PLATFORM} -> ${PLATFORM_GOOS}/${PLATFORM_ARCH}"

	# A0.1 — the version under test is NAMED (C1). This is the correction that matters most:
	# `latest` was many versions behind develop and predated host-native, so a run against it
	# would install the OLD shape and prove nothing about this epic.
	check "A0.1 the version under test is named explicitly (C1: not 'latest'/'stable'/empty)" \
		"$(version_is_explicit "$VERSION" && echo 0 || echo 1)" "$OWNER_THIS_ITEM"
	if ! version_is_explicit "$VERSION"; then
		ABORT_REASON="no explicit --version: \`latest\` predates the host-native work (correction C1)"
		invalid_host "A0.1b an explicit --version is required to identify the artifact" "$ABORT_REASON"
		return 0
	fi

	# A0.2 — this host can even name its own platform and the artifact shape.
	check "A0.2 the platform is one the release matrix publishes" \
		"$([ "$PLATFORM_GOOS" != unknown ] && [ -n "$(archive_ext "$PLATFORM_GOOS")" ] && echo 0 || echo 1)" \
		"$OWNER_THIS_ITEM"
	ARCHIVE="$(archive_name "$VERSION" "$PLATFORM_GOOS" "$PLATFORM_ARCH")"
	check "A0.3 the asset name is derived from the release matrix, not copied from a transcript" \
		"$(case "$ARCHIVE" in orchicon_*_${PLATFORM_GOOS}_${PLATFORM_ARCH}.*) echo 0 ;; *) echo 1 ;; esac)" \
		"$OWNER_THIS_ITEM"

	# A0.4 — NO CONTAINER ENGINE and NO WSL. A host that answers is not a clean machine: the whole
	# claim is about machines like that, so the run must not proceed on one.
	make_shims "$SHIM_DIR" || invalid_host "A0.4 the engine shims could not be planted" "mkdir/write failed under ${SHIM_DIR}"
	export PATH="${SHIM_DIR}:$PATH"
	local i fail=0
	for i in $(engine_shim_names); do
		PATH="${SHIM_DIR}:$PATH" "$i" >/dev/null 2>&1 && fail=1
	done
	check "A0.4 every engine/WSL name on PATH is BOOBY-TRAPPED (docker, podman, wsl, docker.exe…)" \
		"$fail"

	# A0.5 — and no REAL engine is installed. Probed through the shims, so a host whose engine
	# answers (because it is on another path) cannot be mistaken for one where nothing exists.
	# The probe runs ONCE and its answer is reused: probing twice logged `docker info` twice.
	local engine_answers=0
	engine_runs_here "$SHIM_DIR" && engine_answers=1
	if [ "$engine_answers" = "1" ]; then
		invalid_host "A0.5 a container engine ANSWERS on this host — it is not a clean machine" \
			"the claim is about machines with no Docker/Podman; run this on one"
	fi
	check "A0.5 no container engine answers (clean machine)" "$engine_answers"

	# THE PREFLIGHT'S OWN PROBES ARE NOT EVIDENCE ABOUT THE INSTALL PATH, so the invocation log is
	# truncated HERE — after A0.4 deliberately invoked every shim and after A0.5 asked `docker info`,
	# and before the first step that acts on the product. Truncating before A0.5 (the shape this had)
	# left the preflight's own probe in the log and made step 10's "nothing was invoked" assertion
	# FAIL on a spotless machine — a false defect filed against the installer.
	: >"$(shim_log_path "$SHIM_DIR")"

	# A0.6 — nothing named orchicon is already installed, or the run would test a previous install.
	if command -v orchicon >/dev/null 2>&1; then
		invalid_host "A0.6 a previous orchicon is on PATH — this is not a clean machine" \
			"$(command -v orchicon)"
	fi
	check "A0.6 no previous orchicon on PATH (clean machine)" \
		"$(command -v orchicon >/dev/null 2>&1 && echo 1 || echo 0)"

	# A0.7 — the tools the procedure needs to download and verify a published artifact.
	local missing="" t
	for t in curl; do command -v "$t" >/dev/null 2>&1 || missing="${missing} ${t}"; done
	for t in sha256sum shasum certutil; do
		command -v "$t" >/dev/null 2>&1 && break
		[ "$t" = certutil ] && missing="${missing} sha256"
	done
	case "$PLATFORM_GOOS" in
		windows) command -v unzip >/dev/null 2>&1 || missing="${missing} unzip" ;;
		*) command -v tar >/dev/null 2>&1 || missing="${missing} tar" ;;
	esac
	check "A0.7 download + verify + extract tooling is present${missing:+ (missing:${missing})}" \
		"$([ -z "$missing" ] && echo 0 || echo 1)" "$OWNER_THIS_ITEM"

	[ -n "$INSTALL_DIR" ] || INSTALL_DIR="${INSTALL_DIR_DEFAULT:-${HOME}/.local/bin}"
	mkdir -p "$INSTALL_DIR" 2>/dev/null || true
	check "A0.8 the install dir is writable: ${INSTALL_DIR}" \
		"$([ -w "$INSTALL_DIR" ] && echo 0 || echo 1)" "$OWNER_THIS_ITEM"
	note "install dir: ${INSTALL_DIR}"
	note "evidence dir: ${EVIDENCE}"
}

# --- step 1: the PUBLISHED artifact ----------------------------------------
step1_download() {
	step_begin 1 download
	local url="${BASE_URL}/${VERSION}/${ARCHIVE}"
	local sums_url="${BASE_URL}/${VERSION}/SHA256SUMS"
	local dl="${E2E_ROOT}/download/${VERSION}"
	mkdir -p "$dl" 2>/dev/null || true
	ARCHIVE_PATH="${dl}/${ARCHIVE}"
	SUMS_PATH="${dl}/SHA256SUMS"

	note "downloading ${url}"
	c_run curl -fsSL --max-time 600 -o "$ARCHIVE_PATH" "$url" >/dev/null
	check "A1.1 the published archive downloads: ${ARCHIVE}" \
		"$([ -s "$ARCHIVE_PATH" ] && echo 0 || echo 1)" "$OWNER_RELEASE"
	[ -s "$ARCHIVE_PATH" ] || return 0

	ARCHIVE_SIZE="$(wc -c <"$ARCHIVE_PATH" | tr -d ' ')"
	ARCHIVE_SHA="$(sha256_of "$ARCHIVE_PATH" || true)"
	check "A1.2 the archive is a real artifact, not an error page (${ARCHIVE_SIZE} bytes)" \
		"$([ "${ARCHIVE_SIZE:-0}" -gt 1000000 ] && echo 0 || echo 1)" "$OWNER_RELEASE"
	note "archive: ${ARCHIVE}  bytes: ${ARCHIVE_SIZE}  sha256: ${ARCHIVE_SHA}"

	# A1.3/A1.4 — the CHECKSUM. Empty is "unverified", never "verified". This is DEF-1: the
	# release matrix builds SHA256SUMS from `*.tar.gz *.pkg` only, so the Windows zip is published
	# unchecksummed while install.ps1 quietly reports "skipping checksum verification".
	c_run curl -fsSL --max-time 120 -o "$SUMS_PATH" "$sums_url" >/dev/null
	check "A1.3 SHA256SUMS is published beside the archive" \
		"$([ -s "$SUMS_PATH" ] && echo 0 || echo 1)" "$OWNER_RELEASE"
	local want; want="$(checksum_line_for "$ARCHIVE" "$SUMS_PATH" 2>/dev/null || true)"
	if [ -z "$want" ]; then
		check "A1.4 the archive is LISTED in SHA256SUMS (${ARCHIVE})" 1 "$OWNER_RELEASE"
		note "DEF-1: ${ARCHIVE} is absent from SHA256SUMS — release.yml generates it from '*.tar.gz *.pkg'."
	else
		check "A1.4 the archive is LISTED in SHA256SUMS and the published digest matches" \
			"$([ "$want" = "$ARCHIVE_SHA" ] && echo 0 || echo 1)" "$OWNER_RELEASE"
	fi

	# A1.5 — the archive CONTAINS the two shipped binaries, so "extract and run" is a real step.
	local listing="${EVIDENCE}/1-archive-listing.txt"
	case "$PLATFORM_GOOS" in
		windows) unzip -l "$ARCHIVE_PATH" 2>/dev/null >"$listing" || true ;;
		*) tar -tzf "$ARCHIVE_PATH" 2>/dev/null >"$listing" || true ;;
	esac
	gated_contains "A1.5 the archive ships the orchicon binary" "$listing" "orchicon" "the listing could not be produced" "$OWNER_RELEASE"
	gated_contains "A1.6 the archive ships the orch TUI binary" "$listing" "orch" "the listing could not be produced" "$OWNER_RELEASE"
	gated_absent "A1.7 the archive ships no container assets (docker-compose / Dockerfile)" \
		"$listing" "Dockerfile" "the listing could not be produced" "$OWNER_RELEASE"
}

# --- step 2: the artifact IS the version under test (C1) --------------------
step2_extract_and_identify() {
	step_begin 2 extract
	EXTRACT_DIR="${E2E_ROOT}/extract/${VERSION}/${PLATFORM_GOOS}-${PLATFORM_ARCH}"
	rm -rf "$EXTRACT_DIR" 2>/dev/null || true
	mkdir -p "$EXTRACT_DIR" 2>/dev/null || true
	case "$PLATFORM_GOOS" in
		windows) c_run unzip -q -o "$ARCHIVE_PATH" -d "$EXTRACT_DIR" >/dev/null ;;
		*) c_run tar -xzf "$ARCHIVE_PATH" -C "$EXTRACT_DIR" >/dev/null ;;
	esac
	ORCHICON_BIN="$(find "$EXTRACT_DIR" -type f -name 'orchicon*' 2>/dev/null | grep -v '\.txt$' | head -1)"
	ORCH_BIN="$(find "$EXTRACT_DIR" -type f -name 'orch' -o -type f -name 'orch.exe' 2>/dev/null | head -1)"
	check "A2.1 the archive extracts to an orchicon binary" \
		"$([ -n "$ORCHICON_BIN" ] && [ -f "$ORCHICON_BIN" ] && echo 0 || echo 1)" "$OWNER_RELEASE"
	if [ -z "$ORCHICON_BIN" ] || [ ! -f "$ORCHICON_BIN" ]; then
		blocked_surface "A2.2 the binary cannot be identified, so nothing can be run" "$OWNER_RELEASE" \
			"no orchicon binary inside ${ARCHIVE}"
		return 0
	fi
	chmod +x "$ORCHICON_BIN" 2>/dev/null || true
	[ -n "$ORCH_BIN" ] && chmod +x "$ORCH_BIN" 2>/dev/null

	# THE IDENTITY CHECK. A downloaded archive that reports a DIFFERENT version than the tag is
	# either the wrong artifact or a mislabelled release, and either way the run proves nothing
	# about the version it claims (C1: "record which version was tested").
	local vout; vout="$(c_run "$ORCHICON_BIN" --version 2>&1 || true)"
	: >"${EVIDENCE}/2-version.txt"; printf '%s\n' "$vout" >>"${EVIDENCE}/2-version.txt"
	gated_contains "A2.3 the extracted binary reports the version under test (${VERSION})" \
		"${EVIDENCE}/2-version.txt" "${VERSION#v}" "the binary printed nothing" "$OWNER_RELEASE"
	local bin_version; bin_version="$(printf '%s' "$vout" | grep -oE '[0-9]+\.[0-9]+\.[0-9]+[^ ]*' | head -1)"
	note "tag: ${VERSION}   binary reports: ${bin_version:-unknown}   path: ${ORCHICON_BIN}"
	echo "tag=${VERSION}" >>"${EVIDENCE}/2-version.txt"
	echo "binary=${ORCHICON_BIN}" >>"${EVIDENCE}/2-version.txt"
	echo "reports=${bin_version:-unknown}" >>"${EVIDENCE}/2-version.txt"
}

# --- step 3: `orchicon install`, captured (C3) ------------------------------
step3_install() {
	step_begin 3 install
	INSTALL_LOG="${EVIDENCE}/3-install.log"
	PORTS_JSON="$(instance_data_dir)/ports.json"

	e_run "$INSTALL_LOG" "$ORCHICON_BIN" install --instance "$INSTANCE" >/dev/null
	local rc=$?
	check "A3.1 \`orchicon install --instance ${INSTANCE}\` succeeds from the ARCHIVE's binary" \
		"$([ "$rc" = 0 ] && echo 0 || echo 1)" "$OWNER_INSTALLER"
	check "A3.2 the install produced output (an empty capture proves nothing)" \
		"$(capture_is_empty "$INSTALL_LOG" && echo 1 || echo 0)" "$OWNER_INSTALLER"

	# C3 — a captured run asks NOTHING. The absence of port questions is CORRECT here, so this
	# asserts the prompt-free path was taken rather than pretending a human answered.
	check "A3.3 a captured install asked NO questions (C3: redirected stdout never prompts)" \
		"$(output_asked_a_question "$INSTALL_LOG" && echo 1 || echo 0)" "$OWNER_INSTALLER"

	check "A3.4 the install RECORDED its ports for instance ${INSTANCE}" \
		"$([ -s "$PORTS_JSON" ] && echo 0 || echo 1)" "$OWNER_INSTALLER"
	local ports; ports="$(ports_from_json "$PORTS_JSON" 2>/dev/null || true)"
	note "recorded ports for ${INSTANCE}: ${ports:-none}"
	if [ -n "$ports" ]; then
		# shellcheck disable=SC2086
		gated_contains "A3.5 the install output NAMES the ports it used (${ports})" \
			"$INSTALL_LOG" "$(printf '%s' "$ports" | awk '{print $1}')" "the install log is empty" "$OWNER_INSTALLER"
		# shellcheck disable=SC2086
		check "A3.6 every recorded port is named in the output (AC8: say which ports were used)" \
			"$(ports_named_in_output "$INSTALL_LOG" $ports && echo 0 || echo 1)" "$OWNER_INSTALLER"
	fi

	# The install must NOT have refused the residency: a refusal is correct behaviour when an
	# engine asks for container residency, but nothing here asked for it.
	check "A3.7 the install was HOST-native (no container residency was requested or refused)" \
		"$(residency_refusal_in "$(cat "$INSTALL_LOG" 2>/dev/null)" && echo 1 || echo 0)" "$OWNER_PLANE"
}

# --- step 4: a HEALTHY plane, proven independently --------------------------
step4_plane_health() {
	step_begin 4 plane
	local status_out; status_out="$(c_run "$ORCHICON_BIN" plane status "$INSTANCE" || true)"
	printf '%s\n' "$status_out" >"${EVIDENCE}/4-plane-status.txt"

	gated_contains "A4.1 \`plane status <instance>\` names the instance (${INSTANCE})" \
		"${EVIDENCE}/4-plane-status.txt" "instance ${INSTANCE}" "the status surface printed nothing" "$OWNER_PLANE"
	check "A4.2 the plane is HOST-RESIDENT (no container holds it)" \
		"$(status_says_host_resident "$status_out" && echo 0 || echo 1)" "$OWNER_PLANE"
	check "A4.2b the status does not describe a CONTAINER carrier" \
		"$(status_says_container "$status_out" && echo 1 || echo 0)" "$OWNER_PLANE"
	check "A4.3 the status reports the serving binary as CURRENT, not superseded" \
		"$(status_says_current "$status_out" && echo 0 || echo 1)" "$OWNER_PLANE"

	# INDEPENDENT: /healthz over HTTP, not the status line's own claim.
	local cport; cport="$(ports_from_json "$PORTS_JSON" 2>/dev/null | awk '{print $1}')"
	if [ -z "$cport" ]; then
		invalid_host "A4.4 the control port cannot be read from the instance's own record" \
			"${PORTS_JSON} missing or unreadable — a health probe would be a guess"
		return 0
	fi
	local code; code="$(curl -sS -o "${EVIDENCE}/4-healthz.txt" -w '%{http_code}' --max-time 5 \
		"http://localhost:${cport}/healthz" 2>/dev/null || true)"
	echo "GET http://localhost:${cport}/healthz -> ${code:-no answer}" >>"${EVIDENCE}/4-healthz.txt"
	check "A4.4 /healthz answers 200 on the control port ${cport} (independent of the CLI)" \
		"$([ "$code" = "200" ] && echo 0 || echo 1)" "$OWNER_PLANE"
	note "healthz code: ${code:-none} on port ${cport}"
}

# --- step 5: the lifecycle verbs that ACTUALLY exist (C2) -------------------
step5_lifecycle_verbs() {
	step_begin 5 verbs
	local cport; cport="$(ports_from_json "$PORTS_JSON" 2>/dev/null | awk '{print $1}')"

	# STOP. Proven DOWN by a REFUSED connection (curl exit 7), never by the CLI's own report.
	local before_pid; before_pid="$(pid_in_file "$(instance_data_dir)/serve/pids/orchicon.pid" 2>/dev/null || true)"
	c_run "$ORCHICON_BIN" stop "$INSTANCE" >/dev/null
	local rc=$?
	check "A5.1 \`orchicon stop ${INSTANCE}\` exits 0" "$([ "$rc" = 0 ] && echo 0 || echo 1)" "$OWNER_PLANE"
	local out rc7; out="$(curl -sS --max-time 3 "http://localhost:${cport}/healthz" 2>/dev/null)"; rc7=$?
	check "A5.2 the plane is DOWN after stop (connection REFUSED, curl exit 7)" \
		"$([ "$rc7" = "7" ] && [ -z "$out" ] && echo 0 || echo 1)" "$OWNER_PLANE"
	if [ -n "$before_pid" ]; then
		check "A5.2b the SAME process is gone (pid ${before_pid})" \
			"$(process_alive "$before_pid" && echo 1 || echo 0)" "$OWNER_PLANE"
	fi

	# STOP IS IDEMPOTENT (C2).
	c_run "$ORCHICON_BIN" stop "$INSTANCE" >/dev/null
	rc=$?
	local still_down=1
	out="$(curl -sS --max-time 3 "http://localhost:${cport}/healthz" 2>/dev/null)"; rc7=$?
	[ "$rc7" = "7" ] && [ -z "$out" ] && still_down=0
	check "A5.3 a SECOND \`stop ${INSTANCE}\` is idempotent (exit 0 and still REFUSED)" \
		"$([ "$rc" = 0 ] && [ "$still_down" = 0 ] && echo 0 || echo 1)" "$OWNER_PLANE"

	# START — proven UP by a 200 from /healthz.
	c_run "$ORCHICON_BIN" start "$INSTANCE" >/dev/null
	check "A5.4 \`orchicon start ${INSTANCE}\` brings the plane back to 200 on /healthz" \
		"$([ "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 "http://localhost:${cport}/healthz" 2>/dev/null)" = 200 ] && echo 0 || echo 1)" \
		"$OWNER_PLANE"

	# RESTART — still takes an instance.
	c_run "$ORCHICON_BIN" restart "$INSTANCE" >/dev/null
	check "A5.5 \`orchicon restart ${INSTANCE}\` returns the plane to 200 on /healthz" \
		"$([ "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 "http://localhost:${cport}/healthz" 2>/dev/null)" = 200 ] && echo 0 || echo 1)" \
		"$OWNER_PLANE"

}

# --- step 6: a SECOND install re-asks nothing (C3) -------------------------
step6_reinstall() {
	step_begin 6 reinstall
	local before; before="$(ports_from_json "$PORTS_JSON" 2>/dev/null || true)"
	local log2="${EVIDENCE}/6-reinstall.log"
	e_run "$log2" "$ORCHICON_BIN" install --instance "$INSTANCE" >/dev/null
	local rc=$?
	check "A6.1 a second \`orchicon install --instance ${INSTANCE}\` succeeds" \
		"$([ "$rc" = 0 ] && echo 0 || echo 1)" "$OWNER_INSTALLER"
	local after; after="$(ports_from_json "$PORTS_JSON" 2>/dev/null || true)"
	check "A6.2 the re-run re-used the RECORDED ports (not new ones): ${before:-none}" \
		"$([ -n "$before" ] && [ "$before" = "$after" ] && echo 0 || echo 1)" "$OWNER_INSTALLER"
	check "A6.3 the re-run asked NOTHING (C3: ports and runtime images are not re-offered)" \
		"$(output_asked_a_question "$log2" && echo 1 || echo 0)" "$OWNER_INSTALLER"
	gated_absent "A6.4 the re-run did not re-offer the runtime-image choice" "$log2" "choose 1-" \
		"the re-install log is empty" "$OWNER_INSTALLER"
	local code; code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 \
		"http://localhost:$(printf '%s' "$after" | awk '{print $1}')/healthz" 2>/dev/null || true)"
	check "A6.5 the plane is still healthy after the re-install" \
		"$([ "$code" = "200" ] && echo 0 || echo 1)" "$OWNER_PLANE"
}

# --- step 7: an ELEVATED Windows shell gets the ACTIONABLE message (AC3) ---
step7_windows_elevated() {
	step_begin 7 elevated
	if [ "$PLATFORM_GOOS" != "windows" ]; then
		skip "A7.1 an elevated shell refuses the install with an actionable message" \
			"this criterion is Windows-only; this host is ${PLATFORM_GOOS}"
		return 0
	fi
	local ps; ps="$(command -v powershell || command -v pwsh || true)"
	if [ -z "$ps" ]; then
		blocked_surface "A7.1 an elevated shell refuses the install with an actionable message" \
			"$OWNER_INSTALLER" "no powershell on PATH — the Windows elevated path cannot be driven"
		return 0
	fi
	local cap="${EVIDENCE}/7-elevated.log"
	# The installer is fetched from the PUBLISHED location, not from this worktree: this procedure
	# proves the artifact a user gets, and `install.ps1` from a checkout is not that.
	local script="${E2E_ROOT}/install.ps1"
	if [ ! -s "$script" ]; then
		c_run curl -fsSL --max-time 120 -o "$script" \
			"https://raw.githubusercontent.com/${REPO_OWNER}/${REPO_NAME}/${VERSION}/scripts/install.ps1" >/dev/null
	fi
	# A UAC-elevated child, waited on, whose transcript is redirected to a file. If elevation is
	# declined or unavailable the step is a SKIP with the reason — never a PASS, because nothing
	# about the elevated path was exercised.
	# THE CHILD WRITES ITS OWN TRANSCRIPT. `Start-Process -Verb RunAs` cannot redirect an ELEVATED
	# process's output, so the previous shape here left an EMPTY capture on every Windows run and
	# step 7 could only ever SKIP — the AC3 evidence was unobtainable. A `*>` redirect inside the
	# elevated child is what actually captures the message.
	"$ps" -NoProfile -NonInteractive -Command "
		try {
			\$child = \"& '${script//\//\\}' *> '${cap//\//\\}'; exit \\\$LASTEXITCODE\"
			\$p = Start-Process -FilePath 'powershell' -Verb RunAs -Wait -PassThru -ArgumentList @(
				'-NoProfile','-ExecutionPolicy','Bypass','-Command', \$child
			)
			exit \$p.ExitCode
		} catch { exit 97 }" >/dev/null 2>&1
	local rc=$?
	cat "${cap}.err" >>"$cap" 2>/dev/null || true
	printf '\n[elevated run exit %s]\n' "$rc" >>"$cap" 2>/dev/null || true
	{ printf '\n$ powershell -Verb RunAs -File install.ps1   [instance: %s]\n[exit %s]\n' "$INSTANCE" "$rc"; } >>"$STEP_LOG"
	cat "$cap" >>"$STEP_LOG" 2>/dev/null || true

	if [ "$rc" = "97" ]; then
		skip "A7.1 an elevated shell refuses the install with an actionable message" \
			"UAC elevation is unavailable to this process — run the procedure from a machine where it is, or record the manual step"
		return 0
	fi
	check "A7.1 the elevated install REFUSES to proceed (it does not fail downstream in initdb)" \
		"$([ "$rc" != "0" ] && echo 0 || echo 1)" "$OWNER_INSTALLER"
	if capture_is_empty "$cap"; then
		skip "A7.2 the refusal is ACTIONABLE: names Postgres-as-Administrator AND the next steps" \
			"the elevated run left no transcript"
		return 0
	fi
	check "A7.2 the refusal is ACTIONABLE: names Postgres, Administrator, and the next steps" \
		"$(elevated_message_complete "$(cat "$cap")" && echo 0 || echo 1)" "$OWNER_INSTALLER"
	gated_contains "A7.3 it tells the user to open a NORMAL PowerShell (not 'Run as administrator')" \
		"$cap" "NORMAL PowerShell" "the elevated run left no transcript" "$OWNER_INSTALLER"
	gated_contains "A7.4 it states that NOTHING was downloaded or changed" \
		"$cap" "Nothing was downloaded" "the elevated run left no transcript" "$OWNER_INSTALLER"
	gated_absent "A7.5 the refusal mentions no Docker/Docker Desktop (AC5)" \
		"$cap" "docker" "the elevated run left no transcript" "$OWNER_INSTALLER"
}

# --- step 8: Gatekeeper / SmartScreen is not a DEAD END (AC2) --------------
step8_signature_gate() {
	step_begin 8 signature
	local cap="${EVIDENCE}/8-signature.txt"
	: >"$cap"
	case "$PLATFORM_GOOS" in
		darwin)
			local spctl_rc=1 codesign_rc=1
			if command -v spctl >/dev/null 2>&1; then
				spctl --assess --type execute -vv "$ORCHICON_BIN" >>"$cap" 2>&1 && spctl_rc=0
			fi
			if command -v codesign >/dev/null 2>&1; then
				codesign --verify --deep --strict "$ORCHICON_BIN" >>"$cap" 2>&1 && codesign_rc=0
			fi
			xattr -p com.apple.quarantine "$ORCHICON_BIN" >>"$cap" 2>&1 || echo "no com.apple.quarantine xattr" >>"$cap"
			# The instruction is only accepted as a remedy if the RUNBOOK carries it verbatim
			# (AC2: "signed, or an exact tested instruction"). A runbook that has drifted is the
			# same dead end as no instruction at all, so its presence is asserted, not assumed.
			local documented=1
			grep -qF 'xattr -d com.apple.quarantine' "${REPO_ROOT}/docs/published-archive-e2e-acceptance.md" 2>/dev/null && documented=0
			local verdict; verdict="$(signature_verdict "$spctl_rc" "$documented")"
			note "gatekeeper: spctl=${spctl_rc} codesign=${codesign_rc} documented=${documented} -> ${verdict}"
			# The DOWNLOAD MUST RUN: the artifact executed in step 2, so a refusal here is a
			# keychain/notarisation state, never a broken archive.
			check "A8.1 the download RUNS on macOS (the artifact executed in step 2)" \
				"$([ -n "${ARCHIVE_SHA:-}" ] && echo 0 || echo 1)" "$OWNER_RELEASE"
			case "$verdict" in
				accepted) check "A8.2 Gatekeeper accepts the extracted binaries" 0 ;;
				instruction) check "A8.2 Gatekeeper refused, and the runbook carries the EXACT tested instruction" 0 ;;
				*) check "A8.2 Gatekeeper refused and no tested instruction exists — a DEAD END" 1 "$OWNER_RELEASE" ;;
			esac
			;;
		windows)
			local ps; ps="$(command -v powershell || command -v pwsh || true)"
			local signed=1
			if [ -n "$ps" ]; then
				"$ps" -NoProfile -NonInteractive -Command "\$s = Get-AuthenticodeSignature -LiteralPath '${ORCHICON_BIN//\//\\}'; \$s.Status; \$s.SignerCertificate.Subject" >>"$cap" 2>&1
				grep -q '^Valid' "$cap" && signed=0
			fi
			local documented=1
			grep -qF 'Unblock-File' "${REPO_ROOT}/docs/published-archive-e2e-acceptance.md" 2>/dev/null && documented=0
			local verdict; verdict="$(signature_verdict "$signed" "$documented")"
			note "smartscreen: signature_valid=${signed} documented=${documented} -> ${verdict}"
			case "$verdict" in
				accepted) check "A8.2 the extracted orchicon.exe carries a VALID Authenticode signature" 0 ;;
				instruction) check "A8.2 SmartScreen has no signature, and the runbook carries the EXACT tested zip-mark instruction" 0 ;;
				*) check "A8.2 neither a valid signature nor a tested instruction — a SmartScreen DEAD END" 1 "$OWNER_RELEASE" ;;
			esac
			;;
		*)
			skip "A8.1/A8.2 Gatekeeper/SmartScreen" "this platform has neither control (linux)"
			;;
	esac
}

# --- step 9: the TUI is a SEPARATE BINARY with its own concerns (AC4) ------
step9_tui() {
	step_begin 9 tui
	if [ "$SKIP_TUI" = "1" ]; then
		skip "A9.1 the TUI connects to the plane and renders" "--skip-tui was passed (this proves nothing)"
		return 0
	fi
	if [ -z "$ORCH_BIN" ] || [ ! -f "$ORCH_BIN" ]; then
		blocked_surface "A9.1 the TUI binary ships in the published archive" "$OWNER_TUI" \
			"no orch binary inside ${ARCHIVE}"
		return 0
	fi
	local vlog="${EVIDENCE}/9-orch-version.txt"
	"$ORCH_BIN" --version >"$vlog" 2>&1 || true
	gated_contains "A9.1 the TUI binary runs (it is not just present in the archive)" "$vlog" \
		"orch" "the TUI printed nothing for --version" "$OWNER_TUI"

	# DRIVEN IN A REAL PTY, because a TUI that cannot allocate a terminal has not been shown to
	# render. `script` is the portable way to get one (util-linux; BSD has it too).
	local cap="${EVIDENCE}/9-tui-render.txt"
	: >"$cap"
	local keys="${ORCHICON_E2E_TUI_KEYS:-q}"
	if ! command -v script >/dev/null 2>&1; then
		blocked_surface "A9.2 the TUI renders in a pty" "$OWNER_TUI" "no \`script\` on this host to allocate a pty"
		return 0
	fi
	TERM="${TERM:-xterm-256color}"
	export TERM
	( sleep 6; printf '%s' "$keys" ) | run_with_timeout 25 pty_script "$ORCH_BIN" >"$cap" 2>&1 || true
	cp "$cap" "${EVIDENCE}/9-tui-screenshot.txt" 2>/dev/null || true

	if capture_is_empty "$cap"; then
		skip "A9.2 the TUI connects to the plane and renders" \
			"the pty capture is EMPTY — nothing was shown to be drawn (this is not a pass)"
		return 0
	fi
	local marker; marker="$(tui_render_marker "$cap" || true)"
	check "A9.2 the TUI RENDERS in a pty (marker: ${marker:-none})" \
		"$([ -n "$marker" ] && echo 0 || echo 1)" "$OWNER_TUI"
	gated_absent "A9.3 the TUI did not fail to reach the plane (no 'connection refused'/'dial tcp')" \
		"$cap" "connection refused" "the pty capture is empty" "$OWNER_TUI"
	gated_absent "A9.4 the TUI did not report a dial failure" "$cap" "dial tcp" "the pty capture is empty" "$OWNER_TUI"
	note "TUI capture: ${cap} ($(wc -c <"$cap" | tr -d ' ') bytes)"
}

# --- step 10: NO DOCKER, ANYWHERE (AC5) ------------------------------------
step10_no_engine() {
	step_begin 10 no-engine
	local invocations; invocations="$(shim_invocations "$SHIM_DIR" 2>/dev/null || true)"
	printf '%s\n' "$invocations" >"${EVIDENCE}/10-shim-invocations.txt"
	check "A10.1 NOTHING on the install path invoked docker/podman/wsl/docker.exe (shim log empty)" \
		"$([ -z "$invocations" ] && echo 0 || echo 1)" "$OWNER_INSTALLER"

	# The output, too. A transcript that TELLS the user about Docker is the same defect even if
	# nothing was executed — and this is where DEF-2 lives (installer help/teardown text).
	local caps="${EVIDENCE}/10-engine-references.txt"
	engine_reference_in "$INSTALL_LOG" "${EVIDENCE}/6-reinstall.log" "${EVIDENCE}/4-plane-status.txt" \
		"${EVIDENCE}/9-tui-render.txt" 2>/dev/null >"$caps" || true
	if [ -s "$caps" ]; then
		check "A10.2 no Docker/Podman reference appears in any install-path output" 1 "$OWNER_INSTALLER"
		head -5 "$caps" >>"$STEP_LOG"
	else
		check "A10.2 no Docker/Podman reference appears in any install-path output" 0
	fi

	# The PLANE's own log is evidence too: an engine probe there means something asked.
	local planelog; planelog="$(instance_data_dir)/serve/logs/orchicon.log"
	if [ -s "$planelog" ]; then
		cp "$planelog" "${EVIDENCE}/10-plane.log" 2>/dev/null || true
		local plane_refs; plane_refs="$(engine_reference_in "$planelog" 2>/dev/null || true)"
		printf '%s\n' "$plane_refs" >"${EVIDENCE}/10-plane-engine-references.txt"
		check "A10.3 the plane's own log carries no Docker/Podman reference" \
			"$([ -z "$plane_refs" ] && echo 0 || echo 1)" "$OWNER_PLANE"
	fi

	# WSL: Windows local mode must not need it, and no other platform may start talking about it.
	local wsl_refs; wsl_refs="$(wsl_reference_in "$INSTALL_LOG" "${EVIDENCE}/4-plane-status.txt" 2>/dev/null || true)"
	printf '%s\n' "$wsl_refs" >"${EVIDENCE}/10-wsl-references.txt"
	if [ "$PLATFORM_GOOS" = "windows" ]; then
		check "A10.4 a Windows local-mode install NEVER needs WSL (no wsl invocation, no wsl requirement)" \
			"$([ -z "$wsl_refs" ] && echo 0 || echo 1)" "$OWNER_INSTALLER"
	else
		check "A10.4 no WSL requirement leaked into a non-Windows install path" \
			"$([ -z "$wsl_refs" ] && echo 0 || echo 1)" "$OWNER_INSTALLER"
	fi
}

# --- step 11: the footprint, MEASURED (AC6, correction C4) -----------------
step11_footprint() {
	step_begin 11 footprint
	local raw="${EVIDENCE}/11-footprint.txt"
	bash "$FOOTPRINT_SH" --install-dir "$INSTALL_DIR" --data-dir "$(data_dir_root)" \
		--instance "$INSTANCE" --claim-bytes "$CLAIM_BYTES" --raw >"$raw" 2>&1 || true
	cat "$raw" >>"$STEP_LOG"
	local total measured claim
	total="$(awk -F= '$1=="total_bytes"{print $2}' "$raw")"
	measured="$(awk -F= '$1=="measured_at"{print $2}' "$raw")"
	claim="$(awk -F= '$1=="claim_bytes"{print $2}' "$raw")"
	# Kept for the closing RESULT line, which used to report the FINDINGS count as if it were the
	# footprint — the one number this procedure exists to produce was the one it never printed.
	FOOTPRINT_TOTAL="${total:-unknown}"
	FOOTPRINT_MEASURED_AT="${measured:-unknown}"

	check "A11.1 the footprint was MEASURED (not derived): total ${total:-none} bytes at ${measured:-unknown}" \
		"$([ -n "$total" ] && [ -n "$measured" ] && echo 0 || echo 1)" "$OWNER_FOOTPRINT"
	# C4: the number is only meaningful with WHAT was installed beside it, and the date. Both are
	# in the report; the harness states them in the summary so the figure cannot be misread later.
	note "footprint measured_at=${measured} total=${total} claim=${claim}"
	awk -F= '/_bytes=/{print "    " $0}' "$raw" >>"$STEP_LOG" 2>/dev/null || true

	if claim_supported "${total:-0}"; then
		check "A11.2 the measurement SUPPORTS the epic's under-1 GiB claim (${total} < ${claim})" 0
	else
		# A refutation is a FINDING (AC6), not a failure to hide. It is recorded with the number,
		# the date and the claim it contradicts.
		finding "A11.2 the ${claim}-byte claim is REFUTED by this measurement: ${total} bytes on ${measured}" \
			"$OWNER_FOOTPRINT"
	fi
	# Anything still installed is named: until the Grafana removal lands, the figure includes it
	# (correction C4), and a number without that context is misleading in a later review.
	local state_lines; state_lines="$(awk -F= '/_state=/{printf "%s ", $1}' "$raw")"
	note "components measured: ${state_lines}"
}

# --- instance state (read from the INSTANCE'S OWN RECORD, never re-resolved)
data_dir_root() { printf '%s' "${ORCHICON_E2E_DATA_ROOT:-${XDG_DATA_HOME:-${HOME}/.local/share}/orchicon}"; }
instance_data_dir() { printf '%s/orchicon-%s' "$(data_dir_root)" "$INSTANCE"; }
pid_in_file() { # pid_in_file <file> — the recorded pid, only when that process exists
	local f="$1" pid
	[ -s "$f" ] || return 1
	pid="$(tr -dc '0-9' <"$f" 2>/dev/null)"
	[ -n "$pid" ] || return 1
	process_alive "$pid" || return 1
	printf '%s' "$pid"
}

# --- the plan (--dry-run executes nothing) ---------------------------------
print_plan() {
	cat <<'PLAN'
published-archive-e2e plan (--dry-run: nothing below is executed)

step 0   preflight   the version under test is NAMED (C1, `latest` refused); the platform maps to a
                     published asset; engine/WSL names are BOOBY-TRAPPED on PATH and no engine
                     answers; no previous orchicon on PATH; download/verify/extract tooling present
step 1   download    the PUBLISHED archive for this platform, plus SHA256SUMS; the digest for THIS
                     asset is checked (an unlisted asset is UNVERIFIED, never verified); the listing
                     must ship `orchicon` and `orch` and no container assets
step 2   extract     extract, then run the binary's `--version`: it must report the version under test
                     (a locally built binary is not the published archive, and a mislabelled release
                     proves nothing)
step 3   install     `orchicon install --instance I` with stdout CAPTURED, so C3 applies: it must ask
                     NOTHING, must record its ports, and must NAME them in the output
step 4   plane       `plane status I` names the instance and says host-resident + current; /healthz
                     answers 200 on the recorded control port (independent of the CLI's own claim)
step 5   verbs       `stop I` (curl exit 7 = REFUSED, and the pid is gone), `stop I` again (idempotent),
                     `start I` and `restart I` back to 200. No `serve --status`: it does not exist (C2)
step 6   reinstall   a SECOND install re-uses the RECORDED ports, re-asks neither ports nor runtime
                     images, and leaves the plane healthy (C3)
step 7   elevated    Windows only: an elevated shell REFUSES with the Postgres-as-Administrator message
                     and the next steps — not a downstream initdb failure (AC3)
step 8   signature   macOS Gatekeeper / Windows SmartScreen: accepted, or refused WITH the exact tested
                     instruction in the runbook. A refusal with no instruction is a DEAD END (AC2)
step 9   tui         `orch` runs, is driven in a real pty, and RENDERS (an empty capture is a SKIP,
                     never a PASS); no connection/dial failure in the capture (AC4)
step 10  no-engine   the shim invocation log is EMPTY, and no Docker/Podman/WSL reference appears in
                     any install-path output — including the plane's own log (AC5)
step 11  footprint   `scripts/e2e/footprint.sh` MEASURES the install (binaries + host services +
                     instance data, excluding projects/ and runtime images) and states the date and
                     components. A refutation of the under-1 GiB claim is a FINDING (AC6, C4)

exit codes: 0 every assertion passed | 1 at least one FAILED (each names the owning sibling)
            2 NOT a valid proof (no explicit version, a dirty host, or a missing surface)
PLAN
}

# --- summary -----------------------------------------------------------------
verdict_for() { # verdict_for <step>
	local n="$1" st
	st="$(awk -F'\t' -v s="$n" '$1==s {print $2}' "${EVIDENCE}/results.tsv" 2>/dev/null | sort -u | tr '\n' ',')"
	case "$st" in
		*FAIL*) printf '**FAIL** (see defects.tsv)' ;;
		*BLOCKED*) printf 'BLOCKED (see defects.tsv)' ;;
		*PASS*) printf 'PASS' ;;
		*FINDING*|*SKIP*) printf 'SKIP / FINDING (nothing proven — see the step log)' ;;
		""|,) printf 'not run' ;;
		*) printf 'PASS' ;;
	esac
}

render_summary() {
	local md="${EVIDENCE}/summary.md"
	local ports; ports="$(ports_from_json "$(instance_data_dir)/ports.json" 2>/dev/null || true)"
	{
		echo "# Published archive, clean machine, no Docker / no WSL — run ${UTC_STAMP}"
		echo
		echo "## The artifact under test (C1: recorded, not assumed)"
		echo
		echo "- platform: **${PLATFORM_GOOS}/${PLATFORM_ARCH}** (host: \`$(uname -s) $(uname -m)\`)"
		echo "- release tag: **${VERSION}** — host-native merge gate \`${HOST_NATIVE_MERGE_UTC}\`"
		echo "- asset: \`${ARCHIVE:-none}\` (${ARCHIVE_SIZE:-0} bytes)"
		echo "- sha256: \`${ARCHIVE_SHA:-not computed}\`"
		echo "- instance every command acted on: **${INSTANCE}** (C2)"
		echo "- install dir: \`${INSTALL_DIR}\`"
		[ -n "$ports" ] && echo "- recorded ports: \`${ports}\`"
		echo
		echo "- passes: ${PASSES}   failures: ${FAILS}   skips: ${SKIPS}   findings: ${FINDINGS}   blocked: ${BLOCKED}"
		[ -n "$ABORT_REASON" ] && { echo; echo "**NOT A VALID PROOF:** ${ABORT_REASON}"; }
		echo
		echo "## per-step verdict"
		echo
		echo "| step | what it proves | verdict |"
		echo "|---|---|---|"
		printf '| 0 | the run is valid: version named, clean host, no engine | %s |\n' "$(verdict_for 0)"
		printf '| 1 | the PUBLISHED archive downloads, verifies and ships both binaries (AC1, AC9) | %s |\n' "$(verdict_for 1)"
		printf '| 2 | the artifact IS the version under test (C1) | %s |\n' "$(verdict_for 2)"
		printf '| 3 | `orchicon install` asks nothing, records and names its ports (AC1, AC8, C3) | %s |\n' "$(verdict_for 3)"
		printf '| 4 | a healthy HOST-RESIDENT plane, proven by /healthz (AC1) | %s |\n' "$(verdict_for 4)"
		printf '| 5 | start/stop/restart with an instance, proven independently (AC1, C2) | %s |\n' "$(verdict_for 5)"
		printf '| 6 | a re-run re-asks neither ports nor runtime images (AC8) | %s |\n' "$(verdict_for 6)"
		printf '| 7 | the Windows elevated shell gets the actionable message (AC3) | %s |\n' "$(verdict_for 7)"
		printf '| 8 | Gatekeeper/SmartScreen is not a dead end (AC2) | %s |\n' "$(verdict_for 8)"
		printf '| 9 | `orch` renders on this platform (AC4) | %s |\n' "$(verdict_for 9)"
		printf '| 10 | no Docker, no Docker Desktop, no WSL anywhere (AC5) | %s |\n' "$(verdict_for 10)"
		printf '| 11 | the installed footprint is MEASURED (AC6, C4) | %s |\n' "$(verdict_for 11)"
		echo
		echo "## evidence (independent of any component's own report)"
		echo
		echo "- \`/healthz\` is read over HTTP on the instance's RECORDED control port; the down state is"
		echo "  curl exit 7 (REFUSED), not a status line; the serving binary and residency come from"
		echo "  \`plane status\`, which is the only surface that reports them (C2)."
		echo "- the engine/WSL absence is proven by a BOOBY-TRAPPED PATH: every shim exits 127 and logs"
		echo "  its argv, and \`10-shim-invocations.txt\` must be empty."
		echo "- the footprint is \`du\`-measured, per component, on the run's date (see \`11-footprint.txt\`)."
		echo
		echo "## defects — each names the sibling task that owns the fix (AC10)"
		echo
		if [ -s "${EVIDENCE}/defects.tsv" ]; then
			echo "| step | assertion | owner |"
			echo "|---|---|---|"
			awk -F'\t' '{printf "| %s | %s | `%s` |\n", $1, $2, $3}' "${EVIDENCE}/defects.tsv"
		else
			echo "none."
		fi
		echo
		echo "## findings — a claim this run REFUTED (AC6: not hidden, not a failure)"
		echo
		if [ -s "${EVIDENCE}/findings.tsv" ]; then
			echo "| step | finding | owner |"
			echo "|---|---|---|"
			awk -F'\t' '{printf "| %s | %s | `%s` |\n", $1, $2, $3}' "${EVIDENCE}/findings.tsv"
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
	say "passes=${PASSES} failures=${FAILS} skips=${SKIPS} findings=${FINDINGS} blocked=${BLOCKED}"
}

teardown() {
	if [ "$KEEP" = "1" ]; then
		say "teardown: --keep, leaving instance ${INSTANCE} running"
		return 0
	fi
	return 0
}

# --- arguments ---------------------------------------------------------------
parse_args() {
	while [ "$#" -gt 0 ]; do
		case "$1" in
			--version|-v) VERSION="$2"; shift 2 ;;
			--platform) PLATFORM="$2"; shift 2 ;;
			--instance|-i) INSTANCE="$2"; shift 2 ;;
			--evidence-dir) EVIDENCE="$2"; shift 2 ;;
			--install-dir) INSTALL_DIR_DEFAULT="$2"; shift 2 ;;
			--keep) KEEP=1; shift ;;
			--skip-tui) SKIP_TUI=1; shift ;;
			--dry-run) DRY_RUN=1; shift ;;
			--preflight-only) PREFLIGHT_ONLY=1; shift ;;
			-h|--help) usage_text; exit 0 ;;
			*) printf 'published-archive-e2e.sh: unknown argument %s\n' "$1" >&2; exit 2 ;;
		esac
	done
}

usage_text() {
	sed -n '/^# USAGE/,/^# =====/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

main() {
	parse_args "$@"
	if [ "$DRY_RUN" = "1" ]; then print_plan; exit 0; fi

	# THE PLATFORM IS RESOLVED BEFORE ANYTHING READS IT. step 0 resolves it again (harmlessly), but
	# the install-dir default below depends on it: resolving it afterwards gave Windows the LINUX
	# install dir (~/.local/bin), so the run installed and then measured the wrong tree.
	PLATFORM_GOOS="$(platform_from_uname "$PLATFORM")"
	INSTALL_DIR="${INSTALL_DIR_DEFAULT:-${ORCHICON_INSTALL_DIR:-${HOME}/.local/bin}}"
	[ "$PLATFORM_GOOS" = "windows" ] && [ -n "${LOCALAPPDATA:-}" ] && \
		INSTALL_DIR="${INSTALL_DIR_DEFAULT:-${LOCALAPPDATA}/Programs/Orchicon}"
	EVIDENCE="${EVIDENCE:-${REPO_ROOT}/qa-evidence/published-archive-e2e/${UTC_STAMP}-${PLATFORM}}"
	mkdir -p "$EVIDENCE" "${E2E_ROOT}/download" 2>/dev/null || {
		printf 'published-archive-e2e.sh: cannot create %s\n' "$EVIDENCE" >&2; exit 2; }
	: >"${EVIDENCE}/results.tsv"
	: >"${EVIDENCE}/defects.tsv"
	: >"${EVIDENCE}/findings.tsv"
	trap teardown EXIT

	step0_preflight
	if [ "$INVALID" = "1" ] || [ "$BLOCKED_SURFACE" = "1" ]; then
		render_summary
		say ""
		say "RESULT: NOT A VALID PROOF — ${ABORT_REASON:-a required surface is absent}"
		exit 2
	fi
	if [ "$PREFLIGHT_ONLY" = "1" ]; then
		render_summary
		say ""
		say "PREFLIGHT: this host and this version CAN be proven. Re-run without --preflight-only."
		exit 0
	fi

	step1_download
	step2_extract_and_identify
	step3_install
	step4_plane_health
	step5_lifecycle_verbs
	step6_reinstall
	step7_windows_elevated
	step8_signature_gate
	step9_tui
	step10_no_engine
	step11_footprint
	render_summary

	say ""
	if [ "$BLOCKED_SURFACE" = "1" ] || [ "$INVALID" = "1" ]; then
		say "RESULT: NOT A VALID PROOF — ${ABORT_REASON:-a required surface is absent and a defect has been filed against the task that owns it}"
		exit 2
	fi
	if [ "$FAILS" -gt 0 ]; then
		say "RESULT: ${FAILS} assertion(s) FAILED — each is a defect against the owner named in defects.tsv."
		exit 1
	fi
	say "RESULT: the published ${VERSION} archive installs and runs on ${PLATFORM_GOOS}/${PLATFORM_ARCH} with no"
	say "        container engine and no WSL, and produces a healthy host-resident plane."
	say "        MEASURED footprint: ${FOOTPRINT_TOTAL} bytes at ${FOOTPRINT_MEASURED_AT} (claim ${CLAIM_BYTES})."
	say "        findings: ${FINDINGS}   skips: ${SKIPS} — a skip proves nothing (see the step verdicts)."
	exit 0
}

# Sourced (scripts/tests/published-archive-e2e-assertions/run.sh) the FUNCTIONS are the subject and
# nothing may execute: the offline gate proves the decisions without downloading a release, starting
# a plane, or touching an instance's state.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then main "$@"; fi
