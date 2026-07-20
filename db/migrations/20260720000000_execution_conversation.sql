-- Add conversation column to worker_executions for follow-up chat
-- persistence (docs/10 §11, PR: execution UI fixes + features).
-- Stores a JSONB array of {role, content, type, created_at} entries.
ALTER TABLE worker_executions
  ADD COLUMN conversation jsonb NOT NULL DEFAULT '[]';

COMMENT ON COLUMN worker_executions.conversation
  IS 'Follow-up conversation history. Array of {role, content, type, created_at} objects appended by CreateFollowUpExecution and the frontend for inline rendering.';
