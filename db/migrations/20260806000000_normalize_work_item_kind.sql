-- Normalize work item kinds created via Ask Orchicon.
--
-- The Ask Orchicon create_work_item tool stored the kind string verbatim
-- (e.g. "Epic", "Task" — title-cased), while the domain's canonical kind
-- constants are lowercase (epic/feature/task/subtask). The Connect API's
-- kindToProto maps case-sensitively against those constants, so title-cased
-- rows fell through to WORK_ITEM_KIND_UNSPECIFIED and the UI rendered
-- `kind: unknown`.
--
-- 1. Repair: fold any existing non-lowercase canonical kind down to
--    lowercase. Rows whose kind is not one of the canonical kinds are left
--    untouched — the CHECK below then fails loudly on them (desired:
--    resolve genuinely unknown kinds with the maintainer before applying).
--    The migration role is the Postgres superuser in every supported
--    deployment, so this UPDATE bypasses the table's FORCE ROW LEVEL
--    SECURITY policy and reaches every tenant.
-- 2. Harden: the CHECK constraint enforces the invariant at the storage
--    layer (canonical value AND canonical casing), so the whole class of
--    bug cannot recur. Recovery kinds are included because they are
--    first-class typed work item kinds in the domain model.
--
-- The UPDATE and the ALTER run in one implicit transaction (the migration
-- runner executes the file as a single multi-statement query), so a
-- failure on a genuinely unknown kind rolls back the repair — fail-loud,
-- no partial backfill.

UPDATE work_items SET kind = lower(kind)
WHERE kind <> lower(kind)
  AND lower(kind) IN ('epic','feature','task','subtask');

ALTER TABLE work_items ADD CONSTRAINT work_items_kind_check
  CHECK (kind = lower(kind) AND kind IN
    ('epic','feature','task','subtask',
     'recovery_stop','recovery_summarize_restart','recovery_human_escalation','recovery_retry_n'));
