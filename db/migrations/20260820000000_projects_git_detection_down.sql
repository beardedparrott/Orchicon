ALTER TABLE projects
  DROP COLUMN IF EXISTS git_work_tree,
  DROP COLUMN IF EXISTS git_detected_at;
