ALTER TABLE "work_items" DROP COLUMN IF EXISTS "ephemeral";
ALTER TABLE "workers" DROP COLUMN IF EXISTS "ephemeral";
ALTER TABLE "workflows" DROP COLUMN IF EXISTS "ephemeral";

-- The indexes go with the columns: DROP COLUMN would drop them anyway, but
-- naming them keeps the down migration readable and reversible in isolation.
DROP INDEX IF EXISTS "idx_work_items_ephemeral_created";
DROP INDEX IF EXISTS "idx_workers_ephemeral_created";
DROP INDEX IF EXISTS "idx_workflows_ephemeral_created";
