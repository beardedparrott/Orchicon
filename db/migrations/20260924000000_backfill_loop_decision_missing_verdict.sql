-- Backfill `on_missing_decision` onto the loop decisions that have no verdict to read.
--
-- WHY. A loop_decision routes on an upstream VERDICT, read from the value of
-- config.decision_field (default "_decision") in each upstream step run. When no
-- upstream supplies one, the engine falls through to its "no decision" branch: it
-- RE-ASKS the reviewer, and once the re-ask budget (max_reask, default 3) is
-- exhausted it FAILS the node — wedging the run until somebody force-progresses it.
--
-- For a gate whose only upstream is a task that emits no verdict, a re-ask cannot
-- ever succeed. The re-ask re-dispatches the SAME step, and the loop target IS that
-- same step, so the "re-ask" is a loop carrying no new information: it burns the
-- budget and then fails. The terminal devops loop — devops -> loop_decision, with
-- the loop going back to devops and success going to end — is exactly this shape.
-- It exists so a DevOps worker can be run again at the end before the workflow
-- finalizes.
--
-- The engine used to recognise that shape by TENANT STEP ID: it compared
-- loop_branch against "step-devops-pr" and success_branch against "step-l32ezp4b"
-- or "step-end". That guard matched only ONE of the two loops it was written for.
-- The SDLC (human approval) workflow's loop points at a different end step
-- (step-qonmbwyu), so it kept re-asking and kept failing; the "step-end" arm was a
-- fossil, carried by no live row. Both defects are the same root cause: engine
-- behaviour keyed on one tenant's identifiers.
--
-- That hardcode is gone (internal/scheduler/workflow_reconciler.go). The behaviour
-- is now the config key `on_missing_decision`, whose values are:
--
--   reask   — re-dispatch the reviewer and ask for a verdict. The DEFAULT, and what
--             an ABSENT key resolves to, so this policy is purely additive: every
--             workflow that does not carry the key behaves exactly as it did.
--   success — proceed forward. For a gate with no verdict to give.
--   fail    — a verdict is mandatory here; refuse immediately.
--
-- This migration writes `success` onto the steps the hardcode actually rescued, so
-- their behaviour is unchanged, AND onto the loop it failed to rescue, which fixes
-- it. It is the reason the hardcode can be deleted safely: MigrateOnBoot defaults to
-- true and the container applies pending migrations at boot BEFORE the control plane
-- (and therefore the reconciler) starts, so no boot can see the new code reading a
-- key the backfill has not yet written.
--
-- SEMANTICS
--   * Idempotent — the predicate excludes any config that already carries the key,
--     so a second run matches nothing; `IS DISTINCT FROM` on the rebuilt array keeps
--     the write to rows that actually change.
--   * Forward-only data: an UPDATE, no DDL. `config` remains a JSON string inside
--     the steps array; the key is additive and an older binary ignores it.
--   * ALL VERSIONS, not just the latest. A run can be bound to any published
--     version, and the key is behaviour-preserving, so rewriting the snapshots is
--     safe. Published versions are otherwise immutable — a backfill migration is the
--     sanctioned exception, as with 20260831000000_backfill_typed_budget_ladder.sql.
--   * The migration role is the Postgres superuser in every supported deployment, so
--     this UPDATE bypasses workflow_versions' FORCE ROW LEVEL SECURITY policy and
--     reaches every tenant (same reasoning as 20260806000000_normalize_work_item_kind.sql).
--
-- The SHAPE predicate — depends_on holds exactly one entry AND that entry is the
-- loop target — is deliberately NOT a list of ids. It is the structural condition
-- under which a re-ask provably cannot help, so it also covers a tenant or a future
-- seed whose step ids differ. The jsonb_typeof guard sits inside a CASE because SQL
-- does not guarantee AND evaluation order: a bare jsonb_array_length() on a
-- non-array would raise and abort the migration.
--
-- The config is spliced textually (prefix the existing object) rather than parsed and
-- re-serialised: it is a JSON string, and a cast of a malformed one would abort the
-- whole migration. A textual splice cannot throw.
UPDATE workflow_versions wv
SET steps = sub.new_steps
FROM (
  SELECT w.id,
         jsonb_agg(
           CASE
             WHEN s.elem->>'kind' = 'loop_decision'
              AND (s.elem->>'config') LIKE '{%'
              AND (s.elem->>'config') NOT LIKE '%on_missing_decision%'
              AND CASE
                    WHEN jsonb_typeof(s.elem->'depends_on') = 'array'
                    THEN jsonb_array_length(s.elem->'depends_on')
                    ELSE -1
                  END = 1
              AND (s.elem->>'config') LIKE '%"loop_branch":"' || (s.elem->'depends_on'->>0) || '"%'
             THEN jsonb_set(
                    s.elem,
                    '{config}',
                    to_jsonb('{"on_missing_decision":"success",' || substr(s.elem->>'config', 2))
                  )
             ELSE s.elem
           END
           ORDER BY s.ord
         ) AS new_steps
  FROM workflow_versions w
  CROSS JOIN LATERAL jsonb_array_elements(w.steps) WITH ORDINALITY AS s(elem, ord)
  GROUP BY w.id
) sub
WHERE wv.id = sub.id
  AND wv.steps IS DISTINCT FROM sub.new_steps;
