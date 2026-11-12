-- A TERMINAL RUN'S CONTAINER IS REAPED ONCE, AND THE SWEEP REMEMBERS THAT IT WAS.
--
-- REPORTED: the log filled with two INFO lines every second, for the same run, forever:
--
--   {"level":"INFO","msg":"workflow runtime reaped","run":"01M6MKW6V6KC91CTTTPVKTT58W"}
--   {"level":"INFO","msg":"runtime reap sweep: reaped a terminal run's container","run":"01M6MKW6V6KC91CTTTPVKTT58W",
--    "status":"completed"}
--
-- THE SWEEP'S CANDIDATE PREDICATE WAS NEVER CONSUMED. It selected
--
--   status IN ('completed','failed','aborted') AND (runtime_ready = true OR runtime_image <> '')
--
-- and NOTHING EVER CLEARS either column — runtime_image is the run's history and runtime_ready is a gate, so
-- neither is the sweep's to erase. Every terminal run that had ever used a runtime image was therefore a
-- candidate on EVERY pass, forever. Each pass called ReapForRun (a no-op once the container is gone) and logged
-- "reaped a terminal run's container" — a sentence that was FALSE from the second pass onward, repeated once a
-- second for the life of the plane.
--
-- So the evidence of the reap is recorded, and the candidate set excludes it. One row per run, once, which is
-- also the only honest place for "we already dealt with this".
--
-- A COLUMN, NOT A run_context KEY: this is an OUTCOME, and outcomes belong in columns an operator can query —
-- the same reason workflow_runs.error_message and local_integrations exist. run_context holds the run's INPUTS.
ALTER TABLE workflow_runs ADD COLUMN IF NOT EXISTS runtime_reaped_at timestamptz;

-- The sweep's read is (tenant, terminal status, evidence, not yet reaped). Partial index: only terminal rows
-- still awaiting a reap are ever interesting, and after this change that set converges to empty.
CREATE INDEX IF NOT EXISTS workflow_runs_runtime_reap_idx
  ON workflow_runs (tenant_id, ended_at)
  WHERE runtime_reaped_at IS NULL
    AND status IN ('completed', 'failed', 'aborted')
    AND (runtime_ready = true OR runtime_image <> '');
