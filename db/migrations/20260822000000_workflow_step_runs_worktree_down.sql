ALTER TABLE workflow_step_runs
  DROP COLUMN IF EXISTS worktree_status,
  DROP COLUMN IF EXISTS worktree_path,
  DROP COLUMN IF EXISTS worktree_branch;
