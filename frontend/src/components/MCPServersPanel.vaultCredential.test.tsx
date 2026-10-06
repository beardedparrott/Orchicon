// The OWNED scopes' (project / conversation) half of the credential contract.
//
// THE REPORT THIS PINS, verbatim: "on projects in the GUI, it does add the servers but it does NOT allow
// you to select/add from the secrets vault. It is still manually typed in key, value pair. We should not
// allow that option at all. ALL SECRETS should be driven from the secrets that can be stored on the
// platform."
//
// The old markup did exactly what was reported: a free-text `Env var name (e.g. GITHUB_TOKEN)` input next
// to a free-text `Secret value (write-only)` input, with the key/value POSTed at the row. Its replacement
// PICKS both — the key from what the server declares (a catalog entry's requiredEnv) and what it already
// carries, the secret from the store's own names — and writes a ${SECRET_NAME} reference into the row's
// env/headers instead of a value.
//
// The three assertions that matter are (1) the DECLARED key is offered rather than retyped, (2) the
// STORE's names are offered, and (3) NO value field renders at rest — the value field exists only once the
// operator explicitly chose to store a new secret.
import { describe, expect, it, vi } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import fs from "node:fs";
import path from "node:path";

import { MCPServerTransport } from "@/api/gen/orchicon/api/v1/mcp_server_pb";

// A project-owned GitHub row added from the catalog: the plane then requires the credential key to be a
// key the entry declares (a catalog requiredEnv) or already has, which is exactly what the picker offers.
const ROW = {
  id: "srv-gh",
  name: "GitHub",
  transport: MCPServerTransport.MCP_SERVER_TRANSPORT_STDIO,
  catalogSlug: "github",
  enabled: true,
  command: "npx",
  args: ["-y", "@modelcontextprotocol/server-github"],
  env: { GITHUB_PERSONAL_ACCESS_TOKEN: "" },
  headers: {},
  url: "",
  installStatus: 0,
  hasSecretStored: false,
};

vi.mock("@/api/mcpServers", () => {
  const idle = () => ({ mutateAsync: async () => ({}), isPending: false });
  return {
    useMCPServerList: () => ({ data: [ROW], isLoading: false, error: null }),
    useMCPCatalog: () => ({
      data: [
        {
          slug: "github",
          displayName: "GitHub",
          installMechanism: "npx",
          transport: "stdio",
          requiredEnv: ["GITHUB_PERSONAL_ACCESS_TOKEN"],
        },
      ],
    }),
    useMCPRuntimes: () => ({ data: { npx: true } }),
    useCreateMCPServer: idle,
    useUpdateMCPServer: idle,
    useDeleteMCPServer: idle,
    useInstallMCPServer: idle,
    usePrefillMCPCatalogEntry: () => ({ mutateAsync: async () => ({ prefill: {} }), isPending: false }),
  };
});

// The vault, read for its NAMES — the thing the report said could not be selected from.
vi.mock("@/api/secrets", () => {
  const idle = () => ({ mutateAsync: async () => ({}), isPending: false });
  return {
    useSecretList: () => ({
      data: [
        { id: "sec-1", name: "GITHUB_TOKEN", description: "shared GitHub token" },
        { id: "sec-2", name: "MCP_GITHUB_PAT", description: "" },
      ],
      error: null,
    }),
    useCreateSecret: idle,
    useUpdateSecret: idle,
  };
});

const { MCPServersPanel } = await import("./MCPServersPanel");

const src = fs.readFileSync(path.join(__dirname, "MCPServersPanel.tsx"), "utf8");

function selectOptions(html: string): string[][] {
  return html
    .split("<select")
    .slice(1)
    .map((block) => (block.split("</select>")[0].match(/value="([^"]*)"/g) ?? []).map((v) => v.slice(7, -1)));
}

function renderProject(): string {
  return renderToStaticMarkup(
    createElement(MCPServersPanel, { scope: { kind: "project", projectId: "proj-1" } }),
  );
}

describe("MCPServersPanel owned-scope credentials (vault-driven)", () => {
  it("offers the VAULT's names — the thing the report could not select from", () => {
    const selects = selectOptions(renderProject());
    expect(selects.length).toBeGreaterThanOrEqual(2);
    const secrets = selects[selects.length - 1];
    expect(secrets).toContain("GITHUB_TOKEN");
    expect(secrets).toContain("MCP_GITHUB_PAT");
    // Storing a new one is the single escape hatch, and it is explicit.
    expect(secrets).toContain("__new__");
  });

  it("offers the server's DECLARED key first, so the right one is the default rather than a retype", () => {
    const keys = selectOptions(renderProject())[0];
    expect(keys).toContain("GITHUB_PERSONAL_ACCESS_TOKEN");
  });

  it("renders NO value field at rest — a credential cannot be typed onto a server", () => {
    // The strongest form of the report's "we should not allow that option at all": at rest there is
    // nothing to type a value into. It appears only after choosing "Store a new secret…", and even then it
    // goes to the secrets store, never into the server's config.
    const html = renderProject();
    expect(html).not.toContain('type="password"');
    expect(html).not.toContain("Secret value");
    // The card does render (so the absence above is not just an unrendered control).
    expect(html).toContain("Credentials");
    expect(html).toContain("Attach");
  });

  it("writes a REFERENCE into the row, never a value", () => {
    // The owned target updates the row's env/headers with ${NAME} through the MCP service — the same
    // reference an inline spec gets, resolved at session time by mcpsettings.ResolveSecretRefs.
    expect(src).toContain("withReference(row.env, row.headers, isHTTP, draft.key, draft.secretName)");
    expect(src).toContain("replaceEnv: true");
    expect(src).toContain("replaceHeaders: true");
  });
});
