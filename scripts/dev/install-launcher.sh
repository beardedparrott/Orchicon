#!/usr/bin/env bash
# install-launcher.sh — point a user-facing launcher at a project's binaries, NEVER at a throwaway
# worktree.
#
# THE BUG THIS EXISTS FOR. `make rebuild-dev` symlinks ~/.local/bin/orch and ~/.local/bin/orchicon at
# "$(CURDIR)/bin/<binary>". Run from a git worktree, CURDIR is the WORKTREE — so the operator's launchers
# were repointed into `.orchicon-worktrees/<id>/bin/`, and the moment that worktree was removed
# (`git worktree remove`), both launchers became dangling symlinks. The reported symptom was exactly that:
#
#     make rebuild-dev   →  errored
#     "Also my orch binary has been deleted!"
#
# `orch` was not deleted. Its symlink had been moved onto a directory that no longer exists, which is
# indistinguishable from deletion for every command that runs it.
#
# SO THE RULE IS: a launcher belongs to the MAIN checkout. From a worktree, this refuses to point at the
# worktree and — if a launcher is ALREADY pointing into one — repoints it at the main checkout, repairing
# the damage rather than merely declining to add to it.
#
# usage: install-launcher.sh --root <project root> --launcher-dir <dir> [--dry-run] <binary> [<binary>...]
#
# --dry-run prints what it WOULD link and touches nothing. It exists so a test can assert the DECISION — which
# checkout a launcher is allowed to point at — without creating symlinks, because a test sandbox may (and in
# this repo does) block `ln` outright.
set -euo pipefail

root=""
launcher_dir=""
dry_run=0
bins=()
while [ $# -gt 0 ]; do
  case "$1" in
    --root)          root="$2"; shift 2 ;;
    --launcher-dir)  launcher_dir="$2"; shift 2 ;;
    --dry-run)       dry_run=1; shift ;;
    -h|--help)       sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)               bins+=("$1"); shift ;;
  esac
done
if [ -z "$root" ] || [ -z "$launcher_dir" ] || [ ${#bins[@]} -eq 0 ]; then
  echo "usage: install-launcher.sh --root <project root> --launcher-dir <dir> <binary>..." >&2
  exit 2
fi

# main_worktree prints the main checkout for a worktree, or "" when $1 IS the main checkout.
#
# A worktree's .git is a FILE ("gitdir: <main>/.git/worktrees/<name>"), so the main root is two directories
# up from that. git itself is asked first, and the file is parsed as a fallback because a launcher must not
# depend on a particular git version.
main_worktree() {
  local dir="$1" gitdir=""
  if gitdir="$(git -C "$dir" rev-parse --path-format=absolute --git-common-dir 2>/dev/null)"; then
    case "$gitdir" in
      */.git) printf '%s\n' "${gitdir%/.git}"; return 0 ;;
    esac
  fi
  if [ -f "$dir/.git" ]; then
    gitdir="$(sed -n 's/^gitdir: *//p' "$dir/.git" | head -1)"
    # THE MAIN ROOT IS EVERYTHING BEFORE "/.git/worktrees/". An earlier version stripped two path
    # components instead of three and resolved to "<main>/.git", so it then looked for binaries under .git
    # — and reported "skip orch: …/.git/bin/orch does not exist", which reads as "run make build" when the
    # real problem was the path itself.
    case "$gitdir" in
      */.git/worktrees/*) printf '%s\n' "${gitdir%%/.git/worktrees/*}"; return 0 ;;
    esac
  fi
  return 0
}

target_root="$root"
case "$root" in
  */.orchicon-worktrees/*)
    main="$(main_worktree "$root")"
    if [ -z "$main" ]; then
      echo "==> NOT installing launchers from a worktree: $root"
      echo "    (could not locate the main checkout, so there is nothing safe to point them at)"
      exit 0
    fi
    target_root="$main"
    echo "==> this is a worktree ($root)"
    echo "    launchers track the MAIN checkout ($main), not a throwaway worktree."
    echo "    Pointing them here is how ~/.local/bin/orch was left dangling when a worktree was removed."
    ;;
esac

[ "$dry_run" = "1" ] || mkdir -p "$launcher_dir"
for bin in "${bins[@]}"; do
  src="$target_root/bin/$bin"
  if [ ! -f "$src" ]; then
    echo "==> skip $bin: $src does not exist (run 'make build' in $target_root first)"
    continue
  fi
  if [ "$dry_run" = "1" ]; then
    echo "==> would link $launcher_dir/$bin -> $src"
    continue
  fi
  ln -sfn "$src" "$launcher_dir/$bin"
  echo "==> $launcher_dir/$bin -> $src"
done
