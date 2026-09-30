import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// Source-scan test (mirrors MCPServersTab.test.tsx): verifies the project
// and worker editors are wired to the tenant MCP registry with
// reference-based selections (never copies) and react-query invalidation
// for auto-refresh on save.
describe("MCP pickers (ADR-0008 project/worker integration)", () => {
  const picker = fs.readFileSync(path.join(__dirname, "MCPPicker.tsx"), "utf8");
  const api = fs.readFileSync(path.join(__dirname, "../api/mcpServers.ts"), "utf8");
  const workerForm = fs.readFileSync(path.join(__dirname, "WorkerFormSections.tsx"), "utf8");
  const projectEdit = fs.readFileSync(path.join(__dirname, "../routes/projects_.$id.tsx"), "utf8");
  const projectNew = fs.readFileSync(path.join(__dirname, "../routes/projects_.new.tsx"), "utf8");

  it("MCPPicker lists the tenant registry (not opencode well-known list)", () => {
    expect(picker).toContain("useMCPServerList");
    expect(picker).not.toContain("useListOpenCodeMCPs");
    expect(picker).toContain("Settings → Adapters → MCP");
  });

  it("selections are reference ids, never copies", () => {
    // Picker emits `{ id, command }` reference entries (worker path).
    expect(picker).toContain("{ id: srv.id, command: srv.command }");
    expect(picker).toMatch(/references/);
    // The api layer is owner-scoped (selection IS ownership): there is no
    // project/tenant selection-id array any more, only the owner keys.
    expect(api).not.toContain("mcpServerIds");
    expect(api).toContain("ownerProject");
    expect(api).toContain("ownerConversation");
  });

  it("worker form renders the tenant MCP picker and writes permissions.mcp_servers", () => {
    expect(workerForm).toContain("<MCPPicker");
    expect(workerForm).toContain("mcp_servers");
    expect(workerForm).toMatch(/references into Settings/);
    // Worker empty = project defaults (resolution order note).
    expect(workerForm).toMatch(/Worker selection empty = project defaults/);
  });

  it("project pages carry NO reference-based MCP selection any more", () => {
    // Selection IS ownership (mcp_servers.project_id): the project↔server
    // selection RPC pair is gone, so the pages no longer read or write one.
    // Child 7 re-homes the owner-scoped control.
    expect(projectEdit).not.toContain("useGetProjectMCPServers");
    expect(projectEdit).not.toContain("useSetProjectMCPServers");
    expect(projectEdit).not.toContain("mcpServerIds");
    expect(projectNew).not.toContain("useSetProjectMCPServers");
    expect(projectNew).not.toContain("mcpServerIds");
  });
});
