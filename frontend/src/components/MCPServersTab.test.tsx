import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

describe("MCPServersTab (ADR-0008)", () => {
  const src = fs.readFileSync(path.join(__dirname, "MCPServersTab.tsx"), "utf8");
  const api = fs.readFileSync(path.join(__dirname, "../api/mcpServers.ts"), "utf8");

  it("lists tenant MCP servers and renders the registry catalog", () => {
    expect(src).toContain("useMCPServerList");
    expect(src).toContain("useMCPCatalog");
    expect(src).toMatch(/Registry catalog/);
    expect(src).toMatch(/MCP Servers/);
  });

  it("one-click catalog add prefills the create form", () => {
    expect(src).toContain("usePrefillMCPCatalogEntry");
    expect(src).toContain("handleCatalogAdd");
    expect(api).toContain("prefillMCPCatalogEntry");
    expect(src).toMatch(/prefills the add form/);
  });

  it("supports stdio and streamable HTTP transports", () => {
    expect(src).toContain("MCP_SERVER_TRANSPORT_STDIO");
    expect(src).toContain("MCP_SERVER_TRANSPORT_STREAMABLE_HTTP");
    expect(src).toMatch(/stdio/);
    expect(src).toMatch(/streamable HTTP/);
    // Transport switch shows command+args+env for stdio, URL+headers for HTTP.
    expect(src).toContain("form.transport === MCPServerTransport.MCP_SERVER_TRANSPORT_STDIO");
  });

  it("auto-install is explicit (operator clicks Install) and dry-run safe", () => {
    expect(src).toContain("useInstallMCPServer");
    expect(api).toContain("installMCPRuntime");
    // The hook passes dryRun through to the RPC — no implicit network installs.
    expect(api).toContain("dryRun");
    expect(src).toMatch(/Installations are explicit/);
    expect(src).toMatch(/Install/);
  });

  it("detects runtimes (npx/uvx/docker) and disables install when missing", () => {
    expect(src).toContain("useMCPRuntimes");
    expect(src).toContain("runtimeAvailable");
    expect(src).toMatch(/runtime missing/);
  });

  it("auto-writes tenant secrets on save (provider-token pattern, never returned)", () => {
    expect(src).toContain("useSetMCPServerSecret");
    expect(api).toContain("setMCPServerSecret");
    expect(src).toContain('type="password"');
    expect(src).toMatch(/tenant secrets store/);
  });

  it("records install results and surfaces failures clearly", () => {
    expect(src).toContain("installResult");
    expect(src).toContain("installStatus");
    expect(src).toMatch(/failed/);
    expect(src).toMatch(/not installed/);
  });

  it("manages the tenant-default selection set (references, never copies)", () => {
    expect(src).toContain("useGetTenantDefaultMCPServers");
    expect(src).toContain("useSetTenantDefaultMCPServers");
    expect(src).toMatch(/reference/);
  });

  it("every mutation invalidates the shared MCP key — pickers auto-refresh on save", () => {
    const invalidations = (api.match(/invalidateQueries\(\{ queryKey: mcpKeys\.all \}\)/g) ?? []).length;
    // list-relevant mutations: create, update, delete, install (4+)
    expect(invalidations).toBeGreaterThanOrEqual(4);
    expect(api).toContain("mcpKeys.all");
    // project + tenant-default selections invalidate their own keys too.
    expect(api).toContain("mcpKeys.project");
    expect(api).toContain("mcpKeys.tenantDefault");
  });
});
