#!/usr/bin/env bash
# ============================================================================
# site-version-stamp/run.sh — the footer version must never silently become
# "dev" (or anything else untrue) in a CI clone.
#
# WHY THIS EXISTS. orchidicon.dev served a footer reading "dev" after v0.6.1
# shipped. The chain was entirely silent:
#
#   1. Cloudflare Pages clones with --no-tags, so `git describe` prints nothing
#      and exits non-zero.
#   2. The recovery `git fetch --tags` can fail there, and build-site.sh sent its
#      stderr to /dev/null.
#   3. With no tag and no visible error, the last resort was the literal `dev`.
#
# Every step "worked". Nothing failed, nothing warned, and the public front page
# advertised the wrong thing about what we had shipped.
#
# So this test builds the exact environment that broke it — a depth-1, no-tags
# clone whose remote is unreachable — runs build-site.sh, and asserts the footer
# is a real version. The version is recovered from RELEASE_HIGHLIGHTS.md, which
# is TRACKED, so a shallow clone always has it and no network is needed.
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
fail=0
pass() { printf '  \033[32mPASS\033[0m  %s\n' "$1"; }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$1"; fail=1; }

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

# A depth-1, NO-TAGS clone is what Pages actually gives us.
git clone --depth 1 --no-tags --branch "$(git -C "${ROOT}" rev-parse --abbrev-ref HEAD)" \
  --single-branch "file://${ROOT}" "${TMP}/clone" >/dev/null 2>&1 || {
    echo "could not create the test clone (needs a local repo with this branch)"
    exit 1
  }

cp -R "${ROOT}/scripts" "${TMP}/clone/" 2>/dev/null || true
cp "${ROOT}/RELEASE_HIGHLIGHTS.md" "${TMP}/clone/" 2>/dev/null || true

cd "${TMP}/clone"

if [ "$(git tag --list | wc -l | tr -d ' ')" != "0" ]; then
  echo "test setup wrong: the clone still has tags, so it does not reproduce the failure"
  exit 1
fi

# Break the remote so `git fetch --tags` fails exactly as it did on Pages.
git remote set-url origin "${TMP}/definitely-not-here" 2>/dev/null || true

OUT="$(bash scripts/build-site.sh 2>&1)"
STAMP="$(grep -o 'id="site-version">[^<]*' site/index.html 2>/dev/null | sed 's/.*>//')"

echo "stamped footer: '${STAMP}'"

case "${STAMP}" in
  ""|dev|v__VERSION__|*__VERSION__*)
    bad "footer stamped '${STAMP}' — a page must never publish an unresolved version"
    ;;
  v[0-9]*.[0-9]*.[0-9]*)
    pass "footer carries a real version (${STAMP}) with no tags available"
    ;;
  *)
    bad "footer stamped '${STAMP}', which is not a version"
    ;;
esac

# The failed fetch must be REPORTED, not swallowed — that silence is what let
# the wrong footer ship without anyone noticing.
if printf '%s' "${OUT}" | grep -q 'fetch --tags` failed'; then
  pass "the failed tag fetch is reported rather than hidden"
else
  bad "the failed tag fetch was not reported — it is being swallowed again"
fi

# And the recovery must name the file it used, so the provenance is auditable.
if printf '%s' "${OUT}" | grep -q 'RELEASE_HIGHLIGHTS.md'; then
  pass "the recovery path names its source"
else
  bad "the version came from somewhere unstated"
fi

echo
if [ "${fail}" = "0" ]; then
  echo "RESULT: all site-version-stamp assertions passed"
else
  echo "RESULT: site-version-stamp FAILED"
  exit 1
fi
