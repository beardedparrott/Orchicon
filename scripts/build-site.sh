#!/usr/bin/env bash
# ============================================================================
# build-site.sh — CloudFlare Pages build step for orchicon.dev
#
# Copies the canonical install scripts from scripts/ into the static
# site bundle (site/) so the deploy includes:
#   - site/index.html          (landing page)
#   - site/style.css           (landing page styles)
#   - site/install             (copy of scripts/install.sh)
#   - site/install.ps1         (copy of scripts/install.ps1)
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
