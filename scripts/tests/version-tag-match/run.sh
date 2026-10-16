#!/usr/bin/env bash
# ============================================================================
# version-tag-match/run.sh — no version resolution may accept a moving tag.
#
# WHY THIS EXISTS. `canary` is a real tag on this repo and it MOVES: the canary
# build re-points it at develop's tip on every merge. `git describe --tags`
# accepts ANY tag, and when several lightweight tags sit on one commit the
# tie-break is LEXICAL — so `canary` (c...) beats `v0.5.45` (v...) and a local
# rebuild reports its version as "canary".
#
# OBSERVED EXACTLY THAT: the running plane answered {"version":"canary"} and
# `orchicon version` printed "orchicon canary (commit=f5f41ec4)" for a tree
# whose content WAS v0.5.45. Releases were never affected — CI builds from a TAG
# checkout and takes the version from GITHUB_REF_NAME — which is exactly why it
# survived: only LOCAL rebuilds and the site/notes scripts ever resolved it via
# `git describe`.
#
# The invariant is simple and is asserted as one: every `git describe` that
# resolves a VERSION must restrict itself to version-shaped tags with
# `--match 'v[0-9]*'`. How it restricts them is the mechanism; that it does is
# the requirement.
#
# Deliberately dependency-free, and it reads the files as TEXT — the same source
# assertion idiom the frontend uses for what it cannot render
# (frontend/src/components/ui/mode-toggle-source.test.ts). It also asserts the
# BEHAVIOUR at the end, by reproducing the collision in a throwaway repo, so a
# future `git describe` that is correct here but wrong in principle still fails.
# ============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$REPO_ROOT"

fail=0
note() { printf '  %s\n' "$*"; }
ok()   { printf '   OK   %s\n' "$*"; }
bad()  { printf '   FAIL %s\n' "$*"; fail=1; }

# The files that resolve a version from git. A new one added here without the
# match is the failure mode this list exists to catch, so it is deliberately
# explicit rather than a repo-wide grep: a grep cannot tell a version resolve
# from an unrelated describe (a test fixture, a changelog count).
FILES=(
  Makefile
  scripts/container.sh
  scripts/gen-release-notes.sh
  scripts/build-site.sh
)

echo "== every version-resolution call restricts itself to version tags =="
# THE SCAN READS THE REAL LINES, and the first version of this did not: it piped
# `grep | sed` and split on the match, which broke every line containing a QUOTED
# path (`git -C "${REPO_ROOT}" describe ...`) — so build-site.sh's two call sites
# were silently skipped and the harness reported "OK" over a file it never read.
# A guard that cannot see a line is worse than no guard, so the line is read whole
# and the comment test is applied to it rather than to a mangled fragment.
for f in "${FILES[@]}"; do
  [ -f "$f" ] || { bad "$f is missing"; continue; }
  found=0
  while IFS= read -r line; do
    trimmed="${line#"${line%%[![:space:]]*}"}"
    case "$trimmed" in
      '#'*) continue ;;          # documentation, not code
    esac
    # `git describe` AND `git -C <dir> describe` are the same command; the
    # global-option form is what build-site.sh uses, and keying on the adjacent
    # pair missed it. Match the VERB instead.
    case "$trimmed" in
      *"git describe"*|*"git "*" describe"*|*" describe "*) ;;
      *) continue ;;
    esac
    found=$((found + 1))
    case "$trimmed" in
      *"--match 'v[0-9]*'"*|*'--match "v[0-9]*"'*) ok "$f: $trimmed" ;;
      *) bad "$f resolves a version without --match 'v[0-9]*': $trimmed" ;;
    esac
  done < "$f"
  if [ "$found" -eq 0 ]; then
    # A file in this list with NO describe is either a stale list or a moved call
    # site; both are worth failing on rather than passing over.
    bad "$f is listed as a version resolver but contains no 'git describe'"
  fi
done

echo
echo "== the collision is real, and the match defeats it =="
# Reproduce it rather than trusting the reasoning: two lightweight tags on ONE
# commit, one of them lexically after 'v'.
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
git -C "$tmp" init -q
git -C "$tmp" -c user.name=t -c user.email=t@t commit -q --allow-empty -m one
git -C "$tmp" tag canary
git -C "$tmp" tag v0.5.45
got_bare="$(git -C "$tmp" describe --tags --abbrev=0)"
got_match="$(git -C "$tmp" describe --tags --abbrev=0 --match 'v[0-9]*')"
note "bare describe on a shared commit resolves to: $got_bare"
note "with --match it resolves to:                $got_match"
if [ "$got_bare" != "canary" ]; then
  # If this ever changes, the harness's premise is wrong and it should say so
  # rather than pass quietly on a stale assumption.
  bad "expected the bare describe to collide on 'canary'; got '$got_bare' — this harness needs revisiting"
fi
if [ "$got_match" != "v0.5.45" ]; then
  bad "--match 'v[0-9]*' did not select the version tag (got '$got_match')"
fi

echo
if [ "$fail" -ne 0 ]; then
  echo "version-tag-match: FAILED"
  exit 1
fi
echo "version-tag-match: OK — no version resolution accepts a moving tag."
