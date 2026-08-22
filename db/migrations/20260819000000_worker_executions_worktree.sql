-- Worker executions: working-tree provenance for each execution.
--
-- The run-level worktree (workflow_runs.worktree_*, PR #305) is the source
-- of truth; the execution-level columns are written at dispatch so the
-- pruning task and UI can resolve which worktree an execution ran in (and
-- what state it reached) without a join. Nullable: existing rows predate
-- worktrees (backfill null); a null status means the execution is not
-- associated with a worktree. Same vocabulary as workflow_runs
-- (pending/ready/skipped/failed; pruned reserved for the pruning task).
-- Additive and forward-only; RLS untouched (existing tenant_isolation
-- policy already scopes the row).
ALTER TABLE worker_executions
  ADD COLUMN IF NOT EXISTS worktree_status text,
  ADD COLUMN IF NOT EXISTS worktree_path  text,
  ADD COLUMN IF NOT EXISTS worktree_branch text;

COMMENT ON COLUMN worker_executions.worktree_status IS 'Worktree provisioning state copied from the run at dispatch (pending/ready/skipped/failed; pruned by the pruning task). NULL = execution not associated with a worktree.';
COMMENT ON COLUMN worker_executions.worktree_path IS 'Absolute path of the isolated working tree the execution ran in.';
COMMENT ON COLUMN worker_executions.worktree_branch IS 'Deterministic branch created for the run.';
