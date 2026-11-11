#!/usr/bin/env bash
# gotmpdir.sh — choose GOTMPDIR so `t.TempDir()` never lands inside the working repository.
#
# git DISCOVERS THE ENCLOSING REPO FROM ANY SUBDIRECTORY, so t.TempDir() under
# <repo>/.dev/tools/gotmp (what the Makefile used to export) is INSIDE the repo. Tests that build a
# throwaway git fixture then register worktrees, scratch checkouts and branches against the OPERATOR'S
# REAL REPOSITORY, and the async baseline capture keeps writing into the temp dir after the test
# returns. MEASURED: 5-6 failures in internal/scheduler per run with GOTMPDIR inside the repo, 0 with
# it outside, same commit, twice each.
#
# So the temp dir is placed outside the repository. An explicitly exported GOTMPDIR always wins.
set -eu

# `git rev-parse --show-toplevel` is the repo root for BOTH a main checkout and a linked worktree, and
# prints nothing outside a repo — the one question that matters here, asked of git rather than of the
# path's spelling.
root="${1:-$PWD}"
if [ -n "${GOTMPDIR:-}" ]; then
  printf '%s\n' "$GOTMPDIR"
  exit 0
fi

if ! repo="$(git -C "$root" rev-parse --show-toplevel 2>/dev/null)"; then
  # Not a repository: the in-project location is already outside one.
  printf '%s\n' "$root/.dev/tools/gotmp"
  exit 0
fi

# KEYED BY THE CHECKOUT so two checkouts of the same repo never share a temp dir, and a worktree gets
# its own rather than one named after a path git would read as that worktree.
base="${TMPDIR:-/tmp}"
key="$(printf '%s' "$repo" | cksum | cut -d' ' -f1)"
printf '%s\n' "$base/orchicon-gotmp-$key"
