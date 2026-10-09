#!/usr/bin/env bash
# ============================================================================
# disk-report/run.sh — assert the arithmetic and the honesty of disk-report.sh.
#
# WHY THIS EXISTS. The report's entire value is ONE SUBTRACTION:
#
#     pinned ~= btrfs "Data used"  -  du(allocated) of live files
#
# Every way it can be wrong is silent, and every one of them argues the WRONG
# WAY about a disk that is filling up — which is the one moment the number is
# being trusted to decide something:
#
#   1. Metadata folded in. `btrfs filesystem usage` prints "Used" (data +
#      metadata) a few lines above the "Data," line. Reaching for the wrong one
#      inflates the pinned figure by the whole metadata size — on this machine
#      ~25 GiB of DUP'd metadata. The subtraction must use DATA.
#   2. Profiles collapsed. Data can appear on more than one profile line
#      (Data,single + Data,RAID1). Counting only the first understates what is
#      referenced, so real pinned extents read as zero.
#   3. Coverage overstated. With --paths the path set no longer spans the
#      filesystem, so live data is UNDERcounted — pinned becomes an UPPER bound.
#      Presenting that as a measurement is how a bound gets mistaken for a fact.
#   4. Unreadable paths swallowed. A `du` that fails on a subdirectory still
#      prints a total. Keeping that total silently reports live data that does
#      not exist — inflating pinned exactly when permissions are the cause.
#   5. Exit codes lost. A missing target or a bad flag must not exit 0.
#
# The INPUTS are faked — `findmnt`, `df` and `btrfs` shimmed on PATH — so the
# assertions are exact and no real filesystem accounting is read. `du` is the
# REAL du over a fixture tree, and the expected live total is taken from du
# itself, so block rounding (and btrfs compression) can never make an assertion
# pass or fail spuriously.
#
#   scripts/tests/disk-report/run.sh [disk-report.sh]
#
# Exits 0 only when every assertion passes.
# ============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
REPORT_SH="${REPO_ROOT}/scripts/disk-report.sh"
[ "$#" -gt 0 ] && REPORT_SH="$1"

if [ ! -f "$REPORT_SH" ]; then
	echo "run.sh: disk-report.sh not found at ${REPORT_SH}" >&2
	exit 2
fi

PASSED=0
FAILED=0

check() { # <label> <expected> <actual>
	if [ "$2" = "$3" ]; then
		printf '  \033[32mPASS\033[0m  %-52s %s\n' "$1" "$3"
		PASSED=$((PASSED + 1))
	else
		printf '  \033[31mFAIL\033[0m  %-52s want [%s] got [%s]\n' "$1" "$2" "$3"
		# Show what the script said on stderr: a harness that swallows the diagnostic is
		# how a failing case reads as a mystery instead of a reported cause.
		[ -n "${ERR:-}" ] && printf '        stderr: %s\n' "$ERR"
		FAILED=$((FAILED + 1))
	fi
}

check_contains() { # <label> <haystack> <needle>
	if printf '%s' "$2" | grep -qF -- "$3"; then
		printf '  \033[32mPASS\033[0m  %-52s contains "%s"\n' "$1" "$3"
		PASSED=$((PASSED + 1))
	else
		printf '  \033[31mFAIL\033[0m  %-52s missing "%s"\n' "$1" "$3"
		FAILED=$((FAILED + 1))
	fi
}

kv() { # <raw line> <key>
	printf '%s\n' "$1" | tr ' ' '\n' | awk -F= -v k="$2" '$1 == k { print $2 }'
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FAKEBIN="$TMP/bin"
mkdir -p "$FAKEBIN"

# --- fixture: real files, real du -------------------------------------------
FIX="$TMP/fixture"
mkdir -p "$FIX/sub"
for i in 1 2 3 4; do
	head -c 1048576 /dev/urandom > "$FIX/f$i.bin"
done
head -c 1048576 /dev/urandom > "$FIX/sub/deep.bin"

# --- fake findmnt / df / btrfs ----------------------------------------------
cat > "$FAKEBIN/findmnt" <<'FAKE'
#!/usr/bin/env bash
args="$*"
case "$args" in
  *FSTYPE*) echo "${FAKE_FSTYPE:-btrfs}"; exit 0 ;;
  *SOURCE*) echo "${FAKE_SOURCE:-/dev/fake0[/@fake]}"; exit 0 ;;
esac
# the `-no TARGET --target <path>` call: report the mountpoint
echo "${FAKE_MOUNT:-/fake}"
FAKE

cat > "$FAKEBIN/df" <<'FAKE'
#!/usr/bin/env bash
args="$*"
case "$args" in
  *--output=source,target*)
    echo "Filesystem Mounted-on"
    echo "${FAKE_DEV:-/dev/fake0} ${FAKE_MOUNT:-/fake}"
    [ -n "${FAKE_NESTED_MOUNT:-}" ] && echo "${FAKE_DEV:-/dev/fake0} ${FAKE_NESTED_MOUNT}"
    ;;
  *--output=source*) echo "Source"; echo "${FAKE_DEV:-/dev/fake0}" ;;
  *--output=size,used,avail*) echo "1B-blocks Used Avail"; echo "${FAKE_DF:-10485760 8388608 2097152}" ;;
  *) echo "${FAKE_DF:-10485760 8388608 2097152} /fake" ;;
esac
# Explicit: a fake whose exit status depends on its LAST statement would trip `set -e`/
# pipefail inside the script under test, which is a property of the fake, not of the code.
exit 0
FAKE

cat > "$FAKEBIN/btrfs" <<'FAKE'
#!/usr/bin/env bash
# only `filesystem usage -b <path>` is exercised by disk-report.sh
cat <<USAGE
Overall:
    Device size:             ${FAKE_DEVICE:-6815744000}
    Device allocated:        ${FAKE_DEVICE:-6815744000}
    Used:                    ${FAKE_OVERALL:-6553600}
Data,single: Size:${FAKE_DATA_SIZE:-10485760}, Used:${FAKE_DATA_USED:-6291456} (60.00%)
${FAKE_DATA_EXTRA:-}
Metadata,DUP: Size:${FAKE_META_SIZE:-1048576}, Used:${FAKE_META_USED:-524288} (50.00%)
USAGE
FAKE

chmod +x "$FAKEBIN"/*

# report <args...> -> stdout in $OUT, stderr in $ERR, status in $RC
run() {
	OUT="$(PATH="$FAKEBIN:$PATH" bash "$REPORT_SH" "$@" 2>"$TMP/err")"
	RC=$?
	ERR="$(cat "$TMP/err")"
}

# du's own answer for the fixture: the yardstick every live assertion uses
LIVE_EXP="$(du -sx --block-size=1 "$FIX" | awk 'NR==1 { print $1 + 0 }')"

echo "disk-report.sh under test: ${REPORT_SH}"
echo "fixture live total (by du): ${LIVE_EXP} bytes"
echo

# --- 1. the subtraction, complete coverage ----------------------------------
echo "--- complete coverage: pinned = data - live ---"
FAKE_FSTYPE=btrfs FAKE_MOUNT="$FIX" FAKE_DATA_USED=6291456 FAKE_META_USED=524288
export FAKE_FSTYPE FAKE_MOUNT FAKE_DATA_USED FAKE_META_USED
run --raw "$FIX"
check "exit 0" "0" "$RC"
check "fstype reported" "btrfs" "$(kv "$OUT" fstype)"
check "live matches du's own total" "$LIVE_EXP" "$(kv "$OUT" live)"
check "pinned = data_used - live" "$((6291456 - LIVE_EXP))" "$(kv "$OUT" pinned)"
check "coverage is complete" "complete" "$(kv "$OUT" coverage)"

# --- 2. metadata is NOT folded into the subtraction -------------------------
echo "--- metadata excluded from the subtraction ---"
FAKE_DATA_USED=10000000 FAKE_META_USED=99999999
export FAKE_DATA_USED FAKE_META_USED
run --raw "$FIX"
check "pinned uses Data, not overall Used" "$((10000000 - LIVE_EXP))" "$(kv "$OUT" pinned)"
check "metadata reported separately" "99999999" "$(kv "$OUT" metadata_used)"

# --- 3. multiple Data profile lines are summed ------------------------------
# The fake data must EXCEED the fixture's live bytes, or the clamp at zero fires and the
# assertion silently tests the clamp instead of the sum. (It did, first time round.)
echo "--- multiple Data profile lines sum ---"
FAKE_DATA_USED=7000000 FAKE_DATA_EXTRA="Data,RAID1: Size:2097152, Used:2000000 (95.00%)"
export FAKE_DATA_USED FAKE_DATA_EXTRA
run --raw "$FIX"
check "data_used sums every profile line" "9000000" "$(kv "$OUT" data_used)"
check "pinned follows the summed data" "$((9000000 - LIVE_EXP))" "$(kv "$OUT" pinned)"
FAKE_DATA_EXTRA=""
FAKE_DATA_USED=6291456
export FAKE_DATA_EXTRA FAKE_DATA_USED

# --- 3b. EVERY mount of the device is walked ---------------------------------
# On btrfs each subvolume has its OWN st_dev, so `du -x` refuses to descend from one into a
# sibling — which is what makes summing per-mount the right arithmetic rather than a double
# count. A fixture living on a single filesystem cannot reproduce that kernel behaviour, so the
# assertion here is the part that IS ours: that the report sums every mount df names instead of
# picking one and reporting it as the whole filesystem. The subvolume half was verified against
# the real machine — one mount measured 42 GiB where the seven mounts measured 172 GiB.
echo "--- every mount of the device is walked ---"
FAKE_NESTED_MOUNT="$TMP/second"
mkdir -p "$FAKE_NESTED_MOUNT"
head -c 1048576 /dev/urandom > "$FAKE_NESTED_MOUNT/n.bin"
export FAKE_NESTED_MOUNT
SECOND_EXP="$(du -sx --block-size=1 "$FAKE_NESTED_MOUNT" | awk 'NR==1 { print $1 + 0 }')"
run --raw "$FIX"
check "live sums every mount df names" "$((LIVE_EXP + SECOND_EXP))" "$(kv "$OUT" live)"
check "coverage still complete" "complete" "$(kv "$OUT" coverage)"
unset FAKE_NESTED_MOUNT

# --- 4. partial coverage must be marked an upper bound ----------------------
echo "--- partial coverage is labelled, not presented as fact ---"
run --raw --paths "$FIX/sub" "$FIX"
SUB_EXP="$(du -s --block-size=1 "$FIX/sub" | awk 'NR==1 { print $1 + 0 }')"
check "coverage is partial" "partial" "$(kv "$OUT" coverage)"
check "live counts only the listed path" "$SUB_EXP" "$(kv "$OUT" live)"
run --paths "$FIX/sub" "$FIX"
check "human report warns of the upper bound" "0" "$RC"
check_contains "says UPPER BOUND" "$OUT" "UPPER BOUND"

# --- 5. an unreadable path is reported, and does not become live data --------
echo "--- unreadable path: warned about, exits 0 ---"
if [ "$(id -u)" = "0" ]; then
	echo "  \033[33mSKIP\033[0m  running as root: mode-000 dirs stay readable"
else
	NOPERM="$TMP/noperm"
	mkdir -p "$NOPERM"
	head -c 1048576 /dev/urandom > "$NOPERM/x.bin"
	chmod 000 "$NOPERM"
	run --paths "$NOPERM" "$FIX"
	check "exit 0 despite the unreadable path" "0" "$RC"
	check_contains "names the readable warning" "$OUT" "Not everything was readable"
	check_contains "names the unreadable path" "$OUT" "$NOPERM"
	chmod 755 "$NOPERM"
fi

# --- 6. non-btrfs: no attribution offered -----------------------------------
echo "--- non-btrfs filesystem ---"
FAKE_FSTYPE=ext4
export FAKE_FSTYPE
run "$FIX"
check "exit 0" "0" "$RC"
check_contains "states there is no snapshot attribution" "$OUT" "No snapshot attribution"
run --raw "$FIX"
check "raw still reports live data" "$LIVE_EXP" "$(kv "$OUT" live)"
check "raw leaves pinned empty" "" "$(kv "$OUT" pinned)"
FAKE_FSTYPE=btrfs
export FAKE_FSTYPE

# --- 7. live exceeding data clamps to zero, and says so ---------------------
echo "--- no snapshot overhead ---"
FAKE_DATA_USED=1
export FAKE_DATA_USED
run --raw "$FIX"
check "pinned clamps at 0, never negative" "0" "$(kv "$OUT" pinned)"
run "$FIX"
check_contains "says so plainly" "$OUT" "no snapshot overhead detected"
FAKE_DATA_USED=6291456
export FAKE_DATA_USED

# --- 8. btrfs present but usage unreadable -> graceful ----------------------
echo "--- btrfs usage unavailable ---"
mv "$FAKEBIN/btrfs" "$FAKEBIN/btrfs.off"
run "$FIX"
check "exit 0 with no data line" "0" "$RC"
check_contains "falls back to no attribution" "$OUT" "No snapshot attribution"
mv "$FAKEBIN/btrfs.off" "$FAKEBIN/btrfs"

# --- 9. exits: missing target, bad flag, help -------------------------------
echo "--- exit codes ---"
run "$TMP/does-not-exist"
check "missing target exits 1" "1" "$RC"
run --bogus "$FIX"
check "unknown flag exits 2" "2" "$RC"
run --help
check "--help exits 0" "0" "$RC"
check_contains "--help prints usage" "$OUT" "usage: disk-report.sh"

echo
printf '  disk-report harness: %d passed, %d failed\n' "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
