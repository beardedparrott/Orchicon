-- Change the FK constraint on workflow_runs.work_item_id to
-- ON DELETE SET NULL so that deleting a work item does not fail
-- when bound workflow runs reference it. The run's work_item_id
-- becomes NULL (effectively an unbound run) rather than blocking
-- the delete (docs/11 §2.1).
ALTER TABLE workflow_runs
  DROP CONSTRAINT IF EXISTS workflow_runs_work_item_id_fkey,
  ADD CONSTRAINT workflow_runs_work_item_id_fkey
    FOREIGN KEY (work_item_id) REFERENCES work_items(id)
    ON DELETE SET NULL;
