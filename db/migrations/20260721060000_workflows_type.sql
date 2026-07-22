-- Add type column to workflows to distinguish one-shot workflows from
-- repeatable templates (docs/11 §2.1). Backfill existing rows:
--   project_id = ''  → template
--   project_id != '' → one_shot
ALTER TABLE workflows
  ADD COLUMN type TEXT NOT NULL DEFAULT 'one_shot';

UPDATE workflows SET type = 'template' WHERE project_id = '';

COMMENT ON COLUMN workflows.type
  IS 'Workflow type: one_shot (project-scoped, runs once) or template (tenant-level, bindable to work items).';
