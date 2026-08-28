-- Native secrets store (encrypted at rest, selectable per work item)
-- Tenant-scoped secrets + per-work-item secret selection.

CREATE TABLE IF NOT EXISTS "tenant_secrets" (
  "id"          TEXT NOT NULL PRIMARY KEY,
  "tenant_id"   TEXT NOT NULL REFERENCES tenants(id),
  "name"        TEXT NOT NULL CHECK (name ~ '^[A-Z][A-Z0-9_]+$'),
  "description" TEXT NOT NULL DEFAULT '',
  "ciphertext"  TEXT NOT NULL,
  "key_version" SMALLINT NOT NULL DEFAULT 1,
  "created_at"  TIMESTAMPTZ NOT NULL DEFAULT now(),
  "updated_at"  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE ("tenant_id", "name")
);
CREATE INDEX IF NOT EXISTS tenant_secrets_tenant_id_idx ON tenant_secrets(tenant_id);
CREATE UNIQUE INDEX IF NOT EXISTS tenant_secrets_tenant_name_idx ON tenant_secrets(tenant_id, name);

ALTER TABLE tenant_secrets ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_secrets FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON tenant_secrets;
CREATE POLICY tenant_isolation ON tenant_secrets
  FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Backfill drift columns that exist via prior migrations but were missing
-- from the declarative schema.hcl source, so the schema now matches reality.
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS prompt_context JSONB;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS scheduled_start_at TIMESTAMPTZ;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS auto_start_workflow BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS context_files JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS acceptance_review TEXT NOT NULL DEFAULT '';
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS recurring_schedule JSONB;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS next_run_at TIMESTAMPTZ;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS recurring_enabled BOOLEAN NOT NULL DEFAULT true;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS spawned_by_work_item_id TEXT;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS spawned_by_run_id TEXT;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS sequence_attempts INT NOT NULL DEFAULT 0;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS sequence_last_attempt_at TIMESTAMPTZ;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS sequence_consecutive_scan_errors INT NOT NULL DEFAULT 0;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS sequence_last_progress_at TIMESTAMPTZ;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS sort_order DOUBLE PRECISION;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS archived_from_status TEXT;

-- Per-work-item secret selection (covers regular and recurring — same table).
ALTER TABLE work_items ADD COLUMN IF NOT EXISTS secret_ids JSONB NOT NULL DEFAULT '[]'::jsonb;
