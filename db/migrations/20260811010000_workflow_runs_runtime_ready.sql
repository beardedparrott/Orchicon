-- Workflow runs: the runtime-serve readiness gate.
--
-- When a workflow run leaves `pending` it must NOT dispatch any execution
-- until the workflow's runtime container's opencode serve is proven usable
-- (health + session-create round-trip). `runtime_ready` is the persisted
-- gate marker: the WorkflowReconciler flips it false at run start, an async
-- ensure-serving pass proves the serve and flips it true, and step DAG
-- progression (and therefore execution dispatch) is held while it is false.
-- A plane restart re-triggers the probe for any running run with the flag
-- still false. Additive and forward-only; RLS untouched (existing
-- tenant_isolation policy already scopes the row).
ALTER TABLE workflow_runs
  ADD COLUMN IF NOT EXISTS runtime_ready boolean NOT NULL DEFAULT false;
