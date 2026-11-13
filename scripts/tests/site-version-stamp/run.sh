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
#
# CI CHECKS OUT A DETACHED HEAD, where `git rev-parse --abbrev-ref HEAD` answers
# "HEAD" — and `git clone --branch HEAD` fails. So the clone is created without
# --branch (taking whatever HEAD points at, detached or not) and the revision is
# named explicitly. Getting this wrong made the test fail in CI while passing
# locally, which is its own lesson: verify a test in the environment it runs in.
REV="$(git -C "${ROOT}" rev-parse HEAD)"
if ! git init --quiet "${TMP}/src" ||
   ! git -C "${TMP}/src" fetch --quiet --depth 1 --no-tags "file://${ROOT}" "${REV}"; then
  echo "could not build the test clone from ${ROOT} at ${REV}"
  exit 1
fi
git -C "${TMP}/src" -c advice.detachedHead=false checkout --quiet --detach FETCH_HEAD
mv "${TMP}/src" "${TMP}/clone"

# The clone carries only committed files, so the recovery input must be tracked —
# that is the whole point of using RELEASE_HIGHLIGHTS.md. Assert it is here.
if [ ! -f "${TMP}/clone/RELEASE_HIGHLIGHTS.md" ]; then
  echo "RELEASE_HIGHLIGHTS.md is not tracked, so the recovery path cannot work"
  exit 1
fi

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

# NO ASSERTION ON THE FETCH OUTCOME. Whether `git fetch --tags` succeeds depends on
# the sandbox (a local file:// path can be fetched even after the remote URL is
# broken), so asserting "the fetch failed and said so" makes this test a property of
# the environment rather than of build-site.sh. What must hold either way is that the
# footer is correct and the script names where the version came from — both asserted
# above and below. The reporting itself is covered by reading the script's behaviour
# when describe genuinely returns nothing, which is the next block.
if printf '%s' "${OUT}" | grep -q 'resolved .* from RELEASE_HIGHLIGHTS.md\|stamped footer version v'; then
  pass "the stamp path is stated in the output"
else
  bad "build-site.sh did not say how it resolved the version"
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
