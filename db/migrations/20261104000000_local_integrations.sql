-- LOCAL INTEGRATION IS A RECORD, NOT A GUESS.
--
-- The problem this table exists for, stated as it happened. On a `git_strategy=local` project six features ran
-- back to back and all six reported success. Four reached the integration branch; two did not, and NOTHING
-- said so — not the work item (succeeded), not the workflow run (completed), not the acceptance review
-- (written). The only trace was that two branches still existed under refs/heads/.
--
-- The work had been committed correctly every time. What went missing was the platform's own obligation to
-- land it, and the reason it went missing is that the obligation was never WRITTEN DOWN anywhere. It was
-- inferred, every pass, from three things that are not records:
--
--   * a `local_integrated_at` key stuffed into workflow_runs.run_context — an opaque JSONB blob that is
--     semantically the run's INPUTS, so an outcome filed there is invisible in the API and wrong in principle;
--   * an ancestry check (`git merge-base --is-ancestor`) — the honest git-level question, but it answers "is
--     it merged RIGHT NOW", not "did we promise to merge it and then fail";
--   * the periodic sweep's cursor and budget — so a run whose integration was skipped for lack of budget in
--     one pass was indistinguishable from a run that never needed integrating.
--
-- REPORTED as "keep real record of these occurrences instead of weird schedules/monitors/races". That is
-- exactly right: a pending obligation needs a row. With one, the queue is a query, the wait is a count, a
-- conflict is a status an operator can see, and nothing has to be inferred from elapsed time or from a ref
-- that may have been swept.
--
-- KEYED BY workflow_run_id, and it records execution_id alongside. The RUN owns the branch (worktree_branch
-- lives on the run), so the run is the unit of integration; the execution is recorded because it is the thing
-- that actually produced the commit, and because an operator asking "which worker wrote this and did it land"
-- should not have to join three tables to find out. UNIQUE (tenant_id, workflow_run_id) makes enqueueing
-- idempotent — a run can never accumulate two competing obligations.
--
-- STATUS VOCABULARY. Deliberately not a database enum: a new state must be addable without a migration.
--   pending     the obligation exists and the work has not landed yet. THIS is what blocks a project's queue.
--   merged      landed on the integration branch; merge_commit records the resulting commit.
--   conflicted  a real merge conflict; nothing was changed and a human must resolve it. It KEEPS BLOCKING,
--               because a project that branches past a conflict would diverge irrecoverably — the failure this
--               table was built to stop. It is visible rather than silent, which is the whole difference.
--   skipped     there was provably nothing to land (not a local strategy, no branch, branch already gone).
CREATE TABLE IF NOT EXISTS "local_integrations" (
  "id"                 text        NOT NULL PRIMARY KEY,
  "tenant_id"          text        NOT NULL,
  "project_id"         text        NOT NULL,
  "workflow_run_id"    text        NOT NULL,
  "execution_id"       text        NOT NULL DEFAULT '',
  "branch"             text        NOT NULL,
  "base_commit"        text        NOT NULL DEFAULT '',
  "integration_branch" text        NOT NULL DEFAULT '',
  "status"             text        NOT NULL DEFAULT 'pending',
  "attempts"           integer     NOT NULL DEFAULT 0,
  "last_error"         text        NOT NULL DEFAULT '',
  "merge_commit"       text        NOT NULL DEFAULT '',
  "created_at"         timestamptz NOT NULL DEFAULT now(),
  "updated_at"         timestamptz NOT NULL DEFAULT now(),
  "merged_at"          timestamptz
);

-- One obligation per run, enforced by the database rather than by every caller remembering to check.
CREATE UNIQUE INDEX IF NOT EXISTS "local_integrations_run_key"
  ON "local_integrations" ("tenant_id", "workflow_run_id");

-- The queue read: oldest pending obligation for one project. Ordered by created_at so the merge order matches
-- the order the work was produced — which is what makes the branches fast-forwardable instead of divergent.
CREATE INDEX IF NOT EXISTS "local_integrations_queue_idx"
  ON "local_integrations" ("tenant_id", "project_id", "status", "created_at");

ALTER TABLE "local_integrations" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "local_integrations" FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON "local_integrations"
  FOR ALL
  USING ("tenant_id" = current_setting('app.tenant_id', true));

-- BACKFILL FROM EXISTING RUNS, because the runs that motivated this table are still sitting there.
--
-- A completed run with a branch that no local_integrations row mentions is, by definition, work whose landing
-- was never recorded — precisely the state the report describes. Enqueueing every one of them as `pending`
-- would be wrong (most were integrated long ago and their branches swept), and leaving them `pending` would
-- stall every new run behind them, so the backfill records them as `adopted`: drained by the queue in the
-- background, NOT counted by the gate. The git-level ancestry check still decides each one's real fate on the
-- next pass, so a branch that is already an ancestor settles as merged and a gone branch settles as skipped.
-- What matters is that the OBLIGATION now exists for every run — including the two the operator lost — so it is
-- drainable and countable rather than discovered by hand.
--
-- id is derived deterministically from the run so the backfill is idempotent and cannot collide with itself.
INSERT INTO "local_integrations"
  (id, tenant_id, project_id, workflow_run_id, execution_id, branch, status, last_error, created_at, updated_at)
SELECT
  'li_' || r.id,
  r.tenant_id,
  r.project_id,
  r.id,
  COALESCE((
    SELECT sr.worker_execution_id
      FROM workflow_step_runs sr
     WHERE sr.tenant_id = r.tenant_id AND sr.workflow_run_id = r.id
       AND sr.worker_execution_id <> ''
     ORDER BY sr.created_at ASC
     LIMIT 1
  ), ''),
  r.worktree_branch,
  'adopted',
  'backfilled: predates the local_integrations record; the next integration pass re-evaluates it',
  COALESCE(r.ended_at, r.created_at),
  now()
FROM workflow_runs r
WHERE r.status = 'completed'
  AND r.worktree_branch <> ''
  AND NOT EXISTS (
    SELECT 1 FROM "local_integrations" li
     WHERE li.tenant_id = r.tenant_id AND li.workflow_run_id = r.id
  );
