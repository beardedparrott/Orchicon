-- Add is_follow_up flag to worker_executions so follow-up executions
-- (created by CreateFollowUpExecution) can be hidden from the default
-- execution list view.
ALTER TABLE worker_executions
  ADD COLUMN is_follow_up boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN worker_executions.is_follow_up
  IS 'True when this execution was created as a follow-up to another execution (CreateFollowUpExecution). Hidden from the default list view.';
