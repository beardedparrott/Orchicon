-- DOWN for 20260929000000_mcp_server_ownership.sql: puts the tenant-tier
-- join table and the tenant-default column back.
--
-- KNOWN, ACCEPTED IRREVERSIBILITY (stated here, not discovered later):
--
--  1. A row CLONED across N projects by the up migration CANNOT be
--     un-cloned: the up migration records no clone linkage (a clone is
--     just another owned row), so this down migration cannot tell a clone
--     from an original. Every project-owned row is written into the join
--     table as an ORIGINAL reference, which is the closest faithful
--     reconstruction available.
--  2. CONVERSATION-owned rows have NO representation in the join table
--     (it is project ↔ server). They are left ownerless here and their
--     count is reported via RAISE NOTICE so the loss is observable rather
--     than silent.
--  3. UNIQUE(tenant_id, name) is restored, which the up migration
--     explicitly allows to be violated (two owners may each hold the same
--     name). If that happened, this statement FAILS and the operator has
--     to rename first — a loud failure, never a silent data change.
--  4. tenant_settings.default_mcp_servers comes back EMPTY: the down
--     migration cannot know the pre-up default set (the up migration
--     deleted the column and only reported the ids it held).

-- 1. Recreate the join table verbatim from 20260915000001_project_mcp_servers.sql.
CREATE TABLE IF NOT EXISTS "project_mcp_servers" (
  "project_id"     TEXT NOT NULL REFERENCES projects(id),
  "tenant_id"      TEXT NOT NULL REFERENCES tenants(id),
  "mcp_server_id"  TEXT NOT NULL REFERENCES mcp_servers(id),
  "created_at"     TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY ("project_id", "mcp_server_id")
);
CREATE INDEX IF NOT EXISTS project_mcp_servers_mcp_idx ON project_mcp_servers(mcp_server_id);

ALTER TABLE project_mcp_servers ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_mcp_servers FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON project_mcp_servers;
CREATE POLICY tenant_isolation ON project_mcp_servers
  FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- 2. Backfill it from ownership: every project-owned definition becomes one
--    project reference (see irreversibility #1 for why clones land here too).
INSERT INTO project_mcp_servers (project_id, tenant_id, mcp_server_id)
SELECT project_id, tenant_id, id FROM mcp_servers WHERE project_id IS NOT NULL
ON CONFLICT DO NOTHING;

-- 3. Re-add the tenant tier, EMPTY (irreversibility #4).
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS default_mcp_servers JSONB NOT NULL DEFAULT '[]'::jsonb;

-- 4. Report the conversation-owned rows this down migration cannot represent.
DO $$
DECLARE n int := 0;
BEGIN
  SELECT count(*) INTO n FROM mcp_servers WHERE conversation_id IS NOT NULL;
  IF n > 0 THEN
    RAISE NOTICE 'mcp ownership down: % conversation-owned definition(s) have no representation in project_mcp_servers and are left ownerless', n;
  END IF;
END $$;

-- 5. Drop the owner-scoped indexes, the XOR check, the two FKs, then the
--    owner columns themselves.
DROP INDEX IF EXISTS mcp_servers_project_name_key;
DROP INDEX IF EXISTS mcp_servers_conversation_name_key;
DROP INDEX IF EXISTS mcp_servers_project_idx;
DROP INDEX IF EXISTS mcp_servers_conversation_idx;
ALTER TABLE mcp_servers DROP CONSTRAINT IF EXISTS mcp_servers_owner_xor;
ALTER TABLE mcp_servers DROP CONSTRAINT IF EXISTS mcp_servers_conversation_fk;
ALTER TABLE mcp_servers DROP CONSTRAINT IF EXISTS mcp_servers_project_fk;
ALTER TABLE mcp_servers DROP COLUMN IF EXISTS project_id;
ALTER TABLE mcp_servers DROP COLUMN IF EXISTS conversation_id;

-- 6. Restore the tenant-wide name uniqueness (irreversibility #3: fails LOUD
--    if two owners hold the same name).
ALTER TABLE mcp_servers ADD CONSTRAINT mcp_servers_tenant_id_name_key UNIQUE (tenant_id, name);
