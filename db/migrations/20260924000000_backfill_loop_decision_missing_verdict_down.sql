-- Reverse 20260924000000_backfill_loop_decision_missing_verdict.sql: drop the
-- `on_missing_decision` key this migration wrote.
--
-- Only rows carrying the EXACT form the up migration inserted are touched — the key
-- as the FIRST member of the config object, with its trailing comma. The three
-- nested replaces cover the three shapes that can arise from editing the config
-- afterwards:
--
--   1. first member, others follow:  {"on_missing_decision":"success",<rest>}
--   2. a later member:               {<head>,"on_missing_decision":"success"}
--   3. the only member:              {"on_missing_decision":"success"}
--
-- Removing the key returns those steps to the general re-ask path. Combined with the
-- paired code revert (restoring the tenant-id guard) that is the pre-migration
-- behaviour for the loop the guard rescued. The OTHER loop — SDLC (human approval),
-- which the guard never matched — simply goes back to failing on a missing verdict,
-- which is what it did before this change.
--
-- Like the up migration, this is a data-only UPDATE with no DDL, and reaches every
-- tenant because the migration role is the Postgres superuser (workflow_versions has
-- FORCE ROW LEVEL SECURITY).
UPDATE workflow_versions wv
SET steps = sub.new_steps
FROM (
  SELECT w.id,
         jsonb_agg(
           CASE
             WHEN (s.elem->>'config') LIKE '%on_missing_decision%'
             THEN jsonb_set(
                    s.elem,
                    '{config}',
                    to_jsonb(
                      replace(
                        replace(
                          replace(
                            s.elem->>'config',
                            '{"on_missing_decision":"success"}', '{}'
                          ),
                          '{"on_missing_decision":"success",', '{'
                        ),
                        ',"on_missing_decision":"success"', ''
                      )
                    )
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
