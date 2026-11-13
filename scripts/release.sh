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

LATEST="$(gh release list --limit 1 --json tagName --jq '.[0].tagName // empty' 2>/dev/null || true)"
if [ -z "$LATEST" ]; then
  warn "no published release found; anchoring on the highest tag instead"
  LATEST="$(git tag --list 'v*' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1 || true)"
fi
[ -n "$LATEST" ] || die "no version tag to bump from — create the first release with an explicit tag, e.g. scripts/release.sh v0.1.0"

case "$BUMP" in
  patch|minor|major)
    MAJ="$(echo "${LATEST#v}" | cut -d. -f1)"
    MIN="$(echo "${LATEST#v}" | cut -d. -f2)"
    PAT="$(echo "${LATEST#v}" | cut -d. -f3)"
    case "$BUMP" in
      patch) NEW="v${MAJ}.${MIN}.$((PAT + 1))" ;;
      minor) NEW="v${MAJ}.$((MIN + 1)).0" ;;
      major) NEW="v$((MAJ + 1)).0.0" ;;
    esac
    ;;
  v[0-9]*.[0-9]*.[0-9]*)
    NEW="$BUMP"
    ;;
  *)
    die "'$BUMP' is neither a bump (patch|minor|major) nor a version (vX.Y.Z)"
    ;;
esac

info "latest release: ${LATEST}"
info "next version:   ${NEW}"

if git rev-parse -q --verify "refs/tags/${NEW}" >/dev/null 2>&1; then
  die "tag ${NEW} already exists — pick another version, or delete the tag if it is a mistake.
  Re-publishing over an existing tag is how a release ends up pointing at the wrong commit."
fi

# --- 3. the notes must already be written -----------------------------------
# The release body is RELEASE_HIGHLIGHTS.md's `## <version>` section. Creating the tag
# first and discovering the notes are missing afterwards publishes fallback text.
if ! grep -qE "^## ${NEW}$" RELEASE_HIGHLIGHTS.md 2>/dev/null; then
  die "RELEASE_HIGHLIGHTS.md has no '## ${NEW}' section.

  Add it ABOVE the previous release's section (the file is newest-first), then re-run.
  The release body is generated from it; without the section the release publishes a
  fallback and prints a warning nobody reads."
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
