#!/usr/bin/env bash
# ============================================================================
# release.sh — cut a release, deliberately, from main.
#
# WHY THIS REPLACED auto-release.yml. Releasing used to be a SIDE EFFECT of merging
# a labelled PR: `auto-release.yml` fired on `pull_request_target: closed`, computed
# the next version from the label, created the tag and dispatched `release.yml`. That
# meant the version number was decided by a label at merge time, and the release
# itself began before anyone could look at the merged tree.
#
# It is now an explicit act. Nothing publishes until you run this, from `main`, on a
# tree you have looked at.
#
# USAGE
#   scripts/release.sh patch          # v0.6.1 -> v0.6.2
#   scripts/release.sh minor          # v0.6.1 -> v0.7.0
#   scripts/release.sh major          # v0.6.1 -> v1.0.0
#   scripts/release.sh v0.7.0         # an explicit tag
#   scripts/release.sh --dry-run patch
#
# WHAT IT DOES, IN ORDER, AND WHY EACH STEP IS THERE
#   1. Refuses to run anywhere but `main`, on a clean tree, up to date with origin.
#   2. Computes the next version from the highest released tag — from PUBLISHED
#      GitHub releases, not from every tag, so develop's bump tags cannot inflate it.
#   3. Requires RELEASE_HIGHLIGHTS.md to have a `## <version>` section, and says so
#      before tagging rather than publishing fallback notes.
#   4. Creates the annotated tag on HEAD and pushes it.
#   5. Dispatches release.yml with that version.
#
# It does NOT merge anything. `develop` -> `main` is yours to open and merge first;
# this takes a merged `main` and publishes it.
#
# READ THIS BEFORE OPENING THE RELEASE PR — IT IS THE MOST EXPENSIVE MISTAKE HERE.
#
# `develop-bump.yml` mints a `vX.Y.Z` tag on EVERY merge to `develop`, including
# documentation-only ones. So the PR that writes `## v0.6.21` into
# RELEASE_HIGHLIGHTS.md CONSUMES v0.6.21 the moment it merges, and step 3 then fails
# with "no section for v0.6.21" for a version that is now a bump tag — a guaranteed
# round trip through develop and main, every release.
#
# The fix is one word in the PR TITLE: put `[NoDevBump]` at the FRONT of it. The
# merge then rides the current tag instead of minting one. Do it on the highlights
# PR and on the develop -> main release PR.
#
#   [NoDevBump] docs(release): v0.6.21 highlights
#   [NoDevBump] Release v0.6.21: merge develop into main
#
# A bump argument now SKIPS every tag already in use (bump tags are not releases), so
# `patch` lands on the next genuinely free number and prints what it skipped. An
# explicit vX.Y.Z is still refused when taken — you named it, so it is never
# silently renumbered.
# ============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

die()  { printf '\033[31merror\033[0m %s\n' "$1" >&2; exit 1; }
ok()   { printf '\033[32m  ok\033[0m %s\n' "$1"; }
info() { printf '  %s\n' "$1"; }
warn() { printf '\033[33mwarn\033[0m %s\n' "$1" >&2; }

DRY_RUN=0
if [ "${1:-}" = "--dry-run" ]; then DRY_RUN=1; shift; fi
BUMP="${1:-}"
[ -n "$BUMP" ] || die "usage: scripts/release.sh [--dry-run] patch|minor|major|vX.Y.Z"

# --- 1. the tree we are publishing ------------------------------------------
BRANCH="$(git rev-parse --abbrev-ref HEAD)"
[ "$BRANCH" = "main" ] || die "releases are cut from 'main', not '$BRANCH'.
  Merge develop into main first, then re-run from main."
ok "on main"

[ -z "$(git status --porcelain)" ] || die "the working tree is dirty — commit or stash before releasing.
  (A release must be reproducible from the tag alone.)"
ok "tree clean"

git fetch --quiet --tags --force origin
if [ "$(git rev-parse HEAD)" != "$(git rev-parse origin/main)" ]; then
  die "main is not up to date with origin/main — pull first.
  local:  $(git rev-parse --short HEAD)
  origin: $(git rev-parse --short origin/main)"
fi
ok "up to date with origin/main"

# --- 2. the version, from PUBLISHED releases --------------------------------
# `git tag` alone would be wrong: develop-bump tags every merge, and a stray bump tag
# must never decide what the next RELEASE is called. The published releases are the
# only honest anchor.
command -v gh >/dev/null 2>&1 || die "gh is required (it reads published releases and dispatches the workflow)"

# THE ANCHOR MUST BE A PUBLISHED VERSION, and this query used to take whatever came first.
#
# `gh release list --limit 1` returns the newest release of ANY kind, and this repo publishes a
# `canary` PRE-RELEASE that re-points at develop's tip on every merge. Once canary is the newest
# entry it becomes the anchor, and the arithmetic below parses "canary" as a version: MAJ="canary",
# MIN="", PAT="" — so a patch bump computes vcanary.1.1 and every bump mode dies on a tag name
# nobody can type. The release then cannot be cut at all, from a clean main, for a reason that has
# nothing to do with the code being released.
#
# This is the same defect class the Makefile's `version-tag-match` gate exists to prevent for
# `git describe` ("canary is a real tag and it MOVES … a bare `git describe --tags` can resolve a
# local rebuild's version to 'canary'"). Releases are decided by PUBLISHED versions, so the query
# asks for exactly that: exclude pre-releases, and exclude the moving tags by name in case one is
# ever published as a full release.
LATEST="$(gh release list --limit 100 --json tagName,isPrerelease,isDraft \
  --jq '[.[] | select((.isPrerelease|not) and (.isDraft|not)) | .tagName
         | select(test("^v[0-9]+\\.[0-9]+\\.[0-9]+$"))
         | select(. != "canary")] | .[0] // empty' 2>/dev/null || true)"
if [ -z "$LATEST" ]; then
  warn "no published release found; anchoring on the highest tag instead"
  LATEST="$(git tag --list 'v*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1 || true)"
fi
[ -n "$LATEST" ] || die "no version tag to bump from — create the first release with an explicit tag, e.g. scripts/release.sh v0.1.0"

# AND THE ANCHOR MUST PARSE. Guarding here rather than trusting the query means any future anchor
# defect fails with a sentence that names the culprit, instead of minting "vcanary.1.1" and dying on
# a tag nobody can type.
if ! printf '%s' "$LATEST" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
  die "the latest published release ('$LATEST') is not a version tag, so the next version cannot be
  computed from it. A pre-release or a moving tag has become the anchor.
  Pass an explicit version instead:  scripts/release.sh vX.Y.Z"
fi

# A BUMP SKIPS EVERY TAG ALREADY IN USE, and that is the whole point of this block.
#
# The anchor above is the newest PUBLISHED release, but `develop-bump.yml` mints a
# `vX.Y.Z` tag on EVERY merge to develop. So the arithmetic below almost always lands on
# a number that already exists as a bump tag — `patch` from published v0.6.10 computes
# v0.6.11 while v0.6.11..v0.6.17 are already on origin — and the old code then DIED,
# telling the operator to "pick another version". That turned a mechanical step into a
# manual one, every single release, and the numbers it told you to pick had to be found
# by hand with `git tag --list | sort -V | tail`.
#
# The fix is to keep bumping until the name is free. A bump means "the next released
# version", and a bump tag is not a release — it is a marker that the tree advanced. So
# skipping taken numbers is the CORRECT reading of `patch`, not a workaround. An EXPLICIT
# vX.Y.Z is still honoured exactly and still refused when taken, because there the
# operator named a specific version and silently publishing under a different one would
# be worse than failing.
tag_taken() { git rev-parse -q --verify "refs/tags/$1" >/dev/null 2>&1; }

case "$BUMP" in
  patch|minor|major)
    MAJ="$(echo "${LATEST#v}" | cut -d. -f1)"
    MIN="$(echo "${LATEST#v}" | cut -d. -f2)"
    PAT="$(echo "${LATEST#v}" | cut -d. -f3)"
    skipped=""
    # Bounded: a runaway loop here would be an infinite tag walk, not a release.
    for _ in $(seq 1 200); do
      case "$BUMP" in
        patch) CAND="v${MAJ}.${MIN}.$((PAT + 1))" ;;
        minor) CAND="v${MAJ}.$((MIN + 1)).0" ;;
        major) CAND="v$((MAJ + 1)).0.0" ;;
      esac
      if ! tag_taken "$CAND"; then NEW="$CAND"; break; fi
      skipped="${skipped}${CAND} "
      # Advance the anchor and re-derive, so minor/major step by the right amount
      # (a taken v0.6.5 must push `minor` to v0.7.0, not v0.7.1).
      V="${CAND#v}"
      MAJ="$(echo "$V" | cut -d. -f1)"
      MIN="$(echo "$V" | cut -d. -f2)"
      PAT="$(echo "$V" | cut -d. -f3)"
    done
    [ -n "${NEW:-}" ] || die "could not find a free tag by bumping from ${LATEST} — 200 candidates were all taken"
    if [ -n "$skipped" ]; then
      info "skipped bump tags already in use: ${skipped% }"
    fi
    ;;
  v[0-9]*.[0-9]*.[0-9]*)
    NEW="$BUMP"
    if tag_taken "$NEW"; then
      die "tag ${NEW} already exists — you named this version explicitly, so it will not be
  renumbered silently. Either delete the tag if it is a mistake, or pick the next free one:
    git tag --list 'v*' | grep -E '^v[0-9]+\\.[0-9]+\\.[0-9]+$' | sort -V | tail -1"
    fi
    ;;
  *)
    die "'$BUMP' is neither a bump (patch|minor|major) nor a version (vX.Y.Z)"
    ;;
esac

info "latest release: ${LATEST}"
info "next version:   ${NEW}"

# --- 3. the notes must already be written -----------------------------------
# The release body is RELEASE_HIGHLIGHTS.md's `## <version>` section. Creating the tag
# first and discovering the notes are missing afterwards publishes fallback text.
if ! grep -qE "^## ${NEW}$" RELEASE_HIGHLIGHTS.md 2>/dev/null; then
  # THE MESSAGE NAMES THE TRAP, because this is where it is actually encountered. The
  # usual cause is not a forgotten section: it is a section written for a number that a
  # bump tag then consumed, leaving the heading one release behind.
  #
  # (There is deliberately no "did you mean the next tag up?" auto-correction here. NEW
  # is guaranteed free by the bump logic above, so any silently-picked alternative would
  # be a DIFFERENT version than the operator asked to publish — and a release number is
  # not something to guess on their behalf.)
  die "RELEASE_HIGHLIGHTS.md has no '## ${NEW}' section.

  Add it ABOVE the previous release's section (the file is newest-first), then re-run.
  The release body is generated from it; without the section the release publishes a
  fallback and prints a warning nobody reads.

  If you ALREADY wrote this section, check which number it carries:
    grep -nE '^## v[0-9]' RELEASE_HIGHLIGHTS.md | head -3
  A heading one release behind means the PR that added it minted a bump tag for the very
  number it named. Renumber the heading to ${NEW} and re-merge with [NoDevBump] at the
  FRONT of the PR title, so that merge rides the current tag instead of consuming another."
fi
ok "RELEASE_HIGHLIGHTS.md has a ${NEW} section"

if [ "$DRY_RUN" = "1" ]; then
  echo
  info "dry run — would now:"
  info "  git tag -a ${NEW} -m '...'"
  info "  git push origin ${NEW}"
  info "  gh workflow run release.yml --ref main --field version=${NEW}"
  exit 0
fi

# --- 4. tag it ---------------------------------------------------------------
git tag -a "$NEW" -m "${NEW}" || die "could not create tag ${NEW}"
if ! git push origin "$NEW"; then
  git tag -d "$NEW" >/dev/null 2>&1 || true
  die "could not push tag ${NEW} — the local tag was removed so the next run is clean"
fi
ok "tagged ${NEW} at $(git rev-parse --short HEAD)"

# --- 5. publish --------------------------------------------------------------
# The tag already exists, so release.yml's own "tag must exist on origin" check passes,
# and its guard accepts a workflow_dispatch from main.
if ! gh workflow run release.yml --ref main --field version="$NEW"; then
  die "the tag ${NEW} exists but the release workflow could not be dispatched.
  Run it by hand:  gh workflow run release.yml --ref main --field version=${NEW}"
fi
ok "dispatched release.yml for ${NEW}"

echo
info "Watch it:      gh run list --workflow=release.yml --limit 1"
info "Then verify:   gh release view ${NEW}"
info "Expect 13 assets. The MSI's orchicon.exe must report ${NEW#v}, not 'dev'."
