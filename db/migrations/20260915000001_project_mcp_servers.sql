-- Project ↔ MCP server join table (references, never copies): a
-- project's MCP selection is a set of mcp_servers ids. Editing one
-- server entry updates every referencing project automatically.

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
