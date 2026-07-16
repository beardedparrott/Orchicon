-- PR B (context propagation): work item carries the composite prompt the
-- worker should see, populated by the WorkflowReconciler before dispatch.
--
-- The composite prompt includes:
--   - the work item itself (title, description, acceptance criteria)
--   - ancestor work items (parent_id chain — project context)
--   - upstream step summaries from prior worker steps in this workflow
--
-- The opencode adapter reads this column (via the TaskReconciler → manifest
-- Goal) instead of just task.Title, so the worker sees the full context.
--
-- The column is JSONB for forward compatibility (key-value shape rather
-- than a single text blob) but the v0.1 reader only uses one field:
-- `prompt_context.composite` — the full text the worker receives as the
-- message. The shape is:
--   {"composite": "# Task\n...\n# Project context\n...\n# Upstream context\n..."}
--
-- Only the work_items table changes. RLS is unchanged because the
-- tenant_id column on work_items is the same one that the existing
-- tenant_isolation policy uses; no new policy needed.

ALTER TABLE "work_items" ADD COLUMN "prompt_context" jsonb NULL;
COMMENT ON COLUMN "work_items"."prompt_context" IS 'Composite prompt the worker should see (set by WorkflowReconciler before dispatch; docs/02 §2.4).';
