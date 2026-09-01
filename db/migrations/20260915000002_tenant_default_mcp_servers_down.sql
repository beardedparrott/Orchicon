-- Roll back the tenant default MCP server set column.
ALTER TABLE tenant_settings DROP COLUMN IF EXISTS default_mcp_servers;
