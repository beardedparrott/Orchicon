#!/usr/bin/env bash
# ============================================================================
# release-notes-digest/run.sh — every README digest bullet must be a sentence.
#
# WHY THIS EXISTS. `gen-release-notes.sh --sync-readme` writes README's "Last
# Release Changes" section, which is the front page of the repository. The
# source prose in RELEASE_HIGHLIGHTS.md is hard-wrapped, and the renderer used
# to take only the FIRST LINE of each paragraph:
#
#   - **a desktop app, for all three platforms**: … and it ships signed and
#
# A bullet cut mid-clause reads as a truncation bug to every visitor, and it is
# silent — the script exits 0 and the section is written. Two separate cases had
# the same cause: a "### New:" bullet, whose first sentence spans lines, and an
# "Also in this release" bullet, whose continuation lines are indented. Both are
# asserted here from the OUTPUT, not the mechanism, so any future renderer that
# preserves the bullet is fine.
#
# Deliberately dependency-free: awk/sed only, no network, no git history.
# ============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$REPO_ROOT"

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

[ -f scripts/gen-release-notes.sh ] || fail "missing scripts/gen-release-notes.sh"
[ -f RELEASE_HIGHLIGHTS.md ] || fail "missing RELEASE_HIGHLIGHTS.md"

# Render the digest against a THROWAWAY REPO, not the checkout. The generator
# resolves REPO_ROOT from its own path and `cd`s there, so invoking the real
# script with a scratch CWD would re-sync the TRACKED README while this test
# read a stale copy -- it passed against a broken renderer that way.
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
mkdir -p "$scratch/scripts"
cp scripts/gen-release-notes.sh "$scratch/scripts/"
cp README.md RELEASE_HIGHLIGHTS.md "$scratch"/
git -C "$scratch" init -q 2>/dev/null || true
( cd "$scratch" && bash scripts/gen-release-notes.sh --sync-readme >/dev/null )

block="$(awk '/^## Last Release Changes/{f=1;next} /^## /{if(f)exit} f' "$scratch/README.md")"
[ -n "$block" ] || fail "README has no 'Last Release Changes' block after sync"

bullets="$(printf '%s\n' "$block" | grep -E '^- ' || true)"
count="$(printf '%s\n' "$bullets" | grep -cE '^- ' || true)"
[ "$count" -gt 0 ] || fail "digest produced no bullets"

# A complete bullet ends in terminal punctuation. A trailing comma, "and",
# "the", or a bare word is the signature of a mid-clause cut.
bad="$(printf '%s\n' "$bullets" | grep -vE '[.!?]"?$' || true)"
if [ -n "$bad" ]; then
  printf 'FAIL: %s digest bullet(s) end mid-sentence:\n' "$(printf '%s\n' "$bad" | wc -l)" >&2
  printf '%s\n' "$bad" | sed 's/^/  /' >&2
  exit 1
fi

# The "### New:" bullets are the named features and must carry a real sentence,
# not just a heading fragment. Assert a floor rather than an exact length, so
# the check survives a future rewrap of the source.
short="$(printf '%s\n' "$bullets" | awk 'length($0) < 60' || true)"
if [ -n "$short" ]; then
  printf 'FAIL: digest bullet(s) too short to be a sentence:\n' >&2
  printf '%s\n' "$short" | sed 's/^/  /' >&2
  exit 1
fi

printf 'ok: %s digest bullets, all complete sentences\n' "$count"
