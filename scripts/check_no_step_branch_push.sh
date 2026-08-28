#!/usr/bin/env bash
# check_no_step_branch_push.sh — CI guard that step-level branch worktrees
# (*-step-* and branchWorktreeName branches <runBranch>/<stepSlug>-<suffix>)
# are never pushed to origin. The WorktreeReconciler provisions them locally
# only; the run branch is the sole PR head.
set -euo pipefail
echo "Checking that no step-level branches were pushed to origin..."
# List remote branches that match the step pattern
if git ls-remote --heads origin 2>/dev/null | grep -E 'refs/heads/.*-step-' >/dev/null; then
  echo "ERROR: step-level branch found on origin (pattern *-step-*):"
  git ls-remote --heads origin | grep -E 'refs/heads/.*-step-'
  echo "Step branches (e.g. *-step-qa-*) must never be pushed. Only the run branch (<slug>-<suffix>) is the PR head."
  exit 1
fi
echo "OK: no step branches on origin."
# Also check for branchWorktreeName pattern: <runBranch>/<step>-
# These contain a slash after the run branch prefix.
if git ls-remote --heads origin 2>/dev/null | grep -E 'refs/heads/.+/.+-' >/dev/null; then
  # Allow legitimate slashes that are not step branches? All run branches are kebab
  # without slash, so any slash branch is suspicious. Filter to step-like.
  if git ls-remote --heads origin | grep -E 'refs/heads/.+/(step-|qa-|pr-)' >/dev/null; then
    echo "ERROR: branch-worktree sub-branch found on origin:"
    git ls-remote --heads origin | grep -E 'refs/heads/.+/(step-|qa-|pr-)'
    exit 1
  fi
fi
