#!/usr/bin/env bash
# ============================================================================
# gocache-trim/run.sh — assert the invariants of scripts/gocache-trim.sh.
#
# WHY THIS EXISTS: gocache-trim.sh DELETES FILES, and its whole job is to choose
# WHICH ones. Every failure mode is silent and destructive in the same way — the
# wrong entries go, the right ones stay, and the only symptom is a rebuilt
# machine that is mysteriously slow:
#
#   1. Wrong order. Oldest-first is the entire policy; evicting newest-first
#      throws away the hot entries the next build wants and keeps the cold ones.
#      A cache still over its cap looks the same either way.
#   2. Grace ignored. Evicting an entry used minutes ago can delete a file out
#      from under a build running CONCURRENTLY with the trim.
#   3. Non-entries touched. trim.txt and README live in the cache root and are
#      Go's own bookkeeping; a stray file in a subdir is not a cache entry
#      (the toolchain's trimSubdir skips non `-a`/`-d` names for the same
#      reason). Removing either corrupts state this script does not own.
#   4. Refusal lost. The safety checks are what stand between a mistyped
#      argument and somebody's data directory. A refusal that decays into a
#      successful delete is the expensive kind of regression.
#
# The cache is FAKED — 256 hex subdirs, entries with controlled mtimes and
# (sparse) sizes — so the assertions are exact and no real cache is touched.
#
#   scripts/tests/gocache-trim/run.sh [gocache-trim.sh]
#
# Exits 0 only when every assertion passes.
# ============================================================================
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../.." && pwd)"
TRIM_SH="${REPO_ROOT}/scripts/gocache-trim.sh"
[ "$#" -gt 0 ] && TRIM_SH="$1"

if [ ! -f "$TRIM_SH" ]; then
  echo "run.sh: gocache-trim.sh not found at ${TRIM_SH}" >&2
  exit 2
fi

PASSED=0
FAILED=0

check() { # <label> <expected> <actual>
  if [ "$2" = "$3" ]; then
    printf '  \033[32mPASS\033[0m  %-56s %s\n' "$1" "$3"
    PASSED=$((PASSED + 1))
  else
    printf '  \033[31mFAIL\033[0m  %-56s want [%s] got [%s]\n' "$1" "$2" "$3"
    FAILED=$((FAILED + 1))
  fi
}

check_le() { # <label> <max> <actual>
  if [ "$3" -le "$2" ] 2>/dev/null; then
    printf '  \033[32mPASS\033[0m  %-56s %s <= %s\n' "$1" "$3" "$2"
    PASSED=$((PASSED + 1))
  else
    printf '  \033[31mFAIL\033[0m  %-56s %s <= %s\n' "$1" "$3" "$2"
    FAILED=$((FAILED + 1))
  fi
}

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

TODAY="$(date +%s)"

# entry <cachedir> <seconds-old> <size-bytes> <name> — a sparse file with a
# chosen apparent size and mtime, exactly the shape gocache-trim.sh reads.
entry() {
  local dir="$1" age="$2" size="$3" name="$4"
  mkdir -p "$dir/${name:0:2}"
  truncate -s "$size" "$dir/${name:0:2}/${name}"
  touch -d "@$((TODAY - age))" "$dir/${name:0:2}/${name}"
}

# new_cache <dir> — a cache-shaped root (Go's own bookkeeping included).
new_cache() {
  rm -rf "$1"; mkdir -p "$1"
  printf '%s\n' "$TODAY" > "$1/trim.txt"
  printf 'This is a Go build cache.\n' > "$1/README"
}

# Distinct, valid 64-hex entry names WITH Go's own `-a`/`-d` suffix. The suffix
# is load-bearing, not cosmetic: gocache-trim.sh only treats `*-a` / `*-d` two
# levels down as cache entries (Go writes nothing else), so a fixture of bare
# hex names would match nothing and every deletion assertion would pass
# VACUOUSLY while the script deleted nothing at all.
entry_name() { printf '%s-a' "$(printf '%s' "$1" | sha256sum | cut -c1-64)"; }
dir_name()   { printf '%s-d' "$(printf '%s' "$1" | sha256sum | cut -c1-64)"; }

cache_bytes() { # <dir>
  find "$1" -mindepth 2 -maxdepth 2 -type f -printf '%s\n' 2>/dev/null | awk '{t+=$1} END{print t+0}'
}

echo "gocache-trim.sh under test: ${TRIM_SH}"
echo

# --- (i) OLDEST-FIRST, and the cap is actually enforced ----------------------
# 6 entries x 10 MiB = 60 MiB. Cap 30 MiB, so the excess is EXACTLY 30 MiB and
# exactly the 3 OLDEST must go — the 3 NEWEST must survive untouched. (A 25 MiB
# cap would require evicting FOUR: three survivors are still 30 MiB, over cap.)
C1="$WORK/c1"; new_cache "$C1"
OLDS=(); for i in 1 2 3 4 5 6; do
  n="$(entry_name "c1-$i")"; OLDS+=("$n")
  entry "$C1" $(( (7 - i) * 86400 )) $((10 * 1024 * 1024)) "$n"
done
before1="$(cache_bytes "$C1")"
out1="$(bash "$TRIM_SH" "$C1" --cap-mb 30 --grace 1h 2>&1)"; rc1=$?
after1="$(cache_bytes "$C1")"

check "trim exits 0" "0" "$rc1"
check "before is 60 MiB" "$((60 * 1024 * 1024))" "$before1"
check_le "result is at or under the 30 MiB cap" "$((30 * 1024 * 1024))" "$after1"
check "the 3 NEWEST entries survive" "3" \
  "$(for i in 4 5 6; do [ -f "$C1/${OLDS[$((i-1))]:0:2}/${OLDS[$((i-1))]}" ] && echo x; done | wc -l | tr -d ' ')"
check "the 3 OLDEST entries are gone" "0" \
  "$(for i in 1 2 3; do [ -f "$C1/${OLDS[$((i-1))]:0:2}/${OLDS[$((i-1))]}" ] && echo x; done | wc -l | tr -d ' ')"
check "exactly 3 were evicted, stopping at the cap" "$((30 * 1024 * 1024))" "$after1"
check "trim.txt is untouched" "$TODAY" "$(cat "$C1/trim.txt")"
check "README is untouched" "1" "$([ -f "$C1/README" ] && echo 1 || echo 0)"

# --- (ii) GRACE: a recent entry is never evicted, even to satisfy the cap ----
# One huge FRESH entry (40 MiB), two INSIDE a 2h grace window (30m, 90m), and two
# OUTSIDE it (3h, 4h), 4 MiB each. Cap 5 MiB — the fresh entry alone is 8x the
# cap, yet it must survive: evicting it could delete an artifact a build running
# right now just wrote. Only the out-of-window pair is even eligible, so the cap
# is unreachable here; the assertion is that it evicts what it MAY and stops,
# never reaching for a protected entry.
#
# The boundary is asserted as well: an entry aged EXACTLY the grace window is a
# candidate ("skip if younger than grace"), not exempt — the difference between
# a window that is 2h and one that is 2h-and-a-nanosecond is a bug either way.
C2="$WORK/c2"; new_cache "$C2"
FRESH="$(entry_name c2-fresh)"; entry "$C2" 0 $((40 * 1024 * 1024)) "$FRESH"
EDGE="$(entry_name c2-edge-2h)"
entry "$C2" 1800  $((4 * 1024 * 1024)) "$(entry_name c2-in-30m)"
entry "$C2" 5400  $((4 * 1024 * 1024)) "$(entry_name c2-in-90m)"
entry "$C2" 7200  $((4 * 1024 * 1024)) "$EDGE"
entry "$C2" 10800 $((4 * 1024 * 1024)) "$(entry_name c2-out-3h)"
entry "$C2" 14400 $((4 * 1024 * 1024)) "$(entry_name c2-out-4h)"
bash "$TRIM_SH" "$C2" --cap-mb 5 --grace 2h >/dev/null 2>&1
check "a grace-protected entry survives an unsatisfiable cap" "1" \
  "$([ -f "$C2/${FRESH:0:2}/${FRESH}" ] && echo 1 || echo 0)"
check "an entry used 30m ago survives a 2h grace" "1" \
  "$(n="$(entry_name c2-in-30m)"; [ -f "$C2/${n:0:2}/${n}" ] && echo 1 || echo 0)"
check "an entry used 90m ago survives a 2h grace" "1" \
  "$(n="$(entry_name c2-in-90m)"; [ -f "$C2/${n:0:2}/${n}" ] && echo 1 || echo 0)"
check "an entry aged EXACTLY the grace window is evictable" "0" \
  "$([ -f "$C2/${EDGE:0:2}/${EDGE}" ] && echo 1 || echo 0)"
check "an entry older than the grace window is evicted" "0" \
  "$(n="$(entry_name c2-out-3h)"; [ -f "$C2/${n:0:2}/${n}" ] && echo 1 || echo 0)"
check "a second out-of-window entry is evicted too" "0" \
  "$(n="$(entry_name c2-out-4h)"; [ -f "$C2/${n:0:2}/${n}" ] && echo 1 || echo 0)"

# --- (iii) UNDER CAP is a strict no-op ---------------------------------------
C3="$WORK/c3"; new_cache "$C3"
N3="$(entry_name c3-only)"; entry "$C3" 86400 $((1024 * 1024)) "$N3"
bash "$TRIM_SH" "$C3" --cap-mb 100 --grace 1h >/dev/null 2>&1
check "an under-cap cache is left alone" "1048576" "$(cache_bytes "$C3")"
check "an under-cap cache keeps its mtime" "$((TODAY - 86400))" \
  "$(stat -c %Y "$C3/${N3:0:2}/${N3}")"

# --- (iv) --dry-run DELETES NOTHING ------------------------------------------
C4="$WORK/c4"; new_cache "$C4"
for i in 1 2 3; do entry "$C4" $((i * 86400)) $((10 * 1024 * 1024)) "$(entry_name "c4-$i")"; done
before4="$(cache_bytes "$C4")"
out4="$(bash "$TRIM_SH" "$C4" --cap-mb 5 --grace 1h --dry-run 2>&1)"; rc4=$?
check "dry run exits 0" "0" "$rc4"
check "dry run deletes nothing" "$before4" "$(cache_bytes "$C4")"
case "$out4" in *"dry run"*) check "dry run says so" "says so" "says so" ;;
                 *)     check "dry run says so" "says so" "MISSING" ;; esac

# --- (v) NON-ENTRY files in a subdir are never candidates --------------------
# Go only ever writes -a and -d entries; trimSubdir skips anything else. A stray
# file must therefore survive even when the cache is wildly over cap.
C5="$WORK/c5"; new_cache "$C5"
entry "$C5" $((10 * 86400)) $((10 * 1024 * 1024)) "$(entry_name c5-real)"
entry "$C5" $((11 * 86400)) $((10 * 1024 * 1024)) "$(dir_name c5-realdir)"
mkdir -p "$C5/ab"; truncate -s $((10 * 1024 * 1024)) "$C5/ab/not-a-cache-entry.txt"
bash "$TRIM_SH" "$C5" --cap-mb 1 --grace 1h >/dev/null 2>&1
check "a non -a/-d file survives a trim" "1" \
  "$([ -f "$C5/ab/not-a-cache-entry.txt" ] && echo 1 || echo 0)"

# --- (vi) REFUSALS: a directory that is not a cache is never touched ---------
C6="$WORK/notacache"; mkdir -p "$C6/docs"; echo "important" > "$C6/docs/notes.md"
if bash "$TRIM_SH" "$C6" --cap-mb 1 --grace 1h >/dev/null 2>&1; then
  check "a non-cache directory is refused" "non-zero exit" "exit 0"
else
  check "a non-cache directory is refused" "non-zero exit" "non-zero exit"
fi
check "the refused directory is intact" "important" "$(cat "$C6/docs/notes.md" 2>/dev/null)"

if bash "$TRIM_SH" "$WORK/nonexistent" --cap-mb 1 >/dev/null 2>&1; then
  check "a missing directory is refused" "non-zero exit" "exit 0"
else
  check "a missing directory is refused" "non-zero exit" "non-zero exit"
fi

if bash "$TRIM_SH" "$REPO_ROOT" --cap-mb 1 >/dev/null 2>&1; then
  check "a git repo root is refused" "non-zero exit" "exit 0"
else
  check "a git repo root is refused" "non-zero exit" "non-zero exit"
fi

if bash "$TRIM_SH" "$C1" --cap-mb notanumber >/dev/null 2>&1; then
  check "a non-numeric cap is refused" "non-zero exit" "exit 0"
else
  check "a non-numeric cap is refused" "non-zero exit" "non-zero exit"
fi

# --- (vii) cap 0 disables the trim entirely ---------------------------------
C7="$WORK/c7"; new_cache "$C7"
entry "$C7" $((10 * 86400)) $((10 * 1024 * 1024)) "$(entry_name c7-a)"
before7="$(cache_bytes "$C7")"
bash "$TRIM_SH" "$C7" --cap-mb 0 >/dev/null 2>&1
check "cap 0 deletes nothing" "$before7" "$(cache_bytes "$C7")"

# --- (vii-b) NO-OP SAFETY: a cache at cap is not churned ---------------------
# With every entry outside the grace window and the total EXACTLY at the cap,
# nothing may be evicted: excess <= 0, so this is the under-cap path again but
# with deletable candidates present. Guards the boundary ("<=" not "<").
C9="$WORK/c9"; new_cache "$C9"
entry "$C9" $((10 * 86400)) $((20 * 1024 * 1024)) "$(entry_name c9-a)"
entry "$C9" $((9 * 86400)) $((5 * 1024 * 1024)) "$(entry_name c9-b)"
before9="$(cache_bytes "$C9")"
bash "$TRIM_SH" "$C9" --cap-mb 25 --grace 1h >/dev/null 2>&1
check "a cache exactly at cap is left alone" "$before9" "$(cache_bytes "$C9")"

# --- (viii) an EMPTY cache is fine, not an error ----------------------------
C8="$WORK/c8"; mkdir -p "$C8"
if bash "$TRIM_SH" "$C8" --cap-mb 1 >/dev/null 2>&1; then
  check "an empty cache dir is accepted" "0" "0"
else
  check "an empty cache dir is accepted" "0" "refused"
fi

echo
echo "  gocache-trim harness: ${PASSED} passed, ${FAILED} failed"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
