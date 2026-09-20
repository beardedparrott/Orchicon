-- Tenant default MCP server set: the resolution-order fallback when a
-- worker has no MCP selection and its project has none either
-- (worker → project → tenant default → none). Stored as an id array on
-- tenant_settings (no new table).
ALTER TABLE tenant_settings ADD COLUMN IF NOT EXISTS default_mcp_servers JSONB NOT NULL DEFAULT '[]'::jsonb;
