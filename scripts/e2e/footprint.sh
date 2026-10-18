#!/usr/bin/env bash
# ============================================================================
# The INSTALLED FOOTPRINT of an Orchicon install — measured, not derived.
#
# WHY THIS IS NOT `scripts/disk-report.sh`. That tool answers a different
# question: how much of a BTRFS filesystem is held by snapshots (data_used minus
# du(live)). It has no answer on ext4, APFS or NTFS, where it reports "no
# snapshot attribution" — so using it to check the epic's "under 1 GB" claim
# would measure nothing on two of the three platforms this procedure runs on.
# The claim is about BYTES ON DISK for a default install, so this measures bytes
# on disk with `du` (see measure_bytes for the per-platform fallbacks).
#
# WHAT IS COUNTED (the epic's claim is "under 1 GB excluding projects and runtime
# images", so exactly those two are excluded):
#   binaries       the install dir: `orchicon`, `orch` (and nothing else is assumed)
#   host_services  <data>/bin — the Postgres/NATS/etc. binaries the host-native
#                  plane runs directly (this is what replaced the container images)
#   instance_data  <data>/orchicon-<instance> — the plane's own state
# EXCLUDED, and reported separately so the number cannot be quietly widened:
#   projects/      user work, and anything a checkout of theirs puts there
#   runtime/       runtime IMAGES (OCI layers pulled by the runtime daemon)
#
# WHAT IT REFUSES TO DO: invent a number. A component that does not exist is
# reported `absent` and counted as zero — never skipped silently, because a
# missing install dir reading as "0 bytes" is how a footprint claim passes on a
# machine where nothing was installed. `--require-all` turns any absent component
# into a non-zero exit.
#
# USAGE
#   scripts/e2e/footprint.sh [options]
#     --install-dir DIR    where orchicon/orch live  (default $ORCHICON_INSTALL_DIR, else ~/.local/bin)
#     --data-dir DIR       the host data dir         (default $ORCHICON_DATA_DIR, else ~/.local/share/orchicon)
#     --instance NAME      instance whose data dir is measured (default dev)
#     --raw                key=value lines only (this is what the E2E harness reads)
#     --json               one JSON object
#     --require-all        exit 1 if any component is absent
#     --claim-bytes N      the claim to test (default 1073741824 = 1 GiB)
#     -h | --help
#
# EXIT: 0 measured | 1 a required component is absent | 2 usage error
# ============================================================================
set -uo pipefail

INSTALL_DIR="${ORCHICON_INSTALL_DIR:-${HOME}/.local/bin}"
DATA_DIR="${ORCHICON_DATA_DIR:-${XDG_DATA_HOME:-${HOME}/.local/share}/orchicon}"
INSTANCE="dev"
RAW=0
JSON=0
REQUIRE_ALL=0
CLAIM_BYTES=1073741824          # 1 GiB — the epic's claim, never re-derived from the measurement
MEASURED_AT=""
COMPONENT_NAMES=""
COMPONENT_BYTES=""
COMPONENT_STATE=""

usage() {
	cat <<'USAGE'
usage: footprint.sh [--install-dir DIR] [--data-dir DIR] [--instance NAME]
                    [--raw] [--json] [--require-all] [--claim-bytes N]

Measures the INSTALLED footprint of an Orchicon install (binaries + host service
binaries + one instance's data), excluding projects/ and runtime/ images, and
reports whether it supports the epic's under-1 GB claim.
USAGE
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--install-dir) INSTALL_DIR="$2"; shift 2 ;;
		--data-dir) DATA_DIR="$2"; shift 2 ;;
		--instance) INSTANCE="$2"; shift 2 ;;
		--claim-bytes) CLAIM_BYTES="$2"; shift 2 ;;
		--raw) RAW=1; shift ;;
		--json) JSON=1; shift ;;
		--require-all) REQUIRE_ALL=1; shift ;;
		-h|--help) usage; exit 0 ;;
		*) printf 'footprint.sh: unknown argument %s\n' "$1" >&2; usage >&2; exit 2 ;;
	esac
done

case "$CLAIM_BYTES" in ''|*[!0-9]*) printf 'footprint.sh: --claim-bytes must be a number\n' >&2; exit 2 ;; esac

# --- measuring ---------------------------------------------------------------
# measure_bytes <dir> — apparent+allocated bytes of a tree, or nothing when it is absent.
# `du -sb` is GNU-only (apparent bytes, 1-byte blocks). macOS/BSD du has no -b, so it is asked for
# KiB (-k) and scaled; on Windows the measurement goes through PowerShell, which is the only thing
# guaranteed present on a clean Windows box (no CoreUtils). Each fallback is a DIFFERENT unit, so
# every branch is scaled to bytes before it leaves this function — a mixed-unit sum is how a
# footprint report lies by a factor of 1024.
measure_bytes() {
	local dir="$1" n
	[ -d "$dir" ] || return 1
	if du -sb -- "$dir" >/dev/null 2>&1; then
		n="$(du -sb -- "$dir" 2>/dev/null | awk 'NR==1 { print $1 + 0 }')"
		printf '%s' "$n"
		return 0
	fi
	if du -sk -- "$dir" >/dev/null 2>&1; then
		n="$(du -sk -- "$dir" 2>/dev/null | awk 'NR==1 { print $1 + 0 }')"
		printf '%s' "$((n * 1024))"
		return 0
	fi
	if command -v powershell >/dev/null 2>&1 || command -v pwsh >/dev/null 2>&1; then
		local ps; ps="$(command -v powershell || command -v pwsh)"
		n="$("$ps" -NoProfile -NonInteractive -Command \
			"(Get-ChildItem -LiteralPath '${dir//\'/\'\'}' -Recurse -File -Force -ErrorAction SilentlyContinue | Measure-Object -Property Length -Sum).Sum" \
			2>/dev/null | tr -dc '0-9')"
		printf '%s' "${n:-0}"
		return 0
	fi
	return 1
}

# human_bytes <bytes> — the same rounding a human reads in a report.
human_bytes() {
	awk -v b="$1" 'BEGIN {
		split("B KiB MiB GiB TiB", u, " "); i = 1; v = b + 0
		while (v >= 1024 && i < 5) { v /= 1024; i++ }
		printf (i == 1 ? "%d %s" : "%.1f %s"), v, u[i]
	}'
}

# add_component <name> <path> <absent-label> — measure one component, remembering absent ones.
# An ABSENT component is recorded as absent rather than dropped: a footprint that sums to 0 B
# because nothing is installed must not read as a small footprint.
add_component() {
	local name="$1" path="$2" bytes state
	if bytes="$(measure_bytes "$path")"; then
		state="present"
	else
		bytes=0; state="absent"
	fi
	COMPONENT_NAMES="${COMPONENT_NAMES}${COMPONENT_NAMES:+ }${name}"
	COMPONENT_BYTES="${COMPONENT_BYTES}${COMPONENT_BYTES:+ }${bytes}"
	COMPONENT_STATE="${COMPONENT_STATE}${COMPONENT_STATE:+ }${state}"
}

INSTANCE_DATA="${DATA_DIR%/}/orchicon-${INSTANCE}"

# The exclusions are measured FIRST, from the instance data dir, so their bytes exist as evidence
# next to the total — and so the total can be stated as `measured_at_root - excluded`.
add_component binaries "$INSTALL_DIR"
add_component host_services "${DATA_DIR%/}/bin"
add_component instance_data "$INSTANCE_DATA"

PROJECTS_BYTES="$(measure_bytes "${INSTANCE_DATA%/}/projects" 2>/dev/null || printf '0')"
RUNTIME_BYTES="$(measure_bytes "${INSTANCE_DATA%/}/runtime" 2>/dev/null || printf '0')"

TOTAL=0
ABSENT=""
i=0
for name in $COMPONENT_NAMES; do
	i=$((i + 1))
	bytes="$(printf '%s\n' "$COMPONENT_BYTES" | tr ' ' '\n' | sed -n "${i}p")"
	state="$(printf '%s\n' "$COMPONENT_STATE" | tr ' ' '\n' | sed -n "${i}p")"
	TOTAL=$((TOTAL + bytes))
	[ "$state" = "absent" ] && ABSENT="${ABSENT}${ABSENT:+, }${name}"
done
# The claim is "under 1 GB EXCLUDING projects and runtime images", so those two come OUT of the
# total. instance_data is reported GROSS above so the raw figure stays visible; subtracting here is
# what makes the number comparable to the claim at all. It is clamped at zero for the same reason
# disk-report clamps: a negative footprint is a bug, not a small install.
GROSS_TOTAL="$TOTAL"
TOTAL=$((TOTAL - PROJECTS_BYTES - RUNTIME_BYTES))
[ "$TOTAL" -lt 0 ] && TOTAL=0

SUPPORTS="no"
[ "$TOTAL" -lt "$CLAIM_BYTES" ] && SUPPORTS="yes"
MEASURED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

if [ "$JSON" = "1" ]; then
	comps_json=""
	i=0
	for name in $COMPONENT_NAMES; do
		i=$((i + 1))
		[ -n "$comps_json" ] && comps_json="${comps_json},"
		comps_json="${comps_json}\"${name}\":$(printf '%s\n' "$COMPONENT_BYTES" | tr ' ' '\n' | sed -n "${i}p")"
	done
	printf '{"measured_at":"%s","platform":"%s","instance":"%s","install_dir":"%s","data_dir":"%s","components":{%s},"excluded":{"projects":%s,"runtime_images":%s},"gross_total_bytes":%s,"total_bytes":%s,"total_human":"%s","claim_bytes":%s,"claim_supports":%s}\n' \
		"$MEASURED_AT" "$(uname -s | tr '[:upper:]' '[:lower:]')" "$INSTANCE" "$INSTALL_DIR" "$DATA_DIR" \
		"$comps_json" "$PROJECTS_BYTES" "$RUNTIME_BYTES" "$GROSS_TOTAL" "$TOTAL" "$(human_bytes "$TOTAL")" \
		"$CLAIM_BYTES" "$([ "$SUPPORTS" = yes ] && echo true || echo false)"
	[ "$REQUIRE_ALL" = "1" ] && [ -n "$ABSENT" ] && exit 1
	exit 0
fi

if [ "$RAW" = "1" ]; then
	cat <<RAW
measured_at=${MEASURED_AT}
platform=$(uname -s | tr '[:upper:]' '[:lower:]')
instance=${INSTANCE}
install_dir=${INSTALL_DIR}
data_dir=${DATA_DIR}
excluded_projects_bytes=${PROJECTS_BYTES}
excluded_runtime_images_bytes=${RUNTIME_BYTES}
gross_total_bytes=${GROSS_TOTAL}
total_bytes=${TOTAL}
total_human=$(human_bytes "$TOTAL")
claim_bytes=${CLAIM_BYTES}
claim_supports=${SUPPORTS}
absent_components=${ABSENT}
RAW
	i=0
	for name in $COMPONENT_NAMES; do
		i=$((i + 1))
		printf '%s_bytes=%s\n' "$name" "$(printf '%s\n' "$COMPONENT_BYTES" | tr ' ' '\n' | sed -n "${i}p")"
	done
	i=0
	for name in $COMPONENT_NAMES; do
		i=$((i + 1))
		printf '%s_state=%s\n' "$name" "$(printf '%s\n' "$COMPONENT_STATE" | tr ' ' '\n' | sed -n "${i}p")"
	done
	[ "$REQUIRE_ALL" = "1" ] && [ -n "$ABSENT" ] && exit 1
	exit 0
fi

echo "Orchicon installed footprint — measured ${MEASURED_AT}"
echo "  instance ${INSTANCE}   install dir ${INSTALL_DIR}"
echo
printf '  %-16s %12s  %s\n' COMPONENT BYTES PATH
i=0
for name in $COMPONENT_NAMES; do
	i=$((i + 1))
	bytes="$(printf '%s\n' "$COMPONENT_BYTES" | tr ' ' '\n' | sed -n "${i}p")"
	state="$(printf '%s\n' "$COMPONENT_STATE" | tr ' ' '\n' | sed -n "${i}p")"
	case "$name" in
		binaries) path="$INSTALL_DIR" ;;
		host_services) path="${DATA_DIR%/}/bin" ;;
		instance_data) path="$INSTANCE_DATA" ;;
	esac
	if [ "$state" = "absent" ]; then
		printf '  %-16s %12s  %s (ABSENT — counted as 0, not skipped)\n' "$name" "-" "$path"
	else
		printf '  %-16s %12s  %s (%s)\n' "$name" "$(human_bytes "$bytes")" "$path" "$bytes bytes"
	fi
done
echo
echo "  EXCLUDED from the claim (reported, not counted):"
echo "    projects/       $(human_bytes "$PROJECTS_BYTES")  (${PROJECTS_BYTES} bytes)"
echo "    runtime images  $(human_bytes "$RUNTIME_BYTES")  (${RUNTIME_BYTES} bytes)"
echo
printf '  gross total    %12s  (%s bytes)\n' "$(human_bytes "$GROSS_TOTAL")" "$GROSS_TOTAL"
printf '  TOTAL (excl.)  %12s  (%s bytes)\n' "$(human_bytes "$TOTAL")" "$TOTAL"
printf '  claim          %12s  -> %s\n' "$(human_bytes "$CLAIM_BYTES")" \
	"$([ "$SUPPORTS" = yes ] && echo 'SUPPORTED' || echo 'REFUTED — this is a finding, not a failure to hide')"
[ -n "$ABSENT" ] && echo
[ -n "$ABSENT" ] && echo "  NOTE: absent component(s): ${ABSENT}. A footprint with a missing component is not a footprint."

[ "$REQUIRE_ALL" = "1" ] && [ -n "$ABSENT" ] && exit 1
exit 0
