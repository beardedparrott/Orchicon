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
# This is the only step between the source repo and the CloudFlare
# Pages deploy. Run by CF Pages on every push; safe to re-run locally:
#
#   bash scripts/build-site.sh && npx serve site
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

echo "build-site: copied install + install.ps1 into ${SITE_DIR}"
ls -la "${SITE_DIR}"
