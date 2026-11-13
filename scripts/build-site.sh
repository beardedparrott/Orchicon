#!/usr/bin/env bash
# ============================================================================
# build-site.sh — CloudFlare Pages build step for orchicon.dev
#
# Copies the canonical install scripts from scripts/ into the static
# site bundle (site/) so the deploy includes:
#   - site/index.html          (landing page — self-contained: inline CSS, inline SVG)
#   - site/assets/             (screenshots + marks referenced by index.html)
#   - site/install             (copy of scripts/install.sh)
#   - site/install.ps1         (copy of scripts/install.ps1)
#
# index.html and assets/ are COMMITTED; the two install scripts are
# GENERATED here (and gitignored) because the site must always serve the
# same installer the repo ships — a copy that drifted would tell operators
# to run a different install than the one under test.
#
# It also STAMPS THE FOOTER VERSION. The footer used to carry a literal
# ("Orchicon v0.4.0") that nothing ever updated, so the live site advertised a
# version five releases out of date. The committed page now carries a
# `v__VERSION__` placeholder in `<span id="site-version">` and this step fills
# it from the same `git describe --tags --abbrev=0 --match 'v[0-9]*'` the binary uses
# (Makefile VERSION), so the site footer and `orchicon version` cannot disagree —
# including when a moving prerelease tag (canary) shares the commit.
#
# This is the only step between the source repo and the CloudFlare
# Pages deploy. Run by CF Pages on every push; safe to re-run locally:
#
#   bash scripts/build-site.sh && npx serve site
#
# NOTE for a local run: the stamp rewrites the COMMITTED site/index.html, so a
# local build shows as a modified file. That is intentional — the stamp has to
# land in the file CloudFlare serves, and CF Pages builds from a fresh clone so
# it never accumulates. Re-running is idempotent, and `git checkout
# site/index.html` restores the placeholder.
#
# See CLOUDFLARE_SETUP.md and wrangler.toml.
# ============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SITE_DIR="${REPO_ROOT}/site"
SCRIPTS_DIR="${REPO_ROOT}/scripts"

[ -d "${SITE_DIR}" ]   || { echo "missing ${SITE_DIR}" >&2; exit 1; }
[ -d "${SCRIPTS_DIR}" ] || { echo "missing ${SCRIPTS_DIR}" >&2; exit 1; }

cp -f "${SCRIPTS_DIR}/install.sh"  "${SITE_DIR}/install"
cp -f "${SCRIPTS_DIR}/install.ps1" "${SITE_DIR}/install.ps1"

# --- the footer version stamp ----------------------------------------------
#
# THE FOOTER MUST NEVER FALL BACK TO SOMETHING MEANINGLESS. orchicon.dev served a
# footer reading "dev" after v0.6.1 shipped, because:
#
#   1. Cloudflare Pages clones with --no-tags, so `git describe` finds no tag and
#      prints nothing;
#   2. the recovery `git fetch --tags` below can fail in that environment, and its
#      stderr was sent to /dev/null — so the failure was invisible;
#   3. with no tag and no error, the last resort was a literal `dev`.
#
# A silent `dev` on the public front page is worse than a build failure: it looks
# like we do not know what we shipped. So the order is now: resolve from git; if
# that fails, recover the version from a file the clone DOES have (the release
# note heading in RELEASE_HIGHLIGHTS.md, which is tracked and shallow-safe); and
# only then fail the build loudly.
resolve_version() {
  local v
  v="$(git -C "${REPO_ROOT}" describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null || true)"
  if [ -n "${v}" ]; then printf '%s' "${v}"; return 0; fi

  # A no-tags CI clone cannot describe. Say so rather than swallowing it.
  if ! git -C "${REPO_ROOT}" fetch --tags --quiet; then
    echo "build-site: WARNING — \`git fetch --tags\` failed; no tags are available in this clone" >&2
  fi
  v="$(git -C "${REPO_ROOT}" describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null || true)"
  if [ -n "${v}" ]; then printf '%s' "${v}"; return 0; fi

  # LAST RESORT THAT IS STILL TRUE: the newest release heading in a TRACKED file.
  # RELEASE_HIGHLIGHTS.md's first `## vX.Y.Z` is the version this tree is for, and
  # reading it needs no tags, no history and no network.
  v="$(grep -m1 -oE '^## v[0-9]+\.[0-9]+\.[0-9]+' "${REPO_ROOT}/RELEASE_HIGHLIGHTS.md" 2>/dev/null | sed 's/^## //' || true)"
  if [ -n "${v}" ]; then
    echo "build-site: NOTE — resolved ${v} from RELEASE_HIGHLIGHTS.md (no tags in this clone)" >&2
    printf '%s' "${v}"; return 0
  fi
  return 1
}

if ! SITE_VERSION="$(resolve_version)"; then
  echo "build-site: ERROR — could not resolve a version for the footer." >&2
  echo "  No tag, tags cannot be fetched, and RELEASE_HIGHLIGHTS.md has no '## vX.Y.Z' heading." >&2
  echo "  Refusing to publish a page whose footer says nothing true about what this is." >&2
  exit 1
fi

# Rewrite whatever the span currently holds — the placeholder OR a version a
# previous run stamped — so this is idempotent ACROSS RELEASES. Matching only
# the placeholder would work exactly once and then silently stop updating,
# which is the same staleness this replaced.
STAMP_TMP="$(mktemp)"
if sed -E "s#(<span id=\"site-version\">)[^<]*(</span>)#\1${SITE_VERSION}\2#" \
     "${SITE_DIR}/index.html" > "${STAMP_TMP}"; then
  mv "${STAMP_TMP}" "${SITE_DIR}/index.html"
else
  rm -f "${STAMP_TMP}"
  echo "build-site: could not rewrite ${SITE_DIR}/index.html" >&2
  exit 1
fi

# A silent miss here would ship a page with "__VERSION__" in its footer, so it
# is reported. Not fatal: a footer version should never break a deploy.
if grep -q "id=\"site-version\">${SITE_VERSION}<" "${SITE_DIR}/index.html"; then
  echo "build-site: stamped footer version ${SITE_VERSION}"
else
  echo "build-site: WARNING — footer version NOT stamped (marker missing or changed?" \
       "expected <span id=\"site-version\">)" >&2
fi

echo "build-site: copied install + install.ps1 into ${SITE_DIR}"
ls -la "${SITE_DIR}"
