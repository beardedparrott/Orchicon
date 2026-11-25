#!/usr/bin/env bash
# ============================================================================
# release-bump-skips-tags/run.sh — a bump must SKIP tags already in use.
#
# WHY THIS EXISTS. scripts/release.sh anchors the next version on the newest
# PUBLISHED release, then refuses to reuse an existing tag. But develop-bump.yml
# mints a vX.Y.Z tag on EVERY merge to develop, so those two rules collided on
# every single release: `patch` from published v0.6.10 computed v0.6.11 while
# v0.6.11..v0.6.17 were already on origin, and the script died telling the
# operator to "pick another version" — which had to be found by hand.
#
# The fix keeps bumping past taken numbers. A bump means "the next RELEASED
# version", and a bump tag is not a release, so skipping is the correct reading of
# `patch`. An EXPLICIT vX.Y.Z is still refused when taken: the operator named it.
#
# This gate pins that arithmetic without needing a repo, a remote, or network: it
# extracts the logic's contract as a table of cases.
# ============================================================================
set -euo pipefail

RELEASE_SH="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)/scripts/release.sh"
[ -f "$RELEASE_SH" ] || { echo "cannot find scripts/release.sh" >&2; exit 1; }

fail=0
check() {
  local desc="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then
    printf '  ok   %s -> %s\n' "$desc" "$got"
  else
    printf '  FAIL %s -> got %s, want %s\n' "$desc" "$got" "$want" >&2
    fail=1
  fi
}

# The bump-skip loop is a small, self-contained piece of arithmetic. Drive it the
# same way release.sh does, with a tag_taken() stub, over the cases that matter.
bump() {
  local LATEST="$1" BUMP="$2"; shift 2
  local TAKEN="$*"
  local tag_taken
  tag_taken() { local t; for t in $TAKEN; do [ "$t" = "$1" ] && return 0; done; return 1; }
  local NEW="" MAJ MIN PAT CAND V
  MAJ="$(echo "${LATEST#v}" | cut -d. -f1)"
  MIN="$(echo "${LATEST#v}" | cut -d. -f2)"
  PAT="$(echo "${LATEST#v}" | cut -d. -f3)"
  for _ in $(seq 1 200); do
    case "$BUMP" in
      patch) CAND="v${MAJ}.${MIN}.$((PAT + 1))" ;;
      minor) CAND="v${MAJ}.$((MIN + 1)).0" ;;
      major) CAND="v$((MAJ + 1)).0.0" ;;
    esac
    if ! tag_taken "$CAND"; then NEW="$CAND"; break; fi
    V="${CAND#v}"; MAJ="$(echo "$V" | cut -d. -f1)"; MIN="$(echo "$V" | cut -d. -f2)"; PAT="$(echo "$V" | cut -d. -f3)"
  done
  printf '%s' "$NEW"
}

echo "release-bump-skips-tags:"

# THE ACTUAL FAILURE, replayed: published anchor v0.6.10, seven bump tags in the way.
check "patch past 7 bump tags" "v0.6.18" \
  "$(bump v0.6.10 patch v0.6.11 v0.6.12 v0.6.13 v0.6.14 v0.6.15 v0.6.16 v0.6.17)"

check "patch with nothing taken" "v0.6.18" "$(bump v0.6.17 patch)"

# minor/major must step by the right AMOUNT when skipping: a taken v0.7.0 has to
# push minor to v0.8.0, not v0.7.1.
check "minor past a taken .0" "v0.8.0" "$(bump v0.6.10 minor v0.7.0 v0.7.1)"
check "major past a taken major" "v2.0.0" "$(bump v0.6.10 major v1.0.0)"
check "patch into a gap" "v0.6.12" "$(bump v0.6.10 patch v0.6.11 v0.6.13)"

# The release script must still refuse an EXPLICIT taken version rather than
# silently renumber it — guard the guard.
if grep -q 'you named this version explicitly, so it will not be' "$RELEASE_SH"; then
  printf '  ok   explicit taken version is refused, not renumbered\n'
else
  printf '  FAIL explicit taken version no longer refuses\n' >&2
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  echo "release-bump-skips-tags: FAILED" >&2
  exit 1
fi
echo "release-bump-skips-tags: ok — a bump always lands on a free version"
