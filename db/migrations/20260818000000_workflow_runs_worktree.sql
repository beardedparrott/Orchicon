-- Workflow runs: per-run isolated working tree state.
--
-- The WorktreeReconciler provisions each git-backed run its own working
-- tree at arm time (`git worktree add <project_dir>/.orchicon-worktrees/<runID>
-- -b <branch> develop`) and records the result on the run row for downstream
-- consumers (execution-cwd wiring, cleanup, non-repo fallback).
--
--   worktree_status: pending (not yet provisioned) → ready (worktree exists,
--                    path+branch recorded) | skipped (non-repo project — the
--                    run proceeds in place) | failed (provisioning error).
--   worktree_path:   absolute path of the isolated working tree.
--   worktree_branch: the deterministic branch created for this run.
--
-- The 'pending' default makes the scan self-healing for runs armed before
-- the columns existed. Additive and forward-only; RLS untouched (existing
-- tenant_isolation policy already scopes the row).
ALTER TABLE workflow_runs
  ADD COLUMN IF NOT EXISTS worktree_status text NOT NULL DEFAULT 'pending',
  ADD COLUMN IF NOT EXISTS worktree_path  text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS worktree_branch text NOT NULL DEFAULT '';
