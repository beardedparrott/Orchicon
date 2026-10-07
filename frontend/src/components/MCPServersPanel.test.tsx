import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// MCPServersPanel is the ONE MCP surface for every scope. These assertions are
// the carried-forward form of the retired MCPServersTab.test.tsx (AC 2's
// regression pin) plus the panel's own shape (AC 1) and its two persistence
// targets (AC 3).
describe("MCPServersPanel (child 7, AC 1-3)", () => {
  const src = fs.readFileSync(path.join(__dirname, "MCPServersPanel.tsx"), "utf8");
  const api = fs.readFileSync(path.join(__dirname, "../api/mcpServers.ts"), "utf8");

  it("keeps the whole panel intact: Add + runtime hint, Configured servers, catalog, credentials", () => {
    expect(src).toContain("useMCPServerList");
    expect(src).toContain("useMCPCatalog");
    expect(src).toMatch(/Registry catalog/);
    expect(src).toMatch(/Configured servers/);
    // The Add button carries the runtime-availability hint (the panel's
    // `:236-255` contract).
    expect(src).toContain("useMCPRuntimes");
    expect(src).toContain("runtimeAvailable");
    expect(src).toMatch(/runtime missing/);
    // Configures-server cards carry Install / Edit / Delete + install-status and
    // secrets-stored badges.
    expect(src).toMatch(/Install/);
    expect(src).toMatch(/Edit/);
    expect(src).toMatch(/Delete/);
    expect(src).toContain("installStatus");
    expect(src).toMatch(/not installed/);
  });

  it("one-click catalog add prefills the add form", () => {
    expect(src).toContain("usePrefillMCPCatalogEntry");
    expect(src).toContain("handleCatalogAdd");
    expect(api).toContain("prefillMCPCatalogEntry");
    expect(src).toMatch(/prefills the add form/);
  });

  it("supports stdio and streamable HTTP transports with ${SECRET_NAME}", () => {
    expect(src).toContain("MCP_SERVER_TRANSPORT_STDIO");
    expect(src).toContain("MCP_SERVER_TRANSPORT_STREAMABLE_HTTP");
    expect(src).toMatch(/stdio/);
    expect(src).toMatch(/streamable HTTP/);
    expect(src).toContain("form.transport === MCPServerTransport.MCP_SERVER_TRANSPORT_STDIO");
    expect(src).toContain("${SECRET_NAME}");
  });

  it("auto-install is explicit and dry-run safe", () => {
    expect(src).toContain("useInstallMCPServer");
    expect(api).toContain("installMCPRuntime");
    expect(api).toContain("dryRun");
    expect(src).toMatch(/Installations are explicit/);
  });

  it("the credential control draws from the vault — a key/value pair is not an option", () => {
    // THE CONTRACT MOVED WITH THE OPERATOR'S RULE, and this is the assertion that changed: a credential is
    // SELECTED from the tenant secrets store and referenced as ${SECRET_NAME}, never typed onto a server.
    // ("on projects in the GUI … it does NOT allow you to select/add from the secrets vault. It is still
    // manually typed in key, value pair. We should not allow that option at all.")
    expect(src).toContain("useSecretList");
    expect(src).toContain("useCreateSecret");
    expect(src).toContain("useUpdateSecret");
    // A value field survives for ONE case only — creating a new secret, which is write-only by nature.
    expect(src).toContain('type="password"');
    expect(src).toMatch(/tenant secrets store/);
    // The mechanism that typed a key and a value straight onto the server is gone from the panel.
    expect(src).not.toContain("useSetMCPServerSecret");
  });

  it("has NO tenant-default tier: the checkbox, toggleDefault and the selection hooks are gone", () => {
    // The AC 2 regression pin, carried forward from the retired
    // MCPServersTab.test.tsx rather than dropped.
    expect(src).not.toContain("useGetTenantDefaultMCPServers");
    expect(src).not.toContain("useSetTenantDefaultMCPServers");
    expect(src).not.toContain("toggleDefault");
    // The header copy no longer claims entries are shared.
    expect(src).not.toContain("Tenant-scoped");
    expect(src).not.toMatch(/every consumer/);
    // The copy states the ABSENCE ("no tenant default") rather than offering one:
    // what must not exist is the MECHANISM, so pin that.
    expect(src).not.toMatch(/tenant-default checkbox/i);
    expect(src).not.toMatch(/make it the tenant default/);
    expect(api).not.toContain("tenantDefault");
  });

  it("states the honest consequence instead of hiding it", () => {
    const flat = src.replace(/\s+/g, " ");
    expect(flat).toMatch(/no longer be defined once and inherited by several projects/);
    expect(flat).toMatch(/one-click Add per scope is the mitigation/);
  });

  it("has all three scopes, and the worker-version scope writes NO owned row", () => {
    expect(src).toMatch(/\{ kind: "project"; projectId: string \}/);
    expect(src).toMatch(/\{ kind: "conversation"; conversationId: string \}/);
    expect(src).toMatch(/\{ kind: "workerVersion"; value: InlineMCP\[\]; onChange: \(next: InlineMCP\[\]\) => void \}/);
    // The inline mode reports through onChange and never calls a create/update
    // hook — a panel that wrote owned rows here would break version
    // immutability.
    expect(src).toContain("scope.onChange");
    expect(src).toContain("scope.kind === \"workerVersion\"");
  });

  it("the edit form ROUND-TRIPS env/headers, which the update replaces", () => {
    // The update sends replaceEnv/replaceHeaders, so an edit that started from
    // empty fields would silently ERASE the entry's env/headers. The row must
    // carry them and startEdit must seed the form from them.
    expect(src).toContain("env: keyValueText(r.env)");
    expect(src).toContain("headers: keyValueText(r.headers)");
    expect(src).toContain("env: s.env");
    expect(src).toContain("headers: s.headers");
    // And the pre-fix shape (blank env/headers on edit) must be gone.
    expect(src).not.toMatch(/args: \(r\.args \?\? \[\]\)\.join\("\\n"\),\s*env: ""/);
  });

  it("the worker-version mode fires NO unscoped (tenant-wide) list", () => {
    // An unscoped ListMCPServers IS the whole-tenant list the epic removed, so
    // the panel must switch the query OFF rather than run it and ignore it.
    expect(src).toContain("{ enabled: owned }");
    expect(api).toContain("enabled: opts?.enabled ?? true");
  });

  it("every mutation invalidates the shared MCP key, and the api is owner-scoped", () => {
    const invalidations = (api.match(/invalidateQueries\(\{ queryKey: mcpKeys\.all \}\)/g) ?? []).length;
    expect(invalidations).toBeGreaterThanOrEqual(4);
    expect(api).toContain("ownerProject");
    expect(api).toContain("ownerConversation");
    expect(api).not.toContain("mcpKeys.tenantDefault");
  });
});
