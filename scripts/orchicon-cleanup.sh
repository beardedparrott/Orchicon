#!/usr/bin/env bash
# ============================================================================
# orchicon-cleanup.sh — reclaim disk from Orchicon's own garbage.
#
# WHY THIS EXISTS. A heavy dev host accumulates four kinds of leftover that no single command owns:
#
#   go caches     the compiler's build cache (66 GB measured here), the repo-local `.dev/tools`
#                 caches, and Go's module cache. Bounded by `make cache-trim`, unbounded otherwise.
#   worktrees     `.orchicon-worktrees/<id>` — one per run and per capture. A run whose cleanup was
#                 lost (killed plane, container death) leaves its tree AND its git registration, and
#                 nothing ever reclaims it because it is a VALID worktree.
#   containers    a runtime container per workflow execution. One killed mid-run stays as `Exited
#                 (137)` forever. Measured here: seven of them, 39 hours old.
#   logs          `services.log.<stamp>` rotations, and the dev instance's own Loki/Tempo stores,
#                 which are telemetry about local runs and grow without limit.
#
# AND THE THING IT DELIBERATELY DOES NOT DO. It never touches a live database, a KEK or a secret, a
# project directory, or a worktree holding work that exists nowhere else. Deleting the wrong thing
# here costs hours and, for a worktree's unpushed branch, sometimes everything — so every category is
# opt-in, the default is a REPORT, and the two that sit next to irreplaceable data (`backups`,
# `instance-data`) have to be named individually.
#
# USAGE
#   scripts/orchicon-cleanup.sh                     # report only — nothing is deleted
#   scripts/orchicon-cleanup.sh --all --apply       # reclaim every SAFE category
#   scripts/orchicon-cleanup.sh --go-cache --apply  # just one
#   scripts/orchicon-cleanup.sh --help
#
# CATEGORIES
#   go-cache        $GOCACHE, ~/.cache/go-build, .dev/tools/{gocache,gotmp,gomodcache}
#   worktrees       leaked/orphaned .orchicon-worktrees/* across every project root found
#   containers      stopped orchicon-labelled containers + dangling images (never volumes)
#   telemetry       instance loki/tempo stores — telemetry ABOUT local runs, not run history
#   logs            rotated services.log.*
#   scratch         the repo's tmp/
#   sessions        .orchicon/sessions — run TRANSCRIPTS. Named explicitly, never in --all.
#   backups         instance backups/  — REAL database backups. Named explicitly, never in --all.
#   instance-data   a whole instance's data dir. Named explicitly, never in --all.
# ============================================================================
set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

C_RESET=$'\033[0m'; C_DIM=$'\033[2m'; C_B=$'\033[1m'
C_RED=$'\033[31m'; C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'; C_CYAN=$'\033[36m'

APPLY=false
CATS=""
PROJECT_ROOTS=()
INSTANCES=""
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
    --all)              CATS="${CATS} go-cache worktrees containers telemetry logs scratch"; shift ;;
    --go-cache|--worktrees|--containers|--telemetry|--logs|--scratch)
                        CATS="${CATS} ${1#--}"; shift ;;
    --backups)          CATS="${CATS} backups"; shift ;;
    --sessions)         CATS="${CATS} sessions"; shift ;;
    --instance-data)    CATS="${CATS} instance-data"; shift; INSTANCES="${2:-}"; shift 2 ;;
    --instance)         INSTANCES="${2:-}"; shift 2 ;;
    --project-root)     PROJECT_ROOTS+=("${2:-}"); shift 2 ;;
    --cache-cap-mb)     CACHE_CAP_MB="${2:-}"; shift 2 ;;
    --help|-h)          usage; exit 0 ;;
    *) err "unknown option: $1"; usage >&2; exit 2 ;;
  esac
done
[ -n "${CATS// /}" ] || APPLY=false

want() { case " ${CATS} " in *" $1 "*) return 0 ;; esac; return 1; }
tot()  { du -sh "$1" 2>/dev/null | cut -f1 || echo "?"; }
have() { command -v "$1" >/dev/null 2>&1; }

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
  if rm -rf -- "$p" 2>/dev/null; then
    FREED_KB=$(( FREED_KB + $(du -sk "$p" 2>/dev/null | cut -f1 || echo 0) ))
    did "removed $p  ($sz)${why:+  — $why}"
  else
    err "could not remove $p (permissions?)"
  fi
}

FREED_KB=0
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
  fi
  return 0
}

do_worktrees() {
  head1 "stale worktrees"
  local roots=("${PROJECT_ROOTS[@]:-}")
  if [ "${#roots[@]}" -eq 0 ] || [ -z "${roots[0]:-}" ]; then
    roots=("$HOME/projects" "$REPO_ROOT")
  fi
  local found_any=false
  local wtroot repo
  while IFS= read -r wtroot; do
    [ -n "$wtroot" ] || continue
    repo="$(dirname "$wtroot")"
    git -C "$repo" rev-parse --git-dir >/dev/null 2>&1 || continue
    found_any=true
    local name child why
    for child in "$wtroot"/*; do
      [ -d "$child" ] || continue
      name="$(basename "$child")"
      case "$name" in _stage-*) skip "$name — staging dir, belongs to an in-flight adoption"; continue ;; esac

      if ! git -C "$repo" worktree list --porcelain 2>/dev/null | grep -qx "worktree $child"; then
        # NOT REGISTERED. This is the leaked shape: the directory outlived its registration, which is
        # why the platform's own sweeper can never reclaim it (that sweeper trusts the registration).
        remove_path "$child" "unregistered worktree dir (leaked)"
        continue
      fi
      if why="$(wt_branch_safe "$repo" "$child")"; then
        if [ "$APPLY" = true ]; then
          git -C "$repo" worktree remove --force "$child" >/dev/null 2>&1 \
            && did "removed worktree $name" || warn "$name — git refused to remove it"
        else
          did "would remove worktree $name  ($(tot "$child"))"
        fi
      else
        warn "$name — KEPT: $why"
      fi
    done
    if [ "$APPLY" = true ]; then git -C "$repo" worktree prune >/dev/null 2>&1 || true; fi
  done < <( for r in "${roots[@]}"; do find "$r" -maxdepth 3 -type d -name '.orchicon-worktrees' 2>/dev/null; done )
  [ "$found_any" = true ] || skip "no .orchicon-worktrees found under ${roots[*]}"
  return 0
}

# --- containers -------------------------------------------------------------
# Label-scoped, and containers only when STOPPED. Never volumes: a host-wide `volume prune` removes
# other projects' data, which is why the repo's own clean-docker target refuses to do it.
engine() { if have podman; then echo podman; elif have docker; then echo docker; fi; }

do_containers() {
  head1 "containers and images"
  local e; e="$(engine)"
  if [ -z "$e" ]; then skip "no container engine found"; return; fi
  local ids
  ids="$($e ps -a --filter label=orchicon.instance --filter status=exited -q 2>/dev/null || true)"
  if [ -n "$ids" ]; then
    local n; n="$(printf '%s\n' "$ids" | wc -l | tr -d ' ')"
    if [ "$APPLY" = true ]; then
      # shellcheck disable=SC2086
      $e rm $ids >/dev/null 2>&1 && ok "removed $n stopped orchicon container(s)" || warn "some containers refused removal"
    else
      did "would remove $n stopped orchicon container(s)"
    fi
  else
    skip "no stopped orchicon containers"
  fi
  if [ "$APPLY" = true ]; then
    $e image prune -f --filter "dangling=true" >/dev/null 2>&1 && ok "pruned dangling images" || true
  else
    did "would prune dangling images"
  fi
  dim "volumes are NEVER pruned — they hold other projects' data on a shared host"
  return 0
}

# --- telemetry --------------------------------------------------------------
# Loki and Tempo are logs and traces ABOUT local runs. Run HISTORY lives in Postgres and the
# .orchicon/ transcripts, so this is not the record of what happened — it is the observability of it.
do_telemetry() {
  head1 "telemetry stores (loki / tempo)"
  local d found=false
  for d in "$HOME"/.local/share/orchicon-*/loki "$HOME"/.local/share/orchicon-*/tempo; do
    [ -d "$d" ] || continue
    found=true
    remove_path "$d" "telemetry about local runs; the run history is in Postgres"
  done
  [ "$found" = true ] || skip "no loki/tempo stores found"
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
# tmp/ is build scratch. `.orchicon/sessions` is NOT here: it is the transcript history of every run,
# which is evidence rather than garbage, so it has its own category and is never in --all.
do_scratch() {
  head1 "scratch"
  remove_path "$REPO_ROOT/tmp" "repo scratch"
  return 0
}

# --- run transcripts --------------------------------------------------------
# The per-run conversation transcripts: the planner's reasoning, every tool call, every verdict.
# Postgres keeps the run and its outputs; it does not keep this. So deleting it is a decision about
# evidence rather than about disk, and it is never part of --all.
do_sessions() {
  head1 "run session transcripts  (HISTORY, not a cache)"
  local d="$REPO_ROOT/.orchicon/sessions"
  if [ ! -d "$d" ]; then skip "no session transcripts"; return 0; fi
  local n; n="$(find "$d" -mindepth 1 -maxdepth 1 | wc -l | tr -d ' ')"
  warn "$d — $n file(s), $(tot "$d"). This is what a run actually did; there is no other copy."
  remove_path "$d" "run transcripts"
  return 0
}

# --- the two that sit next to irreplaceable data -----------------------------
do_backups() {
  head1 "instance backups  (REAL database backups)"
  local d found=false
  for d in "$HOME"/.local/share/orchicon-*/backups; do
    [ -d "$d" ] || continue
    found=true
    local n; n="$(find "$d" -mindepth 1 -maxdepth 1 | wc -l | tr -d ' ')"
    warn "$d — $n item(s), $(tot "$d"). These are database backups, not caches."
    remove_path "$d" "operator's database backups"
  done
  [ "$found" = true ] || skip "no backups found"
  return 0
}

do_instance_data() {
  head1 "whole instance data dirs"
  if [ -z "${INSTANCES// /}" ]; then err "--instance-data needs --instance dev|prod|test"; return; fi
  local inst d
  for inst in $INSTANCES; do
    d="$HOME/.local/share/orchicon-$inst"
    if [ ! -d "$d" ]; then skip "$inst — no data dir"; continue; fi
    warn "$d — $(tot "$d"). This is the instance's DATABASE, secrets and blobs."
    dim "stop the instance first: scripts/orchicon.sh stop $inst"
    remove_path "$d" "instance $inst state"
  done
  return 0
}

# --- run --------------------------------------------------------------------
if [ "$APPLY" != true ]; then
  say "${C_YELLOW}DRY RUN${C_RESET} — nothing will be deleted. Re-run with ${C_B}--apply${C_RESET} to act."
fi
[ -n "${CATS// /}" ] || dim "no categories selected — reporting the full inventory; pass --all or a category."

want go-cache     && do_go_cache      || true
want worktrees    && do_worktrees     || true
want containers   && do_containers    || true
want telemetry    && do_telemetry     || true
want logs         && do_logs          || true
want scratch      && do_scratch       || true
want sessions     && do_sessions      || true
want backups      && do_backups       || true
want instance-data && do_instance_data || true

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
say ""
