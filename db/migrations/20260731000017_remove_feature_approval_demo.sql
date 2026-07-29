-- Remove the "Feature Development w/ Approval Gate" demo workflow that was
-- seeded in previous migrations. This workflow is being replaced by canned
-- workflows in Go seed code (internal/db/seed_workflows.go).
--
-- Migration is idempotent — the workflow may already have been removed by
-- a prior migration or manual cleanup.

DELETE FROM workflow_step_runs
WHERE workflow_run_id IN (
  SELECT id FROM workflow_runs WHERE workflow_id = 'wf_feature_approval_demo'
);

DELETE FROM workflow_runs WHERE workflow_id = 'wf_feature_approval_demo';

DELETE FROM workflow_versions WHERE workflow_id = 'wf_feature_approval_demo';

DELETE FROM workflows WHERE id = 'wf_feature_approval_demo' AND name = 'Feature Development w/ Approval Gate';
