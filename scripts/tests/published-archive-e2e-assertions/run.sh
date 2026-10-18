#!/usr/bin/env bash
# ============================================================================
# Offline gate for the PUBLISHED-ARCHIVE acceptance harness's own honesty.
#
# WHY THIS EXISTS. Every verdict in scripts/published-archive-e2e.sh is either
# ABSENCE-SHAPED ("no engine was invoked", "no Docker reference appears", "the
# install asked nothing") or IDENTITY-SHAPED ("this artifact IS the version under
# test", "this number IS the footprint"). Both are trivially true of a probe that
# FAILED TO RUN, and both are the difference between a proven artifact and a
# confident-sounding report about nothing:
#
#   1. A checksum that is not in SHA256SUMS must read UNVERIFIED, never verified
#      (that is DEF-1, live in the release matrix today).
#   2. `latest` must be REFUSED — the wrong artifact proves the wrong version (C1).
#   3. An EMPTY capture must never produce a PASS: the TUI, the elevated-shell
#      transcript and the install log are all captures, and a vacuous PASS on one
#      of them is how "it rendered" gets reported with nothing on screen.
#   4. The engine shims must really FAIL and really LOG, or "nothing invoked
#      Docker" is a statement about a PATH that was never poisoned.
#   5. The footprint must be MEASURED: bytes that match `du`, absent components
#      counted as absent, and a refutation recorded as a FINDING rather than a
#      failure or a silent pass (AC6, C4).
#   6. The verbs are `start`/`stop`/`plane status` AND EVERY ONE NAMES AN INSTANCE
#      (C2) — a harness that calls `orchicon stop` with no instance is proving
#      something about whichever instance happened to be exported (C2).
#
# WHAT IS REAL HERE: the harness's own functions are SOURCED and executed, the
# shims are created in a throwaway dir and actually run, and footprint.sh really
# measures a fixture tree. No release is downloaded, no plane starts, no port is
# bound, no database is touched — which is why this gate lives in ci-go.
#
#   scripts/tests/published-archive-e2e-assertions/run.sh
#
# Exits 0 only when every assertion passes.
# ============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
HARNESS="${REPO_ROOT}/scripts/published-archive-e2e.sh"
FOOTPRINT="${REPO_ROOT}/scripts/e2e/footprint.sh"
NOENGINE="${REPO_ROOT}/scripts/e2e/no-engine-path.sh"
RUNBOOK="${REPO_ROOT}/docs/published-archive-e2e-acceptance.md"

PASSED=0
FAILED=0

expect() { # <label> <expected> <actual>
	if [ "$2" = "$3" ]; then
		printf '  \033[32mPASS\033[0m  %-64s %s\n' "$1" "$3"
		PASSED=$((PASSED + 1))
	else
		printf '  \033[31mFAIL\033[0m  %-64s want [%s] got [%s]\n' "$1" "$2" "$3"
		FAILED=$((FAILED + 1))
	fi
}
spin() { echo; echo "  -- $* --"; }
rc() { "$@" >/dev/null 2>&1; echo $?; }

for f in "$HARNESS" "$FOOTPRINT" "$NOENGINE" "$RUNBOOK"; do
	if [ ! -f "$f" ]; then
		echo "run.sh: missing ${f}" >&2
		exit 2
	fi
done

WORK="${REPO_ROOT}/tmp/published-archive-e2e-assertions.$$"
rm -rf "$WORK" 2>/dev/null || true
mkdir -p "$WORK" || { echo "run.sh: cannot create ${WORK}" >&2; exit 1; }
trap 'rm -rf "$WORK" 2>/dev/null || true' EXIT

# --- the harness must be SOURCEABLE and must not execute --------------------
spin "the harness is a library when sourced"
SOURCE_RC=0
# shellcheck source=../../published-archive-e2e.sh
. "$HARNESS" >/dev/null 2>&1 || SOURCE_RC=$?
expect "sourcing scripts/published-archive-e2e.sh never executes the procedure" 0 "$SOURCE_RC"
expect "sourcing chooses no evidence dir (no run happened)" "" "${EVIDENCE}"
expect "sourcing starts nothing (DIRTY is 0)" 0 "${DIRTY}"
expect "the version under test has NO default (C1: it must be named)" "" "${VERSION}"

for fn in record check skip finding blocked_surface invalid_host note step_begin c_run e_run \
          platform_from_uname arch_from_uname archive_ext archive_name version_is_explicit \
          iso_date_lt release_postdates_host_native checksum_line_for output_asked_a_question \
          engine_reference_in wsl_reference_in ports_named_in_output ports_from_json \
          claim_supported capture_is_empty gated_contains gated_absent \
          status_names_instance status_says_host_resident status_says_container \
          status_says_healthy_at status_says_current status_says_superseded \
          residency_refusal_in elevated_message_complete signature_verdict tui_render_marker \
          sha256_of data_dir_root instance_data_dir pid_in_file; do
	expect "the harness defines ${fn}()" "function" "$(type -t "$fn" 2>/dev/null)"
done
for fn in step0_preflight step1_download step2_extract_and_identify step3_install \
          step4_plane_health step5_lifecycle_verbs step6_reinstall step7_windows_elevated \
          step8_signature_gate step9_tui step10_no_engine step11_footprint \
          render_summary print_plan parse_args main; do
	expect "the harness defines ${fn}() (one function per step)" "function" "$(type -t "$fn" 2>/dev/null)"
done

# --- the artifact identity (C1) --------------------------------------------
spin "the platform maps to an asset the release matrix actually publishes"
expect "Linux -> linux"        linux  "$(platform_from_uname Linux)"
expect "Darwin -> darwin"      darwin "$(platform_from_uname Darwin)"
expect "MINGW64_NT -> windows" windows "$(platform_from_uname MINGW64_NT-10.0-19045)"
expect "MSYS -> windows"       windows "$(platform_from_uname MSYS_NT-10.0)"
expect "an unknown kernel is NOT guessed" unknown "$(platform_from_uname FreeBSD)"
expect "a --platform override in lowercase still resolves (linux)" linux "$(platform_from_uname linux)"
expect "…and darwin/osx do too" "darwin darwin" \
	"$(platform_from_uname darwin) $(platform_from_uname osx)"
expect "x86_64 -> amd64"       amd64  "$(arch_from_uname x86_64)"
expect "arm64 -> arm64"        arm64  "$(arch_from_uname arm64)"
expect "aarch64 -> arm64"      arm64  "$(arch_from_uname aarch64)"
expect "linux archives are tar.gz"   tar.gz "$(archive_ext linux)"
expect "darwin archives are tar.gz"  tar.gz "$(archive_ext darwin)"
expect "windows archives are zip"    zip    "$(archive_ext windows)"
expect "an unpublished platform has no archive" "" "$(archive_ext freebsd)"
expect "the linux asset name matches release.yml's ASSET=" \
	"orchicon_1.2.3_linux_amd64.tar.gz" "$(archive_name v1.2.3 linux amd64)"
expect "the macOS arm64 asset name" \
	"orchicon_1.2.3_darwin_arm64.tar.gz" "$(archive_name v1.2.3 darwin arm64)"
expect "the windows asset name is a .zip" \
	"orchicon_1.2.3_windows_amd64.zip" "$(archive_name v1.2.3 windows amd64)"
expect "an unpublished platform yields no asset name (not a hopeful guess)" 1 \
	"$(rc archive_name v1.2.3 freebsd amd64)"

spin "C1: 'latest' is REFUSED and a named version is accepted"
for bad in "" latest LATEST stable current nightly main master develop; do
	expect "version_is_explicit rejects '${bad}'" 1 "$(rc version_is_explicit "$bad")"
done
for good in v1.0.0 v0.2.0-rc1 1.2.3 v2026.10.18; do
	expect "version_is_explicit accepts '${good}'" 0 "$(rc version_is_explicit "$good")"
done
expect "a release dated BEFORE the host-native merge does not qualify" 1 \
	"$(rc release_postdates_host_native 2026-10-17)"
expect "a release dated ON the merge day qualifies" 0 \
	"$(rc release_postdates_host_native 2026-10-18)"
expect "a release dated AFTER the merge qualifies" 0 \
	"$(rc release_postdates_host_native 2026-11-01)"
expect "an UNDATED release does not qualify (unknown is not 'after')" 1 \
	"$(rc release_postdates_host_native '')"

# --- the checksum, including the DEF-1 shape -------------------------------
spin "an unlisted asset is UNVERIFIED, never verified (DEF-1)"
cat >"${WORK}/SHA256SUMS" <<'SUMS'
aaa111  orchicon_1.2.3_linux_amd64.tar.gz
bbb222  ./orchicon_1.2.3_darwin_arm64.tar.gz
ccc333 *orchicon_1.2.3_darwin_amd64.tar.gz
SUMS
expect "a listed asset yields its digest" "aaa111" \
	"$(checksum_line_for orchicon_1.2.3_linux_amd64.tar.gz "${WORK}/SHA256SUMS")"
expect "a ./ -prefixed line is matched by basename" "bbb222" \
	"$(checksum_line_for orchicon_1.2.3_darwin_arm64.tar.gz "${WORK}/SHA256SUMS")"
expect "a *-prefixed (binary marker) line is matched" "ccc333" \
	"$(checksum_line_for orchicon_1.2.3_darwin_amd64.tar.gz "${WORK}/SHA256SUMS")"
expect "an asset absent from SHA256SUMS reads UNVERIFIED — the Windows zip's state today (DEF-1)" 1 \
	"$(rc checksum_line_for orchicon_1.2.3_windows_amd64.zip "${WORK}/SHA256SUMS")"
expect "…and an absent asset prints NOTHING (empty is not a digest)" "" \
	"$(checksum_line_for orchicon_1.2.3_windows_amd64.zip "${WORK}/SHA256SUMS" 2>/dev/null)"
expect "a missing SHA256SUMS file yields nothing rather than a false digest" 1 \
	"$(rc checksum_line_for orchicon_1.2.3_linux_amd64.tar.gz "${WORK}/no-such-file")"

# --- C3: a captured run asked nothing --------------------------------------
spin "C3: a captured install must not ask, and must say which ports it used"
printf 'resolving ports for this instance …\ncontrol port 18080 (recorded)\npostgres port 15432\ninstalled\n' \
	>"${WORK}/install-clean.log"
printf 'Choose runtime images:\n  1) all\n  2) none\n  choose 1-3 or a name: ' >"${WORK}/install-prompt.log"
printf 'proceed? [y/N] ' >"${WORK}/install-yn.log"
: >"${WORK}/install-empty.log"
expect "a prompt-free install is recognised as ASKING NOTHING" 1 \
	"$(rc output_asked_a_question "${WORK}/install-clean.log")"
expect "a numeric choice prompt is caught" 0 "$(rc output_asked_a_question "${WORK}/install-prompt.log")"
expect "a [y/N] prompt is caught" 0 "$(rc output_asked_a_question "${WORK}/install-yn.log")"
expect "an empty capture cannot claim it asked nothing" 1 \
	"$(rc output_asked_a_question "${WORK}/install-empty.log")"
expect "every recorded port is found in the output" 0 \
	"$(rc ports_named_in_output "${WORK}/install-clean.log" 18080 15432)"
expect "a port MISSING from the output is a failure, not a shrug" 1 \
	"$(rc ports_named_in_output "${WORK}/install-clean.log" 18080 19999)"
expect "an empty capture names no ports" 1 \
	"$(rc ports_named_in_output "${WORK}/install-empty.log" 18080)"
printf '{"ORCHICON_CONTROL_PORT": 18080, "ORCHICON_POSTGRES_PORT": 15432}\n' >"${WORK}/ports.json"
expect "the instance's own port record is read back" "18080 15432" \
	"$(ports_from_json "${WORK}/ports.json" | tr -s ' ' | sed 's/^ //;s/ $//')"
expect "a missing port record yields nothing" 1 "$(rc ports_from_json "${WORK}/nope.json")"

# --- empty captures can never PASS (the vacuity rule) ----------------------
spin "an EMPTY capture is SKIP, never PASS — for the TUI, the install and the elevated shell"
EVIDENCE="${WORK}/evidence"; mkdir -p "$EVIDENCE"
: >"${EVIDENCE}/results.tsv"; : >"${EVIDENCE}/defects.tsv"; : >"${EVIDENCE}/findings.tsv"
STEP_LOG="${EVIDENCE}/0-log.txt"; : >"$STEP_LOG"
CURRENT_STEP="9"
PASSES=0; FAILS=0; SKIPS=0; FINDINGS=0

WS_ONLY="${WORK}/capture-whitespace.txt"; printf '   \n\t\n\r\n' >"$WS_ONLY"
gated_contains "the TUI rendered" "${WORK}/install-empty.log" "Orchicon" "nothing was drawn" >/dev/null
expect "gated_contains on an EMPTY capture counts no PASS" 0 "$PASSES"
expect "gated_contains on an EMPTY capture counts one SKIP" 1 "$SKIPS"
PASSES=0; FAILS=0; SKIPS=0
gated_contains "the TUI rendered" "$WS_ONLY" "Orchicon" "nothing was drawn" >/dev/null
expect "a WHITESPACE-ONLY capture is empty too (no PASS)" "0 1" "$PASSES $SKIPS"
PASSES=0; FAILS=0; SKIPS=0
gated_absent "the TUI did not fail to connect" "${WORK}/install-empty.log" "connection refused" "nothing was drawn" \
	>/dev/null
expect "gated_absent on an EMPTY capture counts no PASS (an absence proves nothing)" "0 1" "$PASSES $SKIPS"
PASSES=0; FAILS=0; SKIPS=0
gated_absent "the TUI did not fail to connect" "${WORK}/never-written.txt" "connection refused" "nothing was drawn" \
	>/dev/null
expect "a capture that was never written is empty as well" "0 1" "$PASSES $SKIPS"

spin "a capture WITH bytes still yields real verdicts"
printf 'Orchicon  dev — plane healthy\n' >"${WORK}/tui.txt"
PASSES=0; FAILS=0; SKIPS=0
: >"${EVIDENCE}/results.tsv"
gated_contains "the TUI rendered" "${WORK}/tui.txt" "Orchicon" "nothing was drawn" >/dev/null
expect "gated_contains PASSes when the marker really is there" "1 0 0" "$PASSES $FAILS $SKIPS"
PASSES=0; FAILS=0; SKIPS=0
gated_contains "the TUI rendered" "${WORK}/tui.txt" "Splash" "nothing was drawn" "TUI task" >/dev/null
expect "gated_contains FAILs when the marker is really missing" "0 1" "$PASSES $FAILS"
expect "…and the FAIL files a defect naming its owner" "yes" \
	"$(grep -qF 'TUI task' "${EVIDENCE}/defects.tsv" && echo yes || echo no)"
PASSES=0; FAILS=0; SKIPS=0
gated_absent "no connection refused" "${WORK}/tui.txt" "connection refused" "nothing drawn" "TUI task" >/dev/null
expect "gated_absent PASSes on a real capture with the needle absent" "1 0" "$PASSES $FAILS"
PASSES=0; FAILS=0; SKIPS=0
printf 'Orchicon — dial tcp 127.0.0.1:18080: connect: connection refused\n' >"${WORK}/tui-bad.txt"
gated_absent "no connection refused" "${WORK}/tui-bad.txt" "connection refused" "nothing drawn" "TUI task" >/dev/null
expect "gated_absent FAILs when the needle IS there" "0 1" "$PASSES $FAILS"
expect "the TUI capture yields a render marker" "Orchicon" "$(tui_render_marker "${WORK}/tui.txt")"
expect "an empty TUI capture yields NO marker" 1 "$(rc tui_render_marker "${WORK}/install-empty.log")"
printf 'hello world\n' >"${WORK}/plain.txt"
expect "a capture with no TUI vocabulary yields no marker" 1 "$(rc tui_render_marker "${WORK}/plain.txt")"

# --- the status matchers: residency, health and drift are different claims ---
spin "host-resident is not container, and CURRENT is not merely healthy (C2)"
HOST_LINE="instance dev (host-resident): plane pid 4242 running /home/u/.local/bin/orchicon — binary current, healthy at http://localhost:18080/healthz — data dir /home/u/.local/share/orchicon-dev"
CONTAINER_LINE="instance dev (container orchicon-cnt-dev): plane pid 4242 — binary current, healthy at http://localhost:18080/healthz"
SUPERSEDED="instance dev (host-resident): plane pid 4242 running /home/u/.local/bin/orchicon (deleted) — binary superseded, healthy at http://localhost:18080/healthz"
HEALTHY_ONLY="instance dev: plane pid 4242 — identity unknown, healthy at http://localhost:18080/healthz"
expect "a host-resident line reads as host-resident" 0 "$(rc status_says_host_resident "$HOST_LINE")"
expect "a container line does NOT read as host-resident" 1 "$(rc status_says_host_resident "$CONTAINER_LINE")"
expect "a container line reads as container" 0 "$(rc status_says_container "$CONTAINER_LINE")"
expect "a host-resident line is NOT read as a container carrier" 1 "$(rc status_says_container "$HOST_LINE")"
expect "the instance is enforced (dev is not prod)" "0 1" \
	"$(rc status_names_instance "$HOST_LINE" dev) $(rc status_names_instance "$HOST_LINE" prod)"
expect "a CURRENT line reads as current" 0 "$(rc status_says_current "$HOST_LINE")"
expect "a SUPERSEDED line does not read as current" 1 "$(rc status_says_current "$SUPERSEDED")"
expect "a SUPERSEDED line reads as superseded" 0 "$(rc status_says_superseded "$SUPERSEDED")"
expect "a merely healthy line is neither current nor superseded" "1 1" \
	"$(rc status_says_current "$HEALTHY_ONLY") $(rc status_says_superseded "$HEALTHY_ONLY")"
expect "…but it IS still recognisably healthy" 0 "$(rc status_says_healthy_at "$HEALTHY_ONLY")"

spin "the container-residency REFUSAL is correct behaviour, not a failure"
expect "an install that refuses container residency is recognised" 0 \
	"$(rc residency_refusal_in 'refusing to install: ORCHICON_PLANE_RESIDENCY=container is not supported on this host')"
expect "an ordinary install transcript is not a refusal" 1 \
	"$(rc residency_refusal_in 'Orchicon is installed and running. control plane: http://localhost:18080')"

# --- AC2: Gatekeeper / SmartScreen, three outcomes and no fourth ------------
spin "Gatekeeper/SmartScreen: accepted, documented instruction, or DEAD END"
expect "an OS-accepted artifact needs no instruction" "accepted" "$(signature_verdict 0 1)"
expect "a refused artifact WITH a tested instruction is not a dead end" "instruction" "$(signature_verdict 1 0)"
expect "a refused artifact with NO instruction is a DEAD END" "dead-end" "$(signature_verdict 1 1)"

# --- AC3: the elevated-shell message must be ACTIONABLE --------------------
spin "the Windows elevated message names the cause AND the next steps"
COMPLETE='this PowerShell is running as Administrator, and Orchicon cannot install from an
  elevated shell: its Postgres host service REFUSES to run as Administrator
Next steps:
  2. Open a NORMAL PowerShell (Start → PowerShell; NOT "Run as administrator").
  Nothing was downloaded and nothing was changed.'
expect "the complete message is actionable" 0 "$(rc elevated_message_complete "$COMPLETE")"
expect "a message naming the cause but NO next steps is not actionable" 1 \
	"$(rc elevated_message_complete 'cannot install from an elevated shell: Postgres refuses to run as Administrator')"
expect "a message with next steps but no cause is not actionable either" 1 \
	"$(rc elevated_message_complete 'Next steps: open a NORMAL PowerShell. Nothing was downloaded')"
expect "a downstream initdb-style failure is NOT the actionable message" 1 \
	"$(rc elevated_message_complete 'initdb: error: cannot be run as root/Administrator')"

# --- AC5: the engine reference matcher must not be vacuous -----------------
spin "engine references are found, and correct output is not accused"
printf 'the runtime container is healthy (no engine needed)\n' >"${WORK}/clean.txt"
printf 'docker volume rm orchicon-data-dev\n' >"${WORK}/docker.txt"
printf 'Docker Desktop must be running\n' >"${WORK}/dockerd.txt"
printf 'podman ps\n' >"${WORK}/podman.txt"
printf 'wsl.exe -d Ubuntu\n' >"${WORK}/wsl.txt"
expect "a transcript that only says 'container' is NOT accused" "" \
	"$(engine_reference_in "${WORK}/clean.txt" 2>/dev/null)"
expect "a Docker reference is found" "yes" \
	"$(engine_reference_in "${WORK}/docker.txt" 2>/dev/null | grep -qi docker && echo yes || echo no)"
expect "Docker Desktop is found" "yes" \
	"$(engine_reference_in "${WORK}/dockerd.txt" 2>/dev/null | grep -qi docker && echo yes || echo no)"
expect "a Podman reference is found" "yes" \
	"$(engine_reference_in "${WORK}/podman.txt" 2>/dev/null | grep -qi podman && echo yes || echo no)"
expect "a WSL reference is found (AC3)" "yes" \
	"$(wsl_reference_in "${WORK}/wsl.txt" 2>/dev/null | grep -qi wsl && echo yes || echo no)"
expect "a clean transcript carries no WSL reference" "" "$(wsl_reference_in "${WORK}/clean.txt" 2>/dev/null)"

# --- the engine shims must really fail and really log ----------------------
spin "the no-engine PATH really is booby-trapped"
SHIMS="${WORK}/shims"
make_shims "$SHIMS" >/dev/null 2>&1
expect "make_shims planted a shim for every engine/WSL name" \
	"$(engine_shim_names | wc -l | tr -d ' ')" "$(find "$SHIMS" -maxdepth 1 -type f ! -name 'invocations.log' | wc -l | tr -d ' ')"
shim_ok=0
for n in $(engine_shim_names); do
	PATH="${SHIMS}:$PATH" "$n" >/dev/null 2>&1 && shim_ok=1
done
expect "EVERY shim exits non-zero (a shim that succeeds poisons nothing)" 0 "$shim_ok"
make_shims "$SHIMS" >/dev/null 2>&1   # fresh shims truncate the log
PATH="${SHIMS}:$PATH" docker info >/dev/null 2>&1 || true
PATH="${SHIMS}:$PATH" wsl.exe --list >/dev/null 2>&1 || true
expect "an invoked shim is LOGGED (so 'nothing was invoked' is checkable)" 2 \
	"$(shim_invocations "$SHIMS" | wc -l | tr -d ' ')"
expect "no engine answers on the poisoned PATH (this host reads as clean)" 1 \
	"$(rc engine_runs_here "$SHIMS")"
expect "engine_runs_here probes THROUGH the shims, so a real docker cannot be mistaken for one" \
	"clean" "$(engine_runs_here "$SHIMS" && echo dirty || echo clean)"

# --- AC6/C4: the footprint is MEASURED ------------------------------------
spin "footprint.sh measures bytes that match du, and never invents a number"
FIXBIN="${WORK}/fix-bin"; FIXDATA="${WORK}/fix-data"
mkdir -p "$FIXBIN" "$FIXDATA/bin" "$FIXDATA/orchicon-dev/projects" "$FIXDATA/orchicon-dev/runtime"
head -c 300000 /dev/urandom >"$FIXBIN/orchicon"
head -c 100000 /dev/urandom >"$FIXBIN/orch"
head -c 200000 /dev/urandom >"$FIXDATA/bin/postgres"
head -c 50000  /dev/urandom >"$FIXDATA/orchicon-dev/ports.json"
head -c 400000 /dev/urandom >"$FIXDATA/orchicon-dev/projects/big.bin"
head -c 600000 /dev/urandom >"$FIXDATA/orchicon-dev/runtime/image.tar"
FP_OUT="$(bash "$FOOTPRINT" --install-dir "$FIXBIN" --data-dir "$FIXDATA" --instance dev --raw 2>&1)"
fp() { printf '%s\n' "$FP_OUT" | awk -F= -v k="$1" '$1==k {print $2}'; }
DUSUM="$(( $(du -sb "$FIXBIN" | awk '{print $1}') + $(du -sb "$FIXDATA/bin" | awk '{print $1}') + $(du -sb "$FIXDATA/orchicon-dev" | awk '{print $1}') ))"
expect "the measured total equals du over the three counted components" \
	"$((DUSUM - $(du -sb "$FIXDATA/orchicon-dev/projects" | awk '{print $1}') - $(du -sb "$FIXDATA/orchicon-dev/runtime" | awk '{print $1}')))" \
	"$(fp total_bytes)"
expect "the measurement is DATED (C4: a number without its date gets misread)" "yes" \
	"$(printf '%s' "$(fp measured_at)" | grep -qE '^[0-9]{4}-[0-9]{2}-[0-9]{2}T' && echo yes || echo no)"
expect "the measured data dir follows --instance (a missing instance reads ABSENT)" "present absent" \
	"$(fp instance_data_state) $(bash "$FOOTPRINT" --install-dir "$FIXBIN" --data-dir "$FIXDATA" --instance test --raw 2>/dev/null \
		| awk -F= '$1=="instance_data_state"{print $2}')"
expect "projects/ is EXCLUDED and reported separately" "$(du -sb "$FIXDATA/orchicon-dev/projects" | awk '{print $1}')" \
	"$(fp excluded_projects_bytes)"
expect "runtime images are EXCLUDED and reported separately" "$(du -sb "$FIXDATA/orchicon-dev/runtime" | awk '{print $1}')" \
	"$(fp excluded_runtime_images_bytes)"
expect "every counted component is present" "present present present" \
	"$(fp binaries_state) $(fp host_services_state) $(fp instance_data_state)"
expect "a fixture under the claim SUPPORTS it" "yes" "$(fp claim_supports)"
expect "an absent component is reported ABSENT, not silently dropped" "absent" \
	"$(bash "$FOOTPRINT" --install-dir "${WORK}/does-not-exist" --data-dir "$FIXDATA" --instance dev --raw 2>/dev/null \
		| awk -F= '$1=="binaries_state"{print $2}')"
expect "--require-all exits 1 when a component is absent (a footprint with a hole is not a footprint)" 1 \
	"$(rc bash "$FOOTPRINT" --install-dir "${WORK}/does-not-exist" --data-dir "$FIXDATA" --instance dev --raw --require-all)"
expect "the claim verdict is a REFUTATION when the total is at or above it" "no" \
	"$(bash "$FOOTPRINT" --install-dir "$FIXBIN" --data-dir "$FIXDATA" --instance dev --claim-bytes 1000 --raw 2>/dev/null \
		| awk -F= '$1=="claim_supports"{print $2}')"
expect "the 1 GiB claim is the default yardstick, not a re-derived number" "1073741824" "$(fp claim_bytes)"
expect "--help exits 0" 0 "$(rc bash "$FOOTPRINT" --help)"
expect "an unknown flag exits 2" 2 "$(rc bash "$FOOTPRINT" --bogus)"
expect "--json emits one parseable object" 0 \
	"$(bash "$FOOTPRINT" --install-dir "$FIXBIN" --data-dir "$FIXDATA" --instance dev --json 2>/dev/null \
		| python3 -c 'import json,sys; json.load(sys.stdin)' >/dev/null 2>&1; echo $?)"
PASSES_CLAIM=0
claim_supported 1073741823 && PASSES_CLAIM=$((PASSES_CLAIM + 1))
claim_supported 1073741824 || PASSES_CLAIM=$((PASSES_CLAIM + 1))
claim_supported "" || PASSES_CLAIM=$((PASSES_CLAIM + 1))
expect "claim_supported: under is supported, AT the limit is not, and junk is not" 3 "$PASSES_CLAIM"

# --- AC6: a REFUTATION is a finding, never a failure and never a pass ------
spin "a refuted claim is recorded as a FINDING (AC6)"
PASSES=0; FAILS=0; SKIPS=0; FINDINGS=0
: >"${EVIDENCE}/results.tsv"; : >"${EVIDENCE}/defects.tsv"; : >"${EVIDENCE}/findings.tsv"
CURRENT_STEP="11"
finding "the 1073741824-byte claim is REFUTED by this measurement" "epic: claim" >/dev/null
expect "finding() counts a FINDING" 1 "$FINDINGS"
expect "finding() is NOT a failure (a refutation is a finding, AC6)" 0 "$FAILS"
expect "finding() is NOT a pass either" 0 "$PASSES"
expect "the finding is written where it cannot scroll away" 1 \
	"$(grep -c 'REFUTED' "${EVIDENCE}/findings.tsv" || true)"
expect "the finding names the owner of the claim" 1 \
	"$(grep -c 'epic: claim' "${EVIDENCE}/findings.tsv" || true)"
expect "a FINDING row is not counted as a DEFECT row" 0 \
	"$(wc -l <"${EVIDENCE}/defects.tsv" | tr -d ' ')"
check "an assertion that genuinely fails" 1 "owner: some sibling" >/dev/null
expect "check(FAIL) still counts a failure and files a defect" "1 1" \
	"$FAILS $(wc -l <"${EVIDENCE}/defects.tsv" | tr -d ' ')"

# --- C2 STRUCTURAL: the verbs that exist, each naming its instance ---------
spin "C2: the harness drives start/stop/restart/plane status, and NEVER \`serve\`"
body_of() { sed -n "/^$1() {/,/^}/p" "$HARNESS"; }
count_of() { grep -cF -- "$1" "$HARNESS" 2>/dev/null || true; }
# has_text <literal> — reads its input to the END before deciding, so a MATCH can never be turned
# into a failure by SIGPIPE. `grep -q` exits at the first match, the producer then dies of SIGPIPE,
# and `set -o pipefail` reports THAT as the pipeline's status: the moment step 0 grew past a pipe
# buffer, "the harness plants the engine shims" started FAILing on a harness that still planted them.
has_text() { grep -c -- "$1" >/dev/null 2>&1; }   # BRE, exactly as the grep -q it replaces
expect "the harness never INVOKES \`serve\` as a command (C2: those verbs do not exist)" 0 \
	"$(grep -nE '"\$ORCHICON_BIN" serve|orchicon serve' "$HARNESS" | grep -v '^[0-9]*:#' | wc -l | tr -d ' ')"
for verb in 'stop "$INSTANCE"' 'start "$INSTANCE"' 'restart "$INSTANCE"' 'plane status "$INSTANCE"'; do
	expect "the harness drives \`${verb}\`" "yes" \
		"$([ "$(count_of "$verb" "$HARNESS")" -ge 1 ] && echo yes || echo no)"
done
expect "NO lifecycle verb is invoked without an instance (C2)" 0 \
	"$(grep -nE '"\$ORCHICON_BIN" (start|stop|restart|plane status)("|$)' "$HARNESS" | wc -l | tr -d ' ')"
expect "every install invocation names the instance too (--instance)" "yes" \
	"$([ "$(count_of 'install --instance "$INSTANCE"' "$HARNESS")" -ge 2 ] && echo yes || echo no)"
expect "C3: both install runs are CAPTURED through e_run, not c_run" "yes" \
	"$([ "$(body_of step3_install | grep -c 'e_run')" -ge 1 ] && \
	   [ "$(body_of step6_reinstall | grep -c 'e_run')" -ge 1 ] && echo yes || echo no)"

# --- AC5 STRUCTURAL: the engine PATH is poisoned before anything runs -----
spin "AC5: the shims are planted in preflight, before any install"
expect "step 0 plants the engine shims" "yes" \
	"$(body_of step0_preflight | has_text 'make_shims' && echo yes || echo no)"
expect "step 0 puts the shims FIRST on PATH" "yes" \
	"$(body_of step0_preflight | has_text 'export PATH="\${SHIM_DIR}:\$PATH"' && echo yes || echo no)"
expect "step 0 verifies every shim actually fails" "yes" \
	"$(body_of step0_preflight | has_text 'PATH="\${SHIM_DIR}:\$PATH" "\$i"' && echo yes || echo no)"
expect "step 10 reads the shim invocation log and demands it EMPTY" "yes" \
	"$(body_of step10_no_engine | has_text 'shim_invocations' && echo yes || echo no)"
expect "step 10 checks the install-path OUTPUT as well as the invocations" "yes" \
	"$(body_of step10_no_engine | has_text 'engine_reference_in' && echo yes || echo no)"
expect "step 10 asserts WSL is not needed for a local-mode install (AC3)" "yes" \
	"$(body_of step10_no_engine | has_text 'wsl_reference_in' && echo yes || echo no)"

# The preflight's OWN probes must happen BEFORE the invocation log is truncated. Truncating first
# left the harness's own `docker info` in the log, so step 10's "nothing invoked an engine" FAILED on
# a spotless machine — a defect filed against the installer for something the harness did itself.
s0_body="$(body_of step0_preflight)"
s0_trunc="$(printf '%s\n' "$s0_body" | grep -n 'shim_log_path' | head -1 | cut -d: -f1)"
s0_probe="$(printf '%s\n' "$s0_body" | grep -n 'engine_runs_here' | head -1 | cut -d: -f1)"
expect "step 0 truncates the shim log AFTER its own engine probe (else step 10 blames the installer)" "after" \
	"$([ -n "$s0_trunc" ] && [ -n "$s0_probe" ] && [ "$s0_trunc" -gt "$s0_probe" ] && echo after || echo before)"

# --- the TUI step can never pass on an empty capture ----------------------
spin "AC4: the TUI step is gated, not a raw grep"
step9="$(body_of step9_tui)"
expect "step 9 gates its capture assertions" "yes" \
	"$([ "$(printf '%s\n' "$step9" | grep -c 'gated_')" -ge 3 ] && echo yes || echo no)"
expect "step 9 never trusts an empty pty capture" "yes" \
	"$(printf '%s\n' "$step9" | has_text 'capture_is_empty "\$cap"' && echo yes || echo no)"
expect "step 9 TAKES a screenshot artifact for the evidence (AC9)" "yes" \
	"$(printf '%s\n' "$step9" | has_text '9-tui-screenshot' && echo yes || echo no)"

# --- C1 STRUCTURAL: the artifact identity is checked, not assumed ---------
spin "C1: the version gate is wired into the run"
expect "step 0 refuses an unnamed version" "yes" \
	"$(body_of step0_preflight | has_text 'version_is_explicit' && echo yes || echo no)"
expect "step 2 runs the BINARY and compares it to the tag" "yes" \
	"$(body_of step2_extract_and_identify | has_text '\-\-version' && echo yes || echo no)"
expect "the plan names every one of the 12 steps (a --dry-run that hides a step lies)" 12 \
	"$(grep -cE '^step [0-9]+ ' "$HARNESS")"
expect "the C1 merge-date gate is a single named constant" "yes" \
	"$(grep -q 'HOST_NATIVE_MERGE_UTC="\${ORCHICON_E2E_HOST_NATIVE_MERGE:-' "$HARNESS" && echo yes || echo no)"
expect "an unknown flag is a usage error (exit 2), never a silent run" 2 \
	"$(bash "$HARNESS" --bogus-flag >/dev/null 2>&1; echo $?)"

# --- AC10 STRUCTURAL: nothing is patched here, every defect names an owner -
spin "AC10: the harness files defects, it does not fix product code"
expect "every check(FAIL) site the harness owns passes an owner or is explicitly ownerless-invalid" "yes" \
	"$(grep -q 'OWNER_INSTALLER' "$HARNESS" && grep -q 'OWNER_PLANE' "$HARNESS" && \
	   grep -q 'OWNER_RELEASE' "$HARNESS" && grep -q 'OWNER_TUI' "$HARNESS" && echo yes || echo no)"
expect "defects are filed to defects.tsv" "yes" \
	"$(grep -q 'defects.tsv' "$HARNESS" && echo yes || echo no)"
expect "findings have their own file (a refutation is not a defect)" "yes" \
	"$(grep -q 'findings.tsv' "$HARNESS" && echo yes || echo no)"
expect "the harness patches no product file (it writes only under EVIDENCE/E2E_ROOT)" 0 \
	"$(grep -nE '^[^#]*>[^&]*\b(scripts/install\.sh|scripts/install\.ps1|\.github/workflows)' "$HARNESS" | wc -l | tr -d ' ')"

# --- the runbook carries the EXACT instructions step 8 relies on ----------
spin "AC2/AC9: the runbook carries the exact instructions the code checks for"
expect "the macOS quarantine instruction is in the runbook (step 8 asserts it)" "yes" \
	"$(grep -q 'xattr -d com.apple.quarantine' "$RUNBOOK" && echo yes || echo no)"
expect "the Windows zip-mark instruction is in the runbook (step 8 asserts it)" "yes" \
	"$(grep -q 'Unblock-File' "$RUNBOOK" && echo yes || echo no)"
expect "the runbook names the verbs that exist (plane status)" "yes" \
	"$(grep -q 'plane status' "$RUNBOOK" && echo yes || echo no)"
expect "the runbook documents the instance-per-command rule (C2)" "yes" \
	"$(grep -q 'every command in the evidence names the' "$RUNBOOK" && echo yes || echo no)"
expect "the runbook documents the captured-run rule (C3)" "yes" \
	"$(grep -q 'captured stdout asks NOTHING' "$RUNBOOK" && echo yes || echo no)"
expect "the runbook documents that 'latest' is not a valid target (C1)" "yes" \
	"$(grep -q 'do NOT test .latest' "$RUNBOOK" && echo yes || echo no)"
expect "the runbook warns the footprint is pre-Grafana-removal (C4)" "yes" \
	"$(grep -qi 'grafana' "$RUNBOOK" && echo yes || echo no)"
expect "the runbook says a REFUTED claim is a finding, not a failure (AC6)" "yes" \
	"$(grep -q 'refuted' "$RUNBOOK" && echo yes || echo no)"
expect "the runbook files DEF-1 (the Windows zip is unchecksummed)" "yes" \
	"$(grep -q 'DEF-1' "$RUNBOOK" && echo yes || echo no)"
expect "the runbook files DEF-2 (the installer still names Docker)" "yes" \
	"$(grep -q 'DEF-2' "$RUNBOOK" && echo yes || echo no)"

# --- the gate must be DISCOVERABLE ---------------------------------------
spin "the gate is discoverable and runs in CI"
HELP="$(cd "$REPO_ROOT" && make help 2>/dev/null || true)"
expect "make help lists published-archive-e2e-assertions-test" "yes" \
	"$(printf '%s\n' "$HELP" | has_text 'published-archive-e2e-assertions-test' && echo yes || echo no)"
expect "make help lists the footprint measurer" "yes" \
	"$(printf '%s\n' "$HELP" | has_text 'published-archive-e2e-footprint' && echo yes || echo no)"
expect "make ci-go runs this gate" "yes" \
	"$(grep -qE '^ci-go:.*published-archive-e2e-assertions-test' "${REPO_ROOT}/Makefile" && echo yes || echo no)"
expect "the windows zip is still published (the harness assumes it exists)" "yes" \
	"$(grep -qE 'name: .*\.zip|ASSET.*\.zip' "${REPO_ROOT}/.github/workflows/release.yml" && echo yes || echo no)"

echo
if [ "$FAILED" -gt 0 ]; then
	echo "RESULT: ${FAILED} assertion(s) FAILED, ${PASSED} passed — the harness can claim a proof it does not have."
	exit 1
fi
echo "RESULT: ${PASSED} assertions passed — every verdict says PASS only when the evidence says it should."
exit 0
