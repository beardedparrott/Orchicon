#!/usr/bin/env bash
# ============================================================================
# gocache-trim.sh — bound the Go build cache by SIZE, evicting oldest-first.
#
# WHY THIS EXISTS (and why `go clean -cache` / Go's own trim are not enough).
#
# Go's build cache DOES self-trim, but its policy cannot bound a fast-growing
# cache. From the toolchain source (src/cmd/go/internal/cache/cache.go):
#
#	trimInterval = 24 * time.Hour   // scan at most once/day
#	trimLimit    = 5 * 24 * time.Hour  // never delete anything used within 5 days
#	cutoff := now.Add(-trimLimit - mtimeInterval)
#
# A trim is AGE-based with a hard FIVE-DAY floor. That is a fine policy for an
# ordinary dev cache, and a structurally useless one for this repo's, because
# the compiler never keeps still long enough to have five-day-old entries:
#
#	observed: 71.6 GB, 99,869 entries, and ZERO of them older than 5 days.
#	           A trim had completed 50 minutes earlier and removed nothing.
#
# The reason the cache grows that fast is the cross-platform compile gate:
# `make cross-compile` (run by `ci-go`, and therefore by every `make
# rebuild-dev`) compiles the two shipped binaries for SIX platforms, and the
# per-platform package archives are ~130 MB each. `make ci-go` on this repo
# added ~32 GB in a single day — all of it younger than Go's cutoff, so Go
# could never reclaim any of it.
#
# The cross-compile gate now uses a THROWAWAY GOCACHE (Makefile, cross-compile),
# which removes that growth at the source. This script is the belt to that
# braces: a SIZE cap, which is the only policy that bounds growth regardless of
# how fast it arrives. Age cannot: 32 GB in 24 hours is un-bounded by any age
# rule that must spare recent entries.
#
# SEMANTICS — deliberately Go's own, so this cannot fight the real cache:
#   * Only real cache entries are candidates: `<xx>/<hex>-a` and `<xx>/<hex>-d`.
#     Go writes nothing else into the 256 subdirs, and `trimSubdir` in the
#     toolchain skips anything else for the same reason. trim.txt and README
#     are never touched.
#   * mtime IS last-use. Go refreshes an entry's mtime when it reuses it, at
#     most hourly (mtimeInterval). So "oldest mtime first" is "coldest first",
#     and evicting in that order keeps the entries the next build wants.
#   * Nothing used within --grace is ever evicted. Default 1h is exactly Go's
#     own mtimeInterval: it guarantees a build running CONCURRENTLY with this
#     script cannot have a just-written artifact deleted out from under it.
#     The cost of the window is that a cache cannot be trimmed below what was
#     touched in the last hour — which is the correct trade.
#
#   scripts/gocache-trim.sh [CACHE_DIR] [--cap-mb N] [--grace 1h] [--dry-run] [-v]
#
# Exit 0 always when the trim ran (or had nothing to do); non-zero only when the
# arguments or the target are refused.
# ============================================================================
set -euo pipefail

MB=$((1024 * 1024))

usage() {
  cat >&2 <<'EOF'
usage: gocache-trim.sh [CACHE_DIR] [--cap-mb N] [--grace DURATION] [--dry-run] [-v]

  CACHE_DIR        Go build cache to trim (default: `go env GOCACHE`)
  --cap-mb N       Size cap in MiB (default 20480 = 20 GiB; 0 = no trim)
  --grace DURATION Never evict entries used this recently (default 1h)
  --dry-run        Report what would be evicted; delete nothing
  -v, --verbose    Per-entry detail
EOF
}

CACHE_DIR=""
CAP_MB=20480
GRACE="1h"
DRY_RUN=0
VERBOSE=0

while [ $# -gt 0 ]; do
  case "$1" in
    --cap-mb)  CAP_MB="${2:-}"; shift 2 ;;
    --cap-mb=*) CAP_MB="${1#*=}"; shift ;;
    --grace)   GRACE="${2:-}"; shift 2 ;;
    --grace=*) GRACE="${1#*=}"; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -v|--verbose) VERBOSE=1; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) echo "gocache-trim: unknown option '$1'" >&2; usage; exit 2 ;;
    *) if [ -z "$CACHE_DIR" ]; then CACHE_DIR="$1"; else
         echo "gocache-trim: unexpected extra argument '$1'" >&2; exit 2
       fi; shift ;;
  esac
done

# Resolve the cache the same way the Makefile does: an explicit argument wins,
# then GOCACHE (already exported by the Makefile), then the toolchain's own
# default. `go env GOCACHE` is the authority for the last case so this script
# agrees with whatever `go` actually uses.
if [ -z "$CACHE_DIR" ]; then
  if [ -n "${GOCACHE:-}" ]; then
    CACHE_DIR="$GOCACHE"
  else
    GO_BIN="${DEV_GO:-go}"
    CACHE_DIR="$("$GO_BIN" env GOCACHE 2>/dev/null || true)"
  fi
fi
[ -n "$CACHE_DIR" ] || { echo "gocache-trim: cannot resolve a cache dir (pass one, or set GOCACHE)" >&2; exit 2; }

case "$CAP_MB" in ''|*[!0-9]*) echo "gocache-trim: --cap-mb must be a non-negative integer (got '$CAP_MB')" >&2; exit 2 ;; esac

grace_seconds() {
  local s="$1"
  case "$s" in
    *d) echo $(( ${s%d} * 86400 )) ;;
    *h) echo $(( ${s%h} * 3600 )) ;;
    *m) echo $(( ${s%m} * 60 )) ;;
    *s) echo "${s%s}" ;;
    *[0-9]) echo "$s" ;;
    *) return 1 ;;
  esac
}
GRACE_SECONDS="$(grace_seconds "$GRACE")" \
  || { echo "gocache-trim: --grace must look like 30m / 1h / 2d (got '$GRACE')" >&2; exit 2; }

human() { # bytes -> human, no external deps beyond awk
  awk -v b="$1" 'BEGIN{
    split("B KiB MiB GiB TiB",u," "); i=1
    while (b >= 1024 && i < 5) { b/=1024; i++ }
    printf (i==1 ? "%.0f %s" : "%.1f %s"), b, u[i]
  }'
}

# human_dur is the DURATION sibling of human(). Reporting a grace window with
# human() would print seconds as bytes (1h -> "3.5 KiB"), which reads as a bug in
# exactly the line an operator checks when a trim evicted less than they wanted.
human_dur() { # seconds -> human duration
  awk -v s="$1" 'BEGIN{
    if (s >= 86400 && s % 86400 == 0) { printf "%dd", s/86400; exit }
    if (s >= 3600 && s % 3600 == 0)   { printf "%dh", s/3600; exit }
    if (s >= 60 && s % 60 == 0)      { printf "%dm", s/60; exit }
    printf "%ds", s
  }'
}

# --- Safety -----------------------------------------------------------------
# This script deletes files. Two refusals, both cheap and both load-bearing:
# the target must LOOK like a Go cache, and it must not be a directory whose
# loss would be somebody's data rather than a rebuildable cache.
if [ ! -d "$CACHE_DIR" ]; then
  echo "gocache-trim: not a directory: $CACHE_DIR" >&2
  exit 2
fi
CACHE_REAL="$(cd "$CACHE_DIR" && pwd -P)"

case "$CACHE_REAL" in
  /|"$HOME")
    echo "gocache-trim: refusing to trim '$CACHE_REAL' — that is not a build cache" >&2
    exit 2 ;;
esac
if [ -d "$CACHE_REAL/.git" ]; then
  echo "gocache-trim: refusing to trim '$CACHE_REAL' — it is a git repository root, not a build cache" >&2
  exit 2
fi

# Shape check: a Go cache carries trim.txt and/or hex subdirs. An EMPTY
# directory is also allowed (a freshly created GOCACHE has neither yet), but a
# non-empty directory with no cache-shaped entry at all is refused — that is
# the signature of a mistyped path, and the cost of being wrong here is
# somebody's files.
has_entries=0
for d in "$CACHE_REAL"/*/; do
  [ -d "$d" ] || continue
  case "$(basename "$d")" in
    [0-9a-f][0-9a-f]) has_entries=1; break ;;
  esac
done
if [ ! -f "$CACHE_REAL/trim.txt" ] && [ "$has_entries" -eq 0 ]; then
  if [ -n "$(ls -A "$CACHE_REAL" 2>/dev/null)" ]; then
    echo "gocache-trim: refusing to trim '$CACHE_REAL' — it does not look like a Go build cache" >&2
    echo "               (no trim.txt and no hex subdirectories, but it is not empty)" >&2
    exit 2
  fi
fi

if [ "$CAP_MB" -eq 0 ]; then
  echo "gocache-trim: cap is 0 — trim disabled, nothing to do"
  exit 0
fi

CAP_BYTES=$((CAP_MB * MB))
NOW="$(date +%s)"

echo "gocache-trim: $CACHE_REAL"
echo "  cap                 $(human "$CAP_BYTES")   grace $(human_dur "$GRACE_SECONDS")"

# --- One scan, then select ---------------------------------------------------
# Scanning 100k entries is the expensive half; deleting is cheap. So scan ONCE
# into a file and do every subsequent pass over that file. Entries live exactly
# two levels down (`<xx>/<hex>-{a,d}`), which is what -mindepth/-maxdepth pin.
WORK="$(mktemp -d "${TMPDIR:-/tmp}/gocache-trim.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
ENTRIES="$WORK/entries"
DEL_LIST="$WORK/delete"

find "$CACHE_REAL" -mindepth 2 -maxdepth 2 -type f \( -name '*-a' -o -name '*-d' \) \
  -printf '%T@\t%s\t%p\n' > "$ENTRIES" 2>/dev/null || true
sort -k1,1g -t$'\t' "$ENTRIES" -o "$ENTRIES"

# Select oldest-first, skipping anything inside the grace window. Emits stats on
# stdout and the eviction list (newline separated) to $DEL_LIST.
STATS="$(awk -v now="$NOW" -v cap="$CAP_BYTES" -v grace="$GRACE_SECONDS" \
             -v out="$DEL_LIST" -F'\t' \
'
  { m[NR]=$1+0; s[NR]=$2+0; p[NR]=$3; total+=s[NR] }
  END {
    protected = 0
    for (i = 1; i <= NR; i++) if (now - m[i] < grace) protected++
    excess = total - cap
    if (excess <= 0) {
      printf "total %d\nexcess 0\nreclaimed 0\ndeleted 0\nprotected %d\nkept_all 1\n", total, protected
      exit 0
    }
    reclaimed = 0; deleted = 0
    for (i = 1; i <= NR; i++) {
      if (now - m[i] < grace) continue   # never evict what may be in use
      print p[i] >> out
      reclaimed += s[i]; deleted++
      if (reclaimed >= excess) break
    }
    printf "total %d\nexcess %d\nreclaimed %d\ndeleted %d\nprotected %d\nkept_all 0\n", \
           total, excess, reclaimed, deleted, protected
  }
' "$ENTRIES")"

field() { printf '%s\n' "$STATS" | awk -v k="$1" '$1==k {print $2; found=1} END{if(!found) print ""}'; }
TOTAL="$(field total)"; EXCESS="$(field excess)"; RECLAIMED="$(field reclaimed)"
DELETED="$(field deleted)"; PROTECTED="$(field protected)"; KEPT_ALL="$(field kept_all)"

echo "  before              $(human "$TOTAL")"
echo "  entries             $(wc -l < "$ENTRIES" | tr -d ' ')  (grace-protected: $PROTECTED)"

if [ "$KEPT_ALL" = "1" ]; then
  echo "  already under cap — nothing to evict"
  exit 0
fi

if [ "$DELETED" -eq 0 ]; then
  # Everything over the cap was touched inside the grace window. Say so
  # plainly: a silent no-op here would look like the trim was broken.
  echo "  over cap by         $(human "$EXCESS") BUT every candidate was used within $(human_dur "$GRACE_SECONDS")"
  echo "  nothing evicted (lower --grace to reclaim, at the risk of a concurrent build)"
  exit 0
fi

if [ "$DRY_RUN" -eq 1 ]; then
  echo "  would reclaim       $(human "$RECLAIMED")  ($DELETED entries)"
  echo "  dry run — nothing deleted"
  [ "$VERBOSE" -eq 1 ] && { echo "  oldest candidates:"; head -5 "$DEL_LIST" | sed 's/^/    /'; }
  exit 0
fi

# Delete in batches: one rm per entry would be 50k forks. GOCACHE entry names
# are 64 hex chars inside two-hex-char dirs, so no path here can contain
# whitespace and xargs' default splitting is safe — stated rather than assumed.
xargs -r -n 400 rm -f -- < "$DEL_LIST" 2>/dev/null || true

AFTER="$(find "$CACHE_REAL" -mindepth 2 -maxdepth 2 -type f \( -name '*-a' -o -name '*-d' \) \
         -printf '%s\n' 2>/dev/null | awk '{t+=$1} END{print t+0}')"

echo "  reclaimed           $(human "$RECLAIMED")  ($DELETED entries, oldest-first)"
echo "  after               $(human "$AFTER")"
if [ "$VERBOSE" -eq 1 ]; then
  echo "  oldest evicted:"
  head -5 "$DEL_LIST" | sed 's/^/    /'
fi
