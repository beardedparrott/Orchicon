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
    // Picker emits `{ id, command }` reference entries and stores ids only.
    expect(picker).toContain("{ id: srv.id, command: srv.command }");
    expect(picker).toMatch(/references/);
    // api layer keys project selection by server id arrays.
    expect(api).toContain("mcpServerIds");
    expect(api).toContain("mcpKeys.project");
  });

  it("worker form renders the tenant MCP picker and writes permissions.mcp_servers", () => {
    expect(workerForm).toContain("<MCPPicker");
    expect(workerForm).toContain("mcp_servers");
    expect(workerForm).toMatch(/references into Settings/);
    // Worker empty = project defaults (resolution order note).
    expect(workerForm).toMatch(/Worker selection empty = project defaults/);
  });

  it("project edit page lists configured MCP servers and auto-refreshes on save", () => {
    expect(projectEdit).toContain("useGetProjectMCPServers");
    expect(projectEdit).toContain("useSetProjectMCPServers");
    expect(projectEdit).toContain("<MCPPicker");
    expect(projectEdit).toMatch(/[Aa]uto-refreshes on save/);
    expect(projectEdit).toMatch(/workers fall back/);
  });

  it("project create page collects MCP selection and persists after creation", () => {
    expect(projectNew).toContain("useSetProjectMCPServers");
    expect(projectNew).toContain("<MCPPicker");
    expect(projectNew).toContain("mcpServerIds");
    expect(projectNew).toMatch(/inherit the tenant default/);
  });
});
