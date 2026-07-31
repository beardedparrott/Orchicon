#!/bin/bash
# Orchicon safety lint — runs Semgrep (the industry-standard static
# analyzer) with Orchicon's safety ruleset + Semgrep's public
# security-audit rules. PR Reviewer and QA Engineer workers run this
# before reporting; humans can run it too.
#
# Usage: lint-safety.sh [directory]   (defaults to the current directory)
# Exit status: 0 = clean, 1 = findings, 2 = tooling error (semgrep missing).
#
# The linter is a CHECKER, not an auto-blocker: it reports findings for
# the reviewer to judge. Reviewers report only findings that are genuine
# and relevant to the change — the linter exists so you don't have to
# manually hunt every issue, not so you enumerate every hit.

set -u

TARGET="${1:-.}"

if ! command -v semgrep >/dev/null 2>&1; then
  echo "lint-safety.sh: semgrep is not installed." >&2
  echo "  Install it with: pip install --user semgrep   (or via your package manager)" >&2
  exit 2
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RULESET="$SCRIPT_DIR/semgrep_orchicon.yml"
if [ ! -f "$RULESET" ]; then
  echo "lint-safety.sh: ruleset not found at $RULESET" >&2
  exit 2
fi

# Dependency/build dirs are never scanned.
EXCLUDES=(
  --exclude=.git
  --exclude=.orchicon
  --exclude=node_modules
  --exclude=vendor
  --exclude=dist
  --exclude=build
  --exclude=target
  --exclude=__pycache__
  --exclude=.venv
  --exclude=venv
  --exclude=.tox
  --exclude=.next
  --exclude=coverage
)

# Run with the Orchicon ruleset + public security rules. --error makes
# semgrep exit 1 on findings (exit 2 = tooling error). If the public
# ruleset can't be fetched (offline), retry with the local rules only.
semgrep scan "${EXCLUDES[@]}" \
  --error \
  --config "$RULESET" \
  --config p/security-audit \
  --quiet "$TARGET"
rc=$?

if [ "$rc" -eq 2 ]; then
  echo "lint-safety.sh: public rulesets unavailable (offline?), using local Orchicon rules only" >&2
  semgrep scan "${EXCLUDES[@]}" --error --config "$RULESET" --quiet "$TARGET"
  rc=$?
fi

if [ "$rc" -eq 0 ]; then
  echo "OK: Semgrep found no issues in $TARGET"
fi
exit "$rc"
