#!/usr/bin/env bash
# ============================================================================
# orchicon-cleanup.sh — reclaim disk from Orchicon's own CACHES and LEFTOVERS.
#
# WHY THIS EXISTS. A heavy dev host accumulates leftovers that no single command owns:
#
#   go caches     the compiler's build cache (66 GB measured here), the repo-local `.dev/tools`
#                 caches, and Go's module cache. Bounded by `make cache-trim`, unbounded otherwise.
#   worktrees     `.orchicon-worktrees/<id>` — one per run and per capture. A run whose cleanup was
#                 lost (killed plane, container death) leaves its tree AND its git registration, and
#                 nothing ever reclaims it because it is a VALID worktree.
#   logs          `services.log.<stamp>` rotations. The live log is never touched.
#   scratch       the repo's gitignored tmp/.
#
# WHAT IT DOES NOT DO, ON PURPOSE, AND WHY THAT IS NOT CONFIGURABLE. This script removes only what
# regenerates from a build or from the network. Everything else on a dev host is STATE, not a cache —
# containers, telemetry stores, run transcripts, database backups, an instance's data dir — and a flag
# that offers to delete it is a flag that eventually gets typed. Those categories do not exist here.
# Removing a container or an instance is the operator's job, through the instance tooling that knows
# what depends on it.
#
# What it still refuses: a worktree whose work exists nowhere else — uncommitted files, unmerged
# commits, or a detached HEAD.
#
# USAGE
#   scripts/orchicon-cleanup.sh                     # report only — nothing is deleted
#   scripts/orchicon-cleanup.sh --all --apply       # reclaim every category
#   scripts/orchicon-cleanup.sh --go-cache --apply  # just one
#   scripts/orchicon-cleanup.sh --help
#
# CATEGORIES
#   go-cache        $GOCACHE, ~/.cache/go-build, .dev/tools/{gocache,gotmp,gomodcache,gopath}
#   worktrees       leaked/orphaned .orchicon-worktrees/* across every project root found
#   logs            rotated services.log.*
#   scratch         the repo's tmp/
# ============================================================================
set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

C_RESET=$'\033[0m'; C_DIM=$'\033[2m'; C_B=$'\033[1m'
C_RED=$'\033[31m'; C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'; C_CYAN=$'\033[36m'

APPLY=false
CATS=""
PROJECT_ROOTS=()
CACHE_CAP_MB="${CACHE_CAP_MB:-20480}"

say()  { printf '%s\n' "$*"; }
head1() { printf '\n%s%s%s\n' "$C_B" "$*" "$C_RESET"; }
dim()  { printf '%s%s%s\n' "$C_DIM" "$*" "$C_RESET"; }
ok()   { printf '  %s✓%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
did()  { printf '  %s•%s %s\n' "$C_CYAN" "$C_RESET" "$*"; }
skip() { printf '  %s–%s %s\n' "$C_DIM" "$C_RESET" "$*"; }
warn() { printf '  %s!%s %s\n' "$C_YELLOW" "$C_RESET" "$*"; }
err()  { printf '  %s✗%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; }

usage() { sed -n '2,/^# =====/p' "${BASH_SOURCE[0]}" | sed 's/^# \?//;1d;$d'; }

while [ $# -gt 0 ]; do
  case "$1" in
    --apply|-y|--yes)   APPLY=true; shift ;;
    --all)              CATS="${CATS} go-cache worktrees logs scratch"; shift ;;
    --go-cache|--worktrees|--logs|--scratch)
                        CATS="${CATS} ${1#--}"; shift ;;
    # Named so an operator who remembers them learns WHY they are gone rather than assuming a typo.
    --containers|--telemetry|--sessions|--backups|--instance-data|--instance)
                        err "$1 is gone: this script removes CACHES AND LEFTOVERS only." >&2
                        err "Containers and instance state belong to the operator, via the instance tooling." >&2
                        exit 2 ;;
    --project-root)     PROJECT_ROOTS+=("${2:-}"); shift 2 ;;
    --cache-cap-mb)     CACHE_CAP_MB="${2:-}"; shift 2 ;;
    --help|-h)          usage; exit 0 ;;
    *) err "unknown option: $1"; usage >&2; exit 2 ;;
  esac
done
[ -n "${CATS// /}" ] || APPLY=false

want() { case " ${CATS} " in *" $1 "*) return 0 ;; esac; return 1; }
tot()  { du -sh "$1" 2>/dev/null | cut -f1 || echo "?"; }
gb()   { awk -v k="${1:-0}" 'BEGIN{ if (k<1024) printf "%.0fM", k/1024; else printf "%.1fG", k/1048576 }'; }

# remove_path is the one destructive primitive. It refuses a path that is a mount point or that sits
# outside the directories this script is allowed to touch, so a variable that expands to "" or "/" can
# never reach `rm -rf`.
remove_path() {
  local p="$1" why="${2:-}"
  [ -n "$p" ] || { skip "refusing an empty path"; return; }
  case "$p" in /|/home|/usr|"$HOME") err "refusing to remove $p"; return ;; esac
  if mountpoint -q "$p" 2>/dev/null; then err "refusing to remove mount point $p"; return; fi
  if [ ! -e "$p" ]; then skip "absent: $p"; return; fi
  local sz; sz="$(tot "$p")"
  if [ "$APPLY" != true ]; then did "would remove $p  ($sz)${why:+  — $why}"; return; fi
  # rm's stderr is the only thing that names the real cause — a session guard refusing the command, an
  # immutable file, a busy mount. Swallowing it printed "(permissions?)" for every one of them.
  local rmerr
  if rmerr="$(rm -rf -- "$p" 2>&1)"; then
    did "removed $p  ($sz)${why:+  — $why}"
  else
    err "could not remove $p${rmerr:+: $rmerr}"
  fi
}

START_AVAIL="$(df -Pk "$HOME" | awk 'NR==2{print $4}')"

# --- go caches --------------------------------------------------------------
# GOCACHE for THIS repo is .dev/tools/gocache (the Makefile's `?=` default), and the toolchain's own
# default ~/.cache/go-build is what a bare `go test` outside make uses. Both grow; neither is bounded
# by Go's own trim, which is age-based with a five-day floor no fast-growing cache ever reaches.
do_go_cache() {
  head1 "go caches"
  local gocaches=()
  local repo_cache="$REPO_ROOT/.dev/tools/gocache"
  [ -d "$repo_cache" ] && gocaches+=("$repo_cache")
  [ -d "$HOME/.cache/go-build" ] && gocaches+=("$HOME/.cache/go-build")
  if [ "${#gocaches[@]}" -eq 0 ]; then skip "no go build cache found"; return; fi

  for c in "${gocaches[@]}"; do
    if [ -x "$REPO_ROOT/scripts/gocache-trim.sh" ] && [ "$c" = "$repo_cache" ]; then
      # TRIM, do not wipe, for the cache `make build` uses: evicting down to the cap keeps the next
      # build warm. `go clean -cache` is the hammer and recompiles everything. The trim takes the
      # CACHE AS A POSITIONAL ARGUMENT — it defaults to `go env GOCACHE`, which is not this one.
      if [ "$APPLY" = true ]; then
        did "trimming $c to ${CACHE_CAP_MB}MB"
        bash "$REPO_ROOT/scripts/gocache-trim.sh" "$c" --cap-mb "$CACHE_CAP_MB" >/dev/null 2>&1 \
          && ok "trimmed $c (now $(tot "$c"))" || warn "gocache-trim failed on $c"
      else
        did "would trim $c to ${CACHE_CAP_MB}MB (now $(tot "$c"))"
      fi
    else
      remove_path "$c" "go build cache; regenerates on the next build"
    fi
  done

  remove_path "$REPO_ROOT/.dev/tools/gotmp" "repo-local build scratch; regenerates"
  # The module and tool caches regenerate from the network, so the reason says so: on a fully offline
  # host these two are the difference between a working build and none.
  remove_path "$REPO_ROOT/.dev/tools/gomodcache" "module cache; regenerates (needs network)"
  remove_path "$REPO_ROOT/.dev/tools/gopath" "installed tool binaries; regenerates via make tools (needs network)"
  return 0
}

# --- worktrees --------------------------------------------------------------
# A worktree is SAFE to remove only when its content exists somewhere else: its working tree clean,
# and its branch merged into one of the integration branches (or fully pushed). Anything else is
# reported and left. `_stage-*` is never touched — it belongs to an in-flight adoption.
wt_branch_safe() {
  local repo="$1" wt="$2"
  local dirty
  dirty="$(git -C "$wt" status --porcelain 2>/dev/null | grep -v '^?? \.orchicon-worktrees/' || true)"
  if [ -n "$dirty" ]; then echo "uncommitted changes"; return 1; fi
  # An unpushed commit exists nowhere else, so removing the worktree can orphan it entirely.
  local branch; branch="$(git -C "$wt" rev-parse --abbrev-ref HEAD 2>/dev/null || true)"
  if [ -n "$branch" ] && [ "$branch" != "HEAD" ]; then
    if git -C "$repo" rev-parse --verify --quiet "refs/remotes/origin/$branch" >/dev/null 2>&1; then
      if ! git -C "$wt" merge-base --is-ancestor HEAD "refs/remotes/origin/$branch" 2>/dev/null; then
        echo "unpushed commits on $branch"; return 1
      fi
    elif ! git -C "$repo" rev-parse --verify --quiet "develop" >/dev/null 2>&1; then
      echo "branch $branch has no upstream and no develop to compare against"; return 1
    elif ! git -C "$wt" merge-base --is-ancestor HEAD develop 2>/dev/null; then
      echo "branch $branch is not merged into develop"; return 1
    fi
  else
    # DETACHED HEAD: these commits belong to no branch, so removing the worktree strands them.
    local ref anc=false
    for ref in develop origin/develop; do
      if git -C "$wt" rev-parse --verify --quiet "$ref" >/dev/null 2>&1 &&
         git -C "$wt" merge-base --is-ancestor HEAD "$ref" 2>/dev/null; then anc=true; break; fi
    done
    if [ "$anc" != true ]; then echo "detached HEAD with commits on no branch"; return 1; fi
  fi
  return 0
}

# scan_roots prints the roots to search for .orchicon-worktrees. --project-root REPLACES the defaults so a
# sweep can be scoped; the inventory reads this too, or its numbers describe a different tree than --apply.
scan_roots() {
  local roots=()
  if [ "${#PROJECT_ROOTS[@]}" -gt 0 ] && [ -n "${PROJECT_ROOTS[0]:-}" ]; then
    roots=("${PROJECT_ROOTS[@]}")
  else
    roots=("$HOME/projects" "$REPO_ROOT")
  fi
  printf '%s\n' "${roots[@]}"
}

# wt_verdict classifies one .orchicon-worktrees child: `leaked` (registered nowhere), `safe` (its content
# exists elsewhere), or `keep:<why>`. Both the sweep and the inventory read this one function, so the space
# the report calls reclaimable is the space --apply actually reclaims.
wt_verdict() {
  local repo="$1" child="$2" why
  if ! git -C "$repo" worktree list --porcelain 2>/dev/null | grep -qx "worktree $child"; then
    echo leaked; return
  fi
  if why="$(wt_branch_safe "$repo" "$child")"; then echo safe; else echo "keep:$why"; fi
}

do_worktrees() {
  head1 "stale worktrees"
  local roots=() r
  while IFS= read -r r; do if [ -n "$r" ]; then roots+=("$r"); fi; done < <(scan_roots)
  local found_any=false
  local wtroot repo
  while IFS= read -r wtroot; do
    [ -n "$wtroot" ] || continue
    repo="$(dirname "$wtroot")"
    git -C "$repo" rev-parse --git-dir >/dev/null 2>&1 || continue
    found_any=true
    local name child v
    for child in "$wtroot"/*; do
      [ -d "$child" ] || continue
      name="$(basename "$child")"
      case "$name" in _stage-*) skip "$name — staging dir, belongs to an in-flight adoption"; continue ;; esac

      v="$(wt_verdict "$repo" "$child")"
      case "$v" in
        leaked)
          # NOT REGISTERED. This is the leaked shape: the directory outlived its registration, which is
          # why the platform's own sweeper can never reclaim it (that sweeper trusts the registration).
          remove_path "$child" "unregistered worktree dir (leaked)" ;;
        safe)
          if [ "$APPLY" = true ]; then
            git -C "$repo" worktree remove --force "$child" >/dev/null 2>&1 \
              && did "removed worktree $name" || warn "$name — git refused to remove it"
          else
            did "would remove worktree $name  ($(tot "$child"))"
          fi ;;
        *)
          warn "$name — KEPT: ${v#keep:}" ;;
      esac
    done
    if [ "$APPLY" = true ]; then git -C "$repo" worktree prune >/dev/null 2>&1 || true; fi
  done < <( for r in "${roots[@]}"; do find "$r" -maxdepth 3 -type d -name '.orchicon-worktrees' 2>/dev/null; done )
  [ "$found_any" = true ] || skip "no .orchicon-worktrees found under ${roots[*]}"
  return 0
}

# --- logs -------------------------------------------------------------------
do_logs() {
  head1 "rotated logs"
  local found=false f
  for f in "$HOME"/.local/share/orchicon-*/services.log.*; do
    [ -f "$f" ] || continue
    found=true
    remove_path "$f" "rotated plane log"
  done
  [ "$found" = true ] || skip "no rotated services.log files"
  return 0
}

# --- scratch ----------------------------------------------------------------
# tmp/ is build scratch. `.orchicon/sessions` is NOT here and is not a category: it is the transcript
# history of every run — evidence, not garbage, and the only copy.
do_scratch() {
  head1 "scratch"
  remove_path "$REPO_ROOT/tmp" "repo scratch"
  return 0
}

# --- inventory ---------------------------------------------------------------
# WITH NO CATEGORY NAMED, REPORT WHERE THE SPACE IS. This is the command an operator reaches for when
# the disk is full and they do not yet know why, so it must answer that — not print "reporting the full
# inventory" and then list nothing. Sizes only: no decisions, no removals, and the three explicit-only
# categories are marked as such rather than hidden.
do_inventory() {
  head1 "where the disk is"
  local total_kb=0 kb
  size_line() {
    local label="$1" path="$2" note="${3:-}"
    [ -e "$path" ] || return 0
    local human; human="$(tot "$path")"
    local k; k="$(du -sk "$path" 2>/dev/null | cut -f1 || echo 0)"
    total_kb=$(( total_kb + k ))
    printf '  %-46s %8s  %s\n' "$label" "$human" "$note"
  }

  local rc="$REPO_ROOT/.dev/tools/gocache"
  size_line ".dev/tools/gocache"        "$rc"                       "trimmed by --go-cache"
  size_line "~/.cache/go-build"         "$HOME/.cache/go-build"     "removed by --go-cache"
  size_line ".dev/tools/gotmp"          "$REPO_ROOT/.dev/tools/gotmp"     "removed by --go-cache"
  size_line ".dev/tools/gomodcache"     "$REPO_ROOT/.dev/tools/gomodcache" "removed by --go-cache"
  size_line ".dev/tools/gopath"         "$REPO_ROOT/.dev/tools/gopath"    "removed by --go-cache"

  # Worktrees: total, and the part --worktrees would actually reclaim. Both come from wt_verdict, so the
  # report cannot promise space the sweep will keep — and only the reclaimable part enters the total.
  local wt_kb=0 wt_n=0 wt_r_kb=0 wt_r_n=0 wtroot repo child k
  while IFS= read -r wtroot; do
    [ -n "$wtroot" ] || continue
    repo="$(dirname "$wtroot")"
    git -C "$repo" rev-parse --git-dir >/dev/null 2>&1 || continue
    for child in "$wtroot"/*; do
      [ -d "$child" ] || continue
      case "$(basename "$child")" in _stage-*) continue ;; esac
      wt_n=$(( wt_n + 1 ))
      k="$(du -sk "$child" 2>/dev/null | cut -f1 || echo 0)"
      wt_kb=$(( wt_kb + k ))
      case "$(wt_verdict "$repo" "$child")" in
        leaked|safe) wt_r_n=$(( wt_r_n + 1 )); wt_r_kb=$(( wt_r_kb + k )) ;;
      esac
    done
  done < <( scan_roots | while IFS= read -r r; do find "$r" -maxdepth 3 -type d -name '.orchicon-worktrees' 2>/dev/null; done )
  if [ "$wt_n" -gt 0 ]; then
    total_kb=$(( total_kb + wt_r_kb ))
    printf '  %-46s %8s  %s\n' "worktrees ($wt_n, $wt_r_n reclaimable)" "$(gb "$wt_kb")" \
      "$(gb "$wt_r_kb") reclaimable; the rest hold unmerged or unpushed work"
  fi

  local log_kb=0 f
  for f in "$HOME"/.local/share/orchicon-*/services.log.*; do
    [ -f "$f" ] || continue
    log_kb=$(( log_kb + $(du -sk "$f" 2>/dev/null | cut -f1 || echo 0) ))
  done
  if [ "$log_kb" -gt 0 ]; then
    total_kb=$(( total_kb + log_kb ))
    printf '  %-46s %8s  %s\n' "rotated services.log" "$(awk -v k="$log_kb" 'BEGIN{printf "%.1fG", k/1048576}')" "removed by --logs"
  fi
  size_line "tmp/"                     "$REPO_ROOT/tmp"            "removed by --scratch"

  head1 "total"
  printf '  %-46s %8s\n' "sum of the lines above (overlaps possible)" "$(gb "$total_kb")"
  printf '  %-46s %8s\n' "filesystem available now" "$(df -Ph "$HOME" | awk 'NR==2{print $4}')"
  dim "not listed, because this script cannot remove them: containers, telemetry stores, run"
  dim "transcripts, database backups, instance data dirs. Those are state, not cache."
  return 0
}

# --- run --------------------------------------------------------------------
if [ "$APPLY" != true ]; then
  say "${C_YELLOW}DRY RUN${C_RESET} — nothing will be deleted. Re-run with ${C_B}--apply${C_RESET} to act."
fi

if [ -z "${CATS// /}" ]; then
  do_inventory
fi
want go-cache     && do_go_cache      || true
want worktrees    && do_worktrees     || true
want logs         && do_logs          || true
want scratch      && do_scratch       || true

# An inventory-only run already printed its own totals; repeating them is noise.
if [ -n "${CATS// /}" ]; then
END_AVAIL="$(df -Pk "$HOME" | awk 'NR==2{print $4}')"
head1 "result"
printf '  available now   %s\n' "$(df -Ph "$HOME" | awk 'NR==2{print $4}')"
if [ "$END_AVAIL" -gt "$START_AVAIL" ] 2>/dev/null; then
  # Adaptive unit: a reclaim of a few megabytes printing as "0.0 GiB" reads as "it did nothing",
  # which is exactly the wrong message after it worked.
  printf '  reclaimed       %s\n' "$(awk -v a="$START_AVAIL" -v b="$END_AVAIL" 'BEGIN{
    mb=(b-a)/1024
    if (mb < 1)      printf "%.0f KiB", mb*1024
    else if (mb<1024) printf "%.1f MiB", mb
    else              printf "%.1f GiB", mb/1024 }')"
fi
if [ "$APPLY" != true ]; then
  say ""
  say "  This was a REPORT. ${C_B}--apply${C_RESET} performs the removals listed above."
fi
fi
say ""
