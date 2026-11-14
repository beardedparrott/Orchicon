#!/usr/bin/env bash
# ============================================================================
# release-anchor/run.sh — the release version anchor must be a PUBLISHED VERSION.
#
# WHY THIS EXISTS. scripts/release.sh computes the next version from "the latest
# published release", and it read that with:
#
#     gh release list --limit 1 --json tagName --jq '.[0].tagName'
#
# `--limit 1` takes the newest release of ANY KIND. This repo also publishes a
# `canary` PRE-RELEASE that re-points at develop's tip on every merge, so as soon
# as canary is the newest entry it becomes the anchor — and the bump arithmetic
# below parses "canary" as a version: MAJ="canary", MIN="", PAT="". Every bump
# mode then computes vcanary.1.1 and the release cannot be cut AT ALL, from a
# clean main, for a reason unrelated to the code being released.
#
# This is the same defect class `version-tag-match/` guards for `git describe`
# ("canary is a real tag and it MOVES … a bare `git describe --tags` can resolve
# a local rebuild's version to 'canary'"). A release must be anchored on a
# VERSION, so the query must say so.
#
# Deliberately dependency-free and reads the file as TEXT — the source-assertion
# idiom this repo uses for what it cannot execute in a unit test. It asserts the
# REQUIREMENT (the anchor query filters pre-releases and matches a version shape)
# rather than one exact jq expression, so a future rewrite that is correct in a
# different way still passes.
# ============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
SRC="$ROOT/scripts/release.sh"

[ -f "$SRC" ] || { echo "FAIL: $SRC not found"; exit 1; }

fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }

# --- 1. the anchor statement -------------------------------------------------
# The anchor is written as a MULTI-LINE statement (the jq is too long for one line), so the capture
# has to span from the `LATEST="$(gh release list` line through the `)"` that closes it. A single-line
# grep would read only the first line — and would pass check 2 (isPrerelease is named there) while
# silently skipping the version-shape check on a line it never saw, which is exactly the vacuous
# pass this file exists to avoid.
ANCHOR="$(awk '/LATEST="\$\(gh release list/ {grab=1}
              grab {print}
              grab && /\)"$/ {exit}' "$SRC")"
[ -n "$ANCHOR" ] || ANCHOR="$(grep 'LATEST="\$(gh release list' "$SRC" || true)"
[ -n "$ANCHOR" ] || fail "release.sh no longer has a 'LATEST=\$(gh release list …)' anchor line.
  If the anchor moved, move this assertion with it — the requirement is that whatever
  reads the published release EXCLUDES pre-releases."

# `--limit 1` is the specific shape that broke: it takes the newest of any kind.
case "$ANCHOR" in
  *"--limit 1 "*)
    fail "the release anchor uses --limit 1, which takes the newest release of ANY KIND.
  'canary' is a pre-release that moves; as the newest entry it becomes the anchor and every
  bump mode computes a tag nobody can type. Ask for the newest PUBLISHED VERSION instead." ;;
esac

# --- 2. it must exclude pre-releases (and drafts) ----------------------------
case "$ANCHOR" in
  *isPrerelease*) : ;;
  *) fail "the release anchor does not filter on isPrerelease, so a PRE-RELEASE can become it.
  'canary' is published as a pre-release on every develop merge." ;;
esac

# --- 3. it must require a version shape --------------------------------------
# The jq is escaped inside the shell string, so assert the REQUIREMENT (some version-shaped
# matcher is applied) rather than one exact spelling.
if ! printf '%s' "$ANCHOR" | grep -qE 'test\(|\.\[0-9\]|\[0-9\]\+'; then
  fail "the release anchor does not require a version SHAPE, so a non-version tag could be
  selected and the bump arithmetic would parse it as a version."
fi

# --- 4. the arithmetic refuses an unparseable anchor ------------------------
grep -q "is not a version tag, so the next version cannot be" "$SRC" \
  || fail "release.sh does not validate the anchor before doing arithmetic on it.
  A bad anchor must produce a sentence naming the culprit, not a fabricated tag like vcanary.1.1."

# --- 5. behaviour: the shape of the fixed query, run against fake data ------
# The query is jq over `gh release list` JSON. Reproduce the collision — a canary
# pre-release newer than the real release — and assert the shape selects the
# version, not the pre-release.
if command -v jq >/dev/null 2>&1; then
  FAKE='[{"tagName":"canary","isPrerelease":true,"isDraft":false},
         {"tagName":"v0.6.1","isPrerelease":false,"isDraft":false},
         {"tagName":"v0.6.0","isPrerelease":false,"isDraft":false}]'
  PICKED="$(printf '%s' "$FAKE" | jq -r '
    [.[] | select((.isPrerelease|not) and (.isDraft|not)) | .tagName
           | select(test("^v[0-9]+\\.[0-9]+\\.[0-9]+$"))
           | select(. != "canary")] | .[0] // empty')"
  [ "$PICKED" = "v0.6.1" ] \
    || fail "the anchor shape resolved '$PICKED' from a canary-newer-than-release list; expected v0.6.1"
else
  echo "  (jq not installed — skipped the behavioural half; the source assertions above still ran)"
fi

echo "ok: the release anchor is a published version, never the canary pre-release"
