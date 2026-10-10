#!/usr/bin/env bash
# ============================================================================
# disk-report.sh — how much of a filesystem is held by SNAPSHOTS, not by files.
#
# THE QUESTION THIS ANSWERS. On btrfs with snapper, `df` and `du` disagree by
# design, and NOTHING tells you why. A measured example from this repo:
#
#     deleted                                        65.6 GB
#     free space recovered                            7.0 GB
#     ------------------------------------------------------
#     silently withheld from every live file         58.6 GB
#
# The 58.6 GB was not leaked, not in a trash can, and not open by a process. It
# was referenced by a btrfs snapshot. Copy-on-write keeps an extent alive until
# the LAST reference goes, snapshots included — so `df` (which reports free
# space) shows you the 7 GB and stays silent about the 58. On a machine that
# snapshots a busy home directory hourly, this is how a disk reaches 93% full
# while `du` insists almost nothing is there.
#
# METHOD — and it is a subtraction, so it is worth being exact about:
#
#     pinned  ~=  btrfs "Data used"  -  du(allocated) of every live file
#
# The live side walks EVERY mount of the device with `du -x`, and both halves of
# that matter. On btrfs each subvolume gets its own anonymous st_dev, so -x stops
# at the subvolume boundary: no single mount covers the whole filesystem, and
# -x is what stops a parent mount from counting a child subvolume's bytes again.
#
# `btrfs filesystem usage` counts every data extent ANY subvolume references,
# live or snapshot. `du` counts only what live files reference. The difference
# is data that exists solely because a snapshot holds it. Three honest limits:
#
#   * Filesystem METADATA (inodes, b-tree nodes) is invisible to `du`, so it is
#     excluded from BOTH sides and reported separately. Folding it in would
#     overstate the pinned figure by the whole metadata size.
#   * COVERAGE. If the path set does not span the filesystem, `du` undercounts
#     live data and the pinned figure is an UPPER bound. The report says which
#     case it is rather than presenting a bound as a measurement.
#   * DELETED-BUT-OPEN files also pin extents, and this cannot see them. If the
#     numbers look wrong, check the space against `lsof +L1` before trusting the
#     subtraction.
#
# For exact per-snapshot attribution you need root:
#     sudo btrfs filesystem du -s --raw <snapshot-dir>/*/snapshot | sort -k2 -n
#
# THIS IS A HOST DIAGNOSTIC, NOT A GATE. It describes the machine it runs on, so
# it is deliberately never called from CI. Its arithmetic is pinned separately by
# scripts/tests/disk-report/run.sh, which fakes the inputs and asserts the math.
#
#   scripts/disk-report.sh [--paths LIST] [--raw] [TARGET]
# ============================================================================
set -euo pipefail

PATHS_ARG=""
RAW=0
TARGET=""

usage() {
	cat <<'EOF'
usage: disk-report.sh [--paths LIST] [--raw] [TARGET]

  TARGET        filesystem to report on. Default: the mount holding $PWD, so
                run from this repo it reports /home.
  --paths LIST  comma-separated paths to measure as "live", instead of the whole
                filesystem. Adds a per-path breakdown, but the path set no longer
                spans the filesystem, so the pinned figure becomes an UPPER bound
                and the report says so.
  --raw         print one machine-readable key=value line and nothing else.

For a plain size breakdown of live files, `du -h --max-depth=1 <mountpoint>`.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
		-h|--help) usage; exit 0 ;;
		--raw) RAW=1; shift ;;
		--paths) PATHS_ARG="${2:-}"; shift 2 ;;
		--paths=*) PATHS_ARG="${1#--paths=}"; shift ;;
		-*) echo "disk-report: unknown option: $1" >&2; usage >&2; exit 2 ;;
		*) TARGET="$1"; shift ;;
	esac
done

human() {
	awk -v b="${1:-0}" 'BEGIN {
		split("B KiB MiB GiB TiB", u, " "); i = 1
		while (b >= 1024 && i < 5) { b /= 1024; i++ }
		printf (i == 1 ? "%.0f %s" : "%.1f %s"), b, u[i]
	}'
}
pct() {
	awk -v n="${1:-0}" -v d="${2:-0}" 'BEGIN { printf (d > 0 ? "%.0f" : "0"), (d > 0 ? 100 * n / d : 0) }'
}

# --- resolve the target and its mount ---------------------------------------
if [ -z "$TARGET" ]; then
	TARGET=$(findmnt -no TARGET --target "$PWD" 2>/dev/null || true)
	[ -n "$TARGET" ] || TARGET=/
fi
[ -e "$TARGET" ] || { echo "disk-report: no such path: $TARGET" >&2; exit 1; }

fstype=""
src=""
MNT="$TARGET"
if command -v findmnt >/dev/null 2>&1; then
	fstype=$(findmnt -no FSTYPE --target "$TARGET" 2>/dev/null || true)
	src=$(findmnt -no SOURCE --target "$TARGET" 2>/dev/null || true)
	m=$(findmnt -no TARGET --target "$TARGET" 2>/dev/null || true)
	[ -n "$m" ] && MNT="$m"
fi

# --- capacity ---------------------------------------------------------------
cap_bytes=0
used_bytes=0
free_bytes=0
if command -v df >/dev/null 2>&1; then
	line=$(df -B1 --output=size,used,avail "$TARGET" 2>/dev/null | tail -1 || true)
	if [ -n "$line" ]; then
		read -r cap_bytes used_bytes free_bytes <<< "$line" || true
	fi
fi
cap_bytes=${cap_bytes:-0}
used_bytes=${used_bytes:-0}
free_bytes=${free_bytes:-0}

# --- btrfs data / metadata (bytes, summed over every profile line) -----------
data_used=""
meta_used=""
if [ "$fstype" = "btrfs" ] && command -v btrfs >/dev/null 2>&1; then
	if usage_out=$(btrfs filesystem usage -b "$TARGET" 2>/dev/null); then
		# `-b` alone; `--iec` would override it and hand back human units.
		data_used=$(printf '%s\n' "$usage_out" | awk -F: '
			/^Data,/     { for (i = 1; i <= NF; i++) if ($i ~ /Used/) { split($(i+1), b, " "); s += b[1] } }
			END          { print s + 0 }')
		meta_used=$(printf '%s\n' "$usage_out" | awk -F: '
			/^Metadata,/ { for (i = 1; i <= NF; i++) if ($i ~ /Used/) { split($(i+1), b, " "); s += b[1] } }
			END          { print s + 0 }')
	fi
fi

# --- live files -------------------------------------------------------------
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

unreadable=""
MEASURED=0
measure() { # <path> [extra-du-flags]
	local out bad
	out=$(du -s $2 --block-size=1 "$1" 2>"$tmpdir/err" || true)
	if [ -s "$tmpdir/err" ]; then
		# Record the path du actually failed on, not the walk root — the root says
		# nothing useful when the unreadable thing is /home/.snapshots. Two per walk
		# is enough to diagnose and keeps the report readable.
		bad=$(sed -n "s/^du: [a-zA-Z ]*'\([^']*\)'.*/\1/p" "$tmpdir/err" | sort -u | head -2 | tr '\n' ' ' || true)
		[ -n "$bad" ] && unreadable="${unreadable}${bad} "
	fi
	MEASURED=$(printf '%s\n' "$out" | awk 'NR==1 { print $1 + 0 }')
	[ -n "$MEASURED" ] || MEASURED=0
}

device_mounts() { # <device> -> every mountpoint of it, one per line
	local dev="$1"
	[ -n "$dev" ] || return 0
	df -B1 --output=source,target 2>/dev/null | awk -v d="$dev" 'NR > 1 && $1 == d { print $2 }'
}

live=0
coverage="complete"
scan_desc=""
rows=""

if [ -n "$PATHS_ARG" ]; then
	coverage="partial"
	scan_desc="the listed paths only"
	IFS=',' read -r -a _paths <<< "$PATHS_ARG"
	for p in "${_paths[@]}"; do
		[ -n "$p" ] || continue
		if [ ! -e "$p" ]; then
			echo "disk-report: skipping (no such path): $p" >&2
			continue
		fi
		[ "$RAW" = 0 ] && echo "disk-report: measuring $p ..." >&2
		measure "$p" ""
		live=$((live + MEASURED))
		rows="${rows}${MEASURED}|${p}"$'\n'
	done
	if [ "$live" -eq 0 ] && [ -z "$rows" ]; then
		echo "disk-report: none of the listed paths exist: $PATHS_ARG" >&2
		exit 1
	fi
else
	# `btrfs filesystem usage` reports per FILESYSTEM, so the live side has to span
	# the whole filesystem — not just the subtree TARGET happens to sit in. That
	# means walking EVERY mount of the device, with -x.
	#
	# WHY -x, AND WHY EVERY MOUNT. On btrfs each subvolume gets its own anonymous
	# st_dev, so `du -x` refuses to descend from one subvolume into another. That is
	# what makes this a sum rather than a double count: du -sx / covers the @
	# subvolume alone (42 GiB here), and /home — a different subvolume of the SAME
	# device — is covered by its own du -sx /home (120 GiB). Drop -x and du crosses
	# the boundary and counts the same bytes twice; walk only one mount and you report
	# a fraction of the filesystem as if it were all of it. Measured on this machine:
	# one mount gave 42 GiB against a real total of 172 GiB — and against 514 GiB of
	# btrfs data, which would have invented ~470 GiB of snapshot overhead.
	dev=$(df -B1 --output=source "$TARGET" 2>/dev/null | tail -1 || true)
	# `|| true`: the live side is a diagnostic and must degrade, not die. df exits non-zero
	# on a stale or vanished mount, and under `set -e` that would abort the whole report
	# before it printed anything — the least useful outcome for the one command whose job
	# is to explain a full disk.
	mounts=$(device_mounts "$dev" || true)
	[ -n "$mounts" ] || mounts="$MNT"
	n=0
	while IFS= read -r m; do
		[ -n "$m" ] || continue
		[ "$RAW" = 0 ] && echo "disk-report: measuring $m ..." >&2
		measure "$m" "-x"
		live=$((live + MEASURED))
		n=$((n + 1))
	done <<< "$mounts"
	if [ "$n" -eq 1 ]; then
		scan_desc="whole filesystem via ${mounts%$'\n'} (du -sx)"
	else
		scan_desc="whole filesystem via $n mounts (du -sx each)"
	fi
fi

# --- the subtraction --------------------------------------------------------
pinned=""
if [ -n "$data_used" ]; then
	pinned=$((data_used - live))
	[ "$pinned" -lt 0 ] && pinned=0
fi

if [ "$RAW" = 1 ]; then
	printf 'fstype=%s data_used=%s metadata_used=%s live=%s pinned=%s coverage=%s\n' \
		"${fstype:-unknown}" "${data_used:-}" "${meta_used:-}" "$live" "${pinned:-}" "$coverage"
	exit 0
fi

# --- human report -----------------------------------------------------------
echo "disk-report: $TARGET"
echo

row() { printf '  %-16s %s\n' "$1" "$2"; }

row "filesystem" "${fstype:-unknown}${src:+   $src}"
row "capacity" "$(human "$cap_bytes")"
row "used" "$(human "$used_bytes")   ($(pct "$used_bytes" "$cap_bytes")%)"
row "free" "$(human "$free_bytes")"
echo

if [ -n "$data_used" ]; then
	row "btrfs data" "$(human "$data_used")   every extent any subvolume references"
	row "btrfs metadata" "$(human "$meta_used")   excluded: du cannot see it"
	echo
fi

row "live files" "$(human "$live")   $scan_desc"
if [ -n "$rows" ]; then
	printf '%s' "$rows" | while IFS='|' read -r sz p; do
		[ -n "$p" ] || continue
		printf '  %16s   %s\n' "$(human "$sz")" "$p"
	done
fi

if [ -z "$data_used" ]; then
	echo
	echo "  No snapshot attribution: $(printf '%s' "${fstype:-this filesystem}")"
	echo "  is not btrfs (or btrfs-progs is not installed), so there is no"
	echo "  copy-on-write snapshot mechanism to account for."
	exit 0
fi

echo "  ----------------------------------------------"
if [ "$pinned" -gt 0 ]; then
	row "pinned ~" "$(human "$pinned")   $(pct "$pinned" "$data_used")% of data used"
else
	row "pinned ~" "0 B   no snapshot overhead detected"
fi
echo

if [ "$coverage" = "partial" ]; then
	echo "  ⚠ The path set does not span the filesystem, so live data is"
	echo "    undercounted and PINNED IS AN UPPER BOUND. Drop --paths for the"
	echo "    exact figure."
	echo
fi

if [ -n "$unreadable" ]; then
	# Cap the detail. Every walk has its own unreadable corners on a real machine (mode-000
	# dirs, root-owned snapshot dirs, container overlay layers), and a wall of paths buries
	# the one line that matters: pinned is an upper bound, not a measurement.
	n_bad=$(printf '%s\n' $unreadable | grep -c . || true)
	echo "  ⚠ Not everything was readable, so LIVE DATA IS A LOWER BOUND and PINNED"
	echo "    is an upper bound. du could not read ${n_bad} path(s), including:"
	printf '%s\n' $unreadable | head -4 | sed 's/^/      /' || true
	echo "    Re-run with privileges (sudo) for the exact figure."
	echo
fi

if [ "$pinned" -gt 0 ]; then
	cat <<-EOF
	  Data that exists in NO live file — held only by a btrfs snapshot.
	  \`df\` reports free space, so it stays silent about this: deleting files
	  does not free an extent until the last snapshot referencing it is gone.

	  confirm / act (root):
	    sudo snapper list-configs
	    sudo snapper -c <cfg> list
	    # Bytes EXCLUSIVE to each snapshot = what deleting it actually frees.
	    # The glob MUST expand as ROOT: /home/.snapshots is drwxr-x--- root:root, so an
	    # unprivileged shell passes the pattern through literally and btrfs then reports
	    # "No such file or directory". `sudo` does not help — it runs the command, but the
	    # glob was already expanded by YOUR shell before sudo started.
	    sudo sh -c 'btrfs filesystem du -s --raw ${MNT%/}/.snapshots/*/snapshot' | tail -n +2 | sort -k2 -n
	    sudo snapper -c <cfg> delete <N>        # then re-check df

	  STOP IT RECURRING. A btrfs snapshot cannot exclude a path — it is
	  per-subvolume. So either bound snapper's retention, or put regenerable
	  churn (build caches, worktrees, container layers) on a subvolume that is
	  not snapshotted.
	EOF
fi
