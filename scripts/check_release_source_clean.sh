#!/usr/bin/env bash
# Release-source gate: a released binary must be reproducible from the revision it claims.
#
# THE DEFECT THIS CLOSES. A release was built from a directory with uncommitted changes, so Go stamped it
# vcs.modified=true and the binary at commit e897ecb does NOT reproduce: the revision in the version string
# names code the binary does not contain. Every consequence is quiet and expensive —
#
#   - bug reports cite a revision that lacks the code under test, so the first investigation step is misled;
#   - git bisect cannot work, because one of its endpoints is not a real build of that commit;
#   - `orchicon dev (commit=e897ecb …)` reads as authoritative while being false.
#
# A release is the one build where "which source produced this?" must have an exact answer, so this refuses
# rather than warns.
#
# TWO THINGS IT CHECKS, and the second is not pedantry:
#
#   1. CLEAN TREE — `git status --porcelain` must be empty. Untracked-but-ignored files are fine (build
#      scratch), which is why --porcelain and not `--porcelain --ignored`.
#   2. HEAD MATCHES THE DECLARED TAG when a tag is supplied. A clean tree at the WRONG COMMIT reproduces just
#      as falsely, and a release workflow runs after checkout, where a detached HEAD at the tag is normal but a
#      stale working branch is not.
#
# LOCAL USE IS EXPECTED TO BE STRICT TOO: `make release-src-check` before tagging is cheaper than finding out
# from a downloaded binary that it cannot be reproduced.
#
# Usage:
#   scripts/check_release_source_clean.sh [tag]
#     tag — optional. VERSION=1.2.3 or the tag name (v1.2.3 / 1.2.3 both accepted).
#   exit 0 = the tree and revision are release-safe
#   exit 1 = they are not, and the reason is printed
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if ! command -v git >/dev/null 2>&1; then
  echo "ERROR: git is required to prove a release's source (the whole point of this check)." >&2
  exit 1
fi
if ! git rev-parse --git-dir >/dev/null 2>&1; then
  echo "ERROR: not a git repository — a release cannot name the source it came from, so this refuses." >&2
  echo "       (If you genuinely build releases outside git, set ORCHICON_RELEASE_ALLOW_NO_GIT=1 and say why.)" >&2
  [ "${ORCHICON_RELEASE_ALLOW_NO_GIT:-}" = "1" ] && exit 0
  exit 1
fi

status=0

# --- 1. the working tree ------------------------------------------------------------------------------
dirty="$(git status --porcelain)"
if [ -n "$dirty" ]; then
  status=1
  echo "ERROR: the working tree has uncommitted changes, so a binary built here would stamp"
  echo "       vcs.modified=true and would not be reproducible from the revision it reports."
  echo
  printf '%s\n' "$dirty"
  echo
  echo "Fix: commit (or stash) these, then rebuild. A release is the one build where 'which source produced"
  echo "     this?' must have an exact answer."
fi

# --- 2. HEAD is the revision being released -----------------------------------------------------------
if [ "$#" -ge 1 ] && [ -n "${1:-}" ]; then
  want="$1"
  # Accept both spellings: a workflow passes VERSION=1.2.3, a human may pass v1.2.3.
  case "$want" in
    v*) tag="$want" ;;
    *)  tag="v$want" ;;
  esac
  if git rev-parse -q --verify "refs/tags/$tag" >/dev/null 2>&1; then
    head="$(git rev-parse HEAD)"
    tagged="$(git rev-parse "$tag^{commit}")"
    if [ "$head" != "$tagged" ]; then
      status=1
      echo "ERROR: HEAD ($(git rev-parse --short HEAD)) is not tag $tag ($(git rev-parse --short "$tagged"))."
      echo "       A clean tree at the wrong commit reproduces just as falsely as a dirty one."
    fi
  else
    status=1
    echo "ERROR: tag $tag does not exist, so the release cannot name the revision it came from."
    echo "       Create it (git tag -a $tag -m ...) before releasing."
  fi
fi

# --- the report --------------------------------------------------------------------------------------
head_short="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
if [ "$status" -eq 0 ]; then
  echo "ok: release source is clean at ${head_short}$([ -n "${1:-}" ] && echo " (tag $tag)")"
fi
exit "$status"
