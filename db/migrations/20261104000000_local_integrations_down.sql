-- Reverse of 20261104000000_local_integrations.sql.
--
-- The table is the platform's RECORD that a run's work was landed. Dropping it loses that history — but the
-- git-level truth it described is unaffected: branches, commits and the integration branch are untouched by
-- the migration in either direction, so a downgrade cannot lose WORK, only the record of which work landed.
-- The pre-table behaviour (inference from ancestry plus the run_context stamp) resumes when the code is
-- downgraded with it.
DROP TABLE IF EXISTS "local_integrations";
