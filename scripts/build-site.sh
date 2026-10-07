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
# it from the same `git describe --tags --abbrev=0` the binary uses (Makefile
# VERSION), so the site footer and `orchicon version` cannot disagree.
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
# The SAME resolution the binary uses, so the two agree by construction. The
# canonical tags are created on GitHub at merge time, and a CI clone may not
# have them yet — the Makefile documents the same hazard ("`git pull` does NOT
# fetch tags") — so a best-effort fetch precedes the resolve. Offline, the
# fetch fails harmlessly and a local tag list still resolves.
SITE_VERSION="$(git -C "${REPO_ROOT}" describe --tags --abbrev=0 2>/dev/null || true)"
if [ -z "${SITE_VERSION}" ]; then
  git -C "${REPO_ROOT}" fetch --tags --quiet 2>/dev/null || true
  SITE_VERSION="$(git -C "${REPO_ROOT}" describe --tags --abbrev=0 2>/dev/null || echo dev)"
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
