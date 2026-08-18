-- Workflow step runs: per-step-run isolated working tree state.
--
-- Parallel-branch children (steps whose depends_on references a `parallel`
-- step) execute in their OWN worktree — one per step run — so independent
-- branches never share a filesystem or a .orchicon/ metadata dir
-- (architecture-notes/concurrent-step-run-dispatch.md D2). The
-- WorktreeReconciler provisions these at branch dispatch time and records
-- the result on the step run row, mirroring the run-level columns on
-- workflow_runs (worktree_status/path/branch).
--
--   worktree_status: pending (not yet provisioned) → ready (worktree exists,
--                    path+branch recorded) | skipped (non-repo project — the
--                    branch runs in the run's cwd) | failed (provisioning
--                    error) | pruned (reaped at step-run terminal).
--   worktree_path:   absolute path of the isolated working tree
--                    (<project_dir>/.orchicon-worktrees/<runID>/<stepRunID>).
--   worktree_branch: the deterministic branch created for this step run
--                    (<runBranch>/<stepSlug>-<stepRunSuffix>).
--
-- The 'pending' default makes the scan self-healing for step runs created
-- before the columns existed. Additive and forward-only; RLS untouched
-- (existing tenant_isolation policy already scopes the row).
ALTER TABLE workflow_step_runs
  ADD COLUMN IF NOT EXISTS worktree_status text NOT NULL DEFAULT 'pending',
  ADD COLUMN IF NOT EXISTS worktree_path  text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS worktree_branch text NOT NULL DEFAULT '';
