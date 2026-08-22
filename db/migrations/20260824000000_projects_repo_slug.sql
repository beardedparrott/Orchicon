-- Projects: cached git origin for deterministic per-branch PR links.
--
-- The WorktreeReconciler already caches git work-tree detection on the
-- project row (git_work_tree/git_detected_at). When it detects a work
-- tree it also reads the remote origin and records it here as
-- "owner/repo", so the read path can synthesize a per-branch PR link
-- (https://github.com/{owner}/{repo}/pull/new/{branch}) without a provider
-- call. NULL = project not git-backed or origin unknown.
--
-- Additive and forward-only; RLS untouched (the existing tenant_isolation
-- policy already scopes the row).
ALTER TABLE projects
  ADD COLUMN IF NOT EXISTS repo_slug text;

COMMENT ON COLUMN projects.repo_slug IS 'Cached git origin owner/repo of project_dir; used for deterministic per-branch PR links.';
