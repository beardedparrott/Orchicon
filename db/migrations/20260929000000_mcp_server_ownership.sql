-- MCP definitions become OWNER-SCOPED: a definition belongs to exactly one
-- project OR one Ask conversation, instead of being a tenant-wide row that
-- projects reference through a join table.
--
-- WHY THE CONVERSATION FOREIGN KEY IS COMPOSITE (and must be): the
-- referenced table declares PRIMARY KEY (tenant_id, id) —
-- db/migrations/20260731000016_ask_orchicon.sql:15 — so there is NO unique
-- index on `id` alone and a single-column
--   FOREIGN KEY (conversation_id) REFERENCES ask_orchicon_conversations(id)
--   ... is impossible: Postgres requires the referenced columns to be a
--   unique/primary key. The FK must therefore carry tenant_id as well:
--     FOREIGN KEY (tenant_id, conversation_id)
--       REFERENCES ask_orchicon_conversations(tenant_id, id)
-- which additionally makes the ON DELETE CASCADE tenant-correct: a cascade
-- can never cross a tenant boundary.
--
-- WHY THE TENANT TIER GOES AWAY: `tenant_settings.default_mcp_servers` was
-- the third resolution tier (worker → project → tenant default → none) and
-- a tenant-wide default has no bounded owner — an orphaned id there has no
-- target to be re-attached to, which is why the backfill REPORTS it rather
-- than cloning it.
--
-- WHY NAME UNIQUENESS MOVES: UNIQUE(tenant_id, name) forbids two projects
-- each owning a server called `postgres`, which the owner-scoped model
-- requires. It is replaced by one partial unique index per owner.
--
-- Additive + backfilling; paired with 20260929000000_mcp_server_ownership_down.sql.

-- 1. Owner columns. Both nullable: every pre-existing row is ownerless
--    until the backfill below assigns one.
ALTER TABLE mcp_servers ADD COLUMN IF NOT EXISTS project_id text NULL;
ALTER TABLE mcp_servers ADD COLUMN IF NOT EXISTS conversation_id text NULL;

-- 2. Drop the old tenant-wide name uniqueness NOW, BEFORE the backfill
--    clones anything: a clone keeps its source row's name, so while
--    UNIQUE(tenant_id, name) exists every clone INSERT would fail. Done via
--    a defensive loop (not a hardcoded constraint name) so it works whether
--    the constraint carries its auto-generated name or a hand-set one.
DO $$
DECLARE c record;
BEGIN
  FOR c IN SELECT conname FROM pg_constraint
           WHERE conrelid = 'mcp_servers'::regclass AND contype = 'u' LOOP
    EXECUTE format('ALTER TABLE mcp_servers DROP CONSTRAINT %I', c.conname);
  END LOOP;
END $$;

DO $$
DECLARE
  n int := 0;
  cloned_secret int := 0;
  orphan text;
  ownerless int := 0;
  ref record;
  clone_id text;
BEGIN
  -- 3. FIRST: the first referencing project takes the row itself, and every
  --    additional referencing project gets a CLONE. Deterministic order:
  --    created_at ASC, project_id ASC — so a re-run of this migration on the
  --    same data always picks the same "first" project.
  WITH first_ref AS (
    SELECT DISTINCT ON (mcp_server_id) mcp_server_id, project_id
    FROM project_mcp_servers
    ORDER BY mcp_server_id, created_at ASC, project_id ASC
  )
  UPDATE mcp_servers m SET project_id = f.project_id
  FROM first_ref f WHERE m.id = f.mcp_server_id;

  -- 4. Every ADDITIONAL referencing project gets a CLONE (new id, every
  --    field copied) — a definition can only have one owner now. Safe here
  --    because step 2 already dropped the old UNIQUE(tenant_id, name).
  FOR ref IN
    SELECT pms.mcp_server_id, pms.project_id,
           row_number() OVER (PARTITION BY pms.mcp_server_id
                              ORDER BY pms.created_at ASC, pms.project_id ASC) AS rn
    FROM project_mcp_servers pms
  LOOP
    CONTINUE WHEN ref.rn = 1;
    clone_id := gen_random_uuid()::text;
    INSERT INTO mcp_servers (id, tenant_id, name, transport, command, args, env, url, headers, enabled,
                             catalog_slug, install_status, install_result, created_at, updated_at, project_id)
    SELECT clone_id, s.tenant_id, s.name, s.transport, s.command, s.args, s.env, s.url, s.headers, s.enabled,
           s.catalog_slug, s.install_status, s.install_result, s.created_at, now(), ref.project_id
    FROM mcp_servers s WHERE s.id = ref.mcp_server_id;
    n := n + 1;
    -- 4b. A clone whose credential name is ID-DERIVED points at a secret
    --     that does not exist. SecretNameFor (internal/mcpsettings/service.go)
    --     derives MCP_<SLUG>_<ENV> from catalog_slug when set, ELSE from the
    --     row id — so a manual entry (catalog_slug = '') with an id-derived
    --     secret name cannot be minted for the clone (it would reference an
    --     unreachable secret). REPORT it; never mint a name.
    IF (SELECT COALESCE(catalog_slug, '') FROM mcp_servers WHERE id = clone_id) = ''
       AND EXISTS (SELECT 1 FROM mcp_servers WHERE id = clone_id
                   AND (env::text LIKE '%${%' OR headers::text LIKE '%${%'))
    THEN
      cloned_secret := cloned_secret + 1;
      RAISE NOTICE 'mcp ownership backfill: clone % (project %, name %) has an ID-DERIVED secret name (no catalog slug); its credential must be RE-ENTERED in Settings -> Adapters -> MCP',
        clone_id, ref.project_id, (SELECT name FROM mcp_servers WHERE id = clone_id);
    END IF;
  END LOOP;
  RAISE NOTICE 'mcp ownership backfill: % definition(s) cloned across projects', n;
  RAISE NOTICE 'mcp ownership backfill: % clone(s) need credential re-entry', cloned_secret;

  -- 5. Tenant-default orphans: REPORTED, never cloned — there is no bounded
  --    target for a tenant-wide default. Reported BEFORE the ownerless-row
  --    delete below so an id that is both a tenant default and ownerless is
  --    observably reported as an orphan and only then deleted.
  FOR orphan IN SELECT jsonb_array_elements_text(default_mcp_servers) FROM tenant_settings LOOP
    RAISE NOTICE 'mcp ownership backfill: tenant default MCP server % is an ORPHAN after the tenant tier is removed and needs RE-ATTACHMENT to a project or conversation', orphan;
  END LOOP;

  -- 6. Any row still ownerless is unreachable through any project (neither a
  --    project reference nor a tenant default target exists): report, then
  --    delete, so the XOR CHECK below can be added.
  SELECT count(*) INTO ownerless FROM mcp_servers WHERE project_id IS NULL AND conversation_id IS NULL;
  IF ownerless > 0 THEN
    RAISE NOTICE 'mcp ownership backfill: deleting % ownerless definition(s) (referenced by no project)', ownerless;
    DELETE FROM mcp_servers WHERE project_id IS NULL AND conversation_id IS NULL;
  END IF;
END $$;

-- 7. Owner foreign keys. project_id -> projects(id); conversation_id is the
--    COMPOSITE (tenant_id, conversation_id) -> (tenant_id, id) FK the header
--    explains. Both cascade: deleting the owner deletes its definitions.
ALTER TABLE mcp_servers DROP CONSTRAINT IF EXISTS mcp_servers_project_fk;
ALTER TABLE mcp_servers ADD CONSTRAINT mcp_servers_project_fk
  FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE;
ALTER TABLE mcp_servers DROP CONSTRAINT IF EXISTS mcp_servers_conversation_fk;
ALTER TABLE mcp_servers ADD CONSTRAINT mcp_servers_conversation_fk
  FOREIGN KEY (tenant_id, conversation_id) REFERENCES ask_orchicon_conversations(tenant_id, id) ON DELETE CASCADE;

-- 8. The XOR: exactly one owner is non-null. Added AFTER the backfill —
--    every pre-existing row was (NULL, NULL) and would violate it.
ALTER TABLE mcp_servers DROP CONSTRAINT IF EXISTS mcp_servers_owner_xor;
ALTER TABLE mcp_servers ADD CONSTRAINT mcp_servers_owner_xor
  CHECK ((project_id IS NULL) <> (conversation_id IS NULL));

-- 9. Owner-scoped name uniqueness + per-owner read indexes. Created after the
--    backfill so a bad backfill fails LOUDLY here rather than blocking the
--    clone inserts above.
CREATE UNIQUE INDEX IF NOT EXISTS mcp_servers_project_name_key
  ON mcp_servers (tenant_id, project_id, name) WHERE project_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS mcp_servers_conversation_name_key
  ON mcp_servers (tenant_id, conversation_id, name) WHERE conversation_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS mcp_servers_project_idx
  ON mcp_servers (tenant_id, project_id) WHERE project_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS mcp_servers_conversation_idx
  ON mcp_servers (tenant_id, conversation_id) WHERE conversation_id IS NOT NULL;

-- 10. The join table and the tenant tier are gone: selection IS ownership.
DROP TABLE IF EXISTS project_mcp_servers;
ALTER TABLE tenant_settings DROP COLUMN IF EXISTS default_mcp_servers;
