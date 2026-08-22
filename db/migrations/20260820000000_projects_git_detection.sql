-- Projects: cached git-work-tree detection for the WorktreeReconciler.
--
-- The reconciler shells out to `git rev-parse --is-inside-work-tree` once at
-- reconcile time and caches the result on the project row so the control loop
-- never repeats the subprocess on every pass (the non-repo in-place fallback
-- work item).
--
--   git_work_tree:  whether project_dir is (was, at last detection) a git work
--                   tree. The cached decision; false = non-repo → run proceeds
--                   in place.
--   git_detected_at: when the cached value was written. NULL = never detected
--                   (undetermined) → the reconciler must detect. Used to
--                   TTL-refresh so a directory that becomes a repo is picked
--                   up without a project_dir change, and reset to NULL when
--                   project_dir changes.
--
-- Additive and forward-only; RLS untouched (the existing tenant_isolation
-- policy already scopes the row).
ALTER TABLE projects
  ADD COLUMN IF NOT EXISTS git_work_tree boolean NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS git_detected_at timestamptz;
