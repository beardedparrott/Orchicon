// The INLINE credential control: the worker-version scope's half of "a credential is selected, not typed".
//
// A worker version has no definition row, so a credential cannot be STORED against it — it has to be
// REFERENCED, and the reference is built into the spec's own env (stdio) or headers (HTTP). The reported
// asymmetry was that only the owned scopes had a Credentials card at all; here the same control exists at
// every scope, pointed at a different target.
//
// The repo ships no jsdom (see SessionGrants.test.tsx), so these assertions are on the markup server
// rendering emits — which is where the datalist suggestions and the button types actually live — plus a
// source scan for the one thing markup cannot show: the ORDER of the two writes.
import { describe, expect, it, vi } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import fs from "node:fs";
import path from "node:path";

vi.mock("@/api/mcpServers", () => {
  const idle = () => ({ mutateAsync: async () => ({}), isPending: false });
  return {
    useMCPServerList: () => ({ data: [], isLoading: false, error: null }),
    useMCPCatalog: () => ({ data: [] }),
    useMCPRuntimes: () => ({ data: { npx: true } }),
    useCreateMCPServer: idle,
    useUpdateMCPServer: idle,
    useDeleteMCPServer: idle,
    useInstallMCPServer: idle,
    useSetMCPServerSecret: idle,
    usePrefillMCPCatalogEntry: () => ({ mutateAsync: async () => ({ prefill: {} }), isPending: false }),
  };
});

// The tenant secrets store, read for its NAMES. A value never appears here because the API never returns
// one (see api/secrets.ts).
vi.mock("@/api/secrets", () => {
  const idle = () => ({ mutateAsync: async () => ({}), isPending: false });
  return {
    useSecretList: () => ({
      data: [
        {
          id: "sec-gh",
          name: "MCP_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN",
          description: "GitHub personal access token",
        },
      ],
      error: null,
    }),
    useCreateSecret: idle,
    useUpdateSecret: idle,
  };
});

const { MCPServersPanel } = await import("./MCPServersPanel");
import type { InlineMCP } from "./MCPServersPanel";

const src = fs.readFileSync(path.join(__dirname, "MCPServersPanel.tsx"), "utf8");

// datalistOptions returns the `value`s each <datalist> offers, in document order. The panel writes the
// KEY list first and the secret list second, so index 0 is the spec's own keys and index 1 is the store's
// names — the assertion below depends on that order deliberately, because the two must never be swapped.
function datalistOptions(html: string): string[][] {
  return html
    .split("<datalist")
    .slice(1)
    .map((block) => (block.split("</datalist>")[0].match(/value="([^"]*)"/g) ?? []).map((v) => v.slice(7, -1)));
}

function renderWorkerVersion(specs: InlineMCP[]): string {
  return renderToStaticMarkup(
    createElement(
      "form",
      { id: "draftForm", onSubmit: () => {} },
      createElement(MCPServersPanel, {
        scope: {
          kind: "workerVersion",
          value: specs,
          onChange: () => {},
        },
      }),
    ),
  );
}

describe("MCPServersPanel inline credential", () => {
  it("offers a credential control at the worker-version scope", () => {
    const html = renderWorkerVersion([
      { id: "gh", type: "stdio", command: ["npx", "-y", "server-github"], env: { GITHUB_PERSONAL_ACCESS_TOKEN: "" } },
    ]);
    expect(html).toContain("Credentials");
    expect(html).toContain("Attach");
    // The spec picker names the server the credential will be attached to.
    expect(html).toContain('value="gh"');
  });

  it("suggests the spec's OWN keys and the STORE's names — the two things to pick instead of type", () => {
    const html = renderWorkerVersion([
      { id: "gh", type: "stdio", command: ["npx", "-y", "server-github"], env: { GITHUB_PERSONAL_ACCESS_TOKEN: "" } },
    ]);
    const lists = datalistOptions(html);
    expect(lists.length).toBeGreaterThanOrEqual(2);
    expect(lists[0]).toContain("GITHUB_PERSONAL_ACCESS_TOKEN");
    expect(lists[1]).toContain("MCP_GITHUB_GITHUB_PERSONAL_ACCESS_TOKEN");
    // The stored secret suggestions explain themselves (the description rides the option's text).
    expect(html).toContain("GitHub personal access token");
  });

  it("suggests the spec's HEADER keys for a streamable-HTTP server, not its env", () => {
    const html = renderWorkerVersion([
      {
        id: "remote",
        type: "http",
        url: "https://mcp.example/sse",
        headers: { Authorization: "" },
        env: { MISTAKEN: "x" },
      },
    ]);
    const keys = datalistOptions(html)[0];
    expect(keys).toContain("Authorization");
    expect(keys).not.toContain("MISTAKEN");
  });

  it("shows no credential control when the version has no servers to attach one to", () => {
    const html = renderWorkerVersion([]);
    expect(html).not.toContain("Attach");
    // The Configured-servers empty state is what tells the operator what to do instead.
    expect(html).toContain("No MCP servers configured yet");
  });

  it("stores the value BEFORE the reference is written into the spec", () => {
    // The one thing markup cannot show. A reference is resolved when the worker runs
    // (mcpsettings.ResolveSecretRefs), so a version saved before its secret exists would fail every
    // session — the store write has to come first, exactly as the TUI's credential form orders them.
    const store = src.indexOf("await createSecret.mutateAsync");
    const attach = src.indexOf("writeInline(attachSecret(scope.value");
    expect(store).toBeGreaterThan(-1);
    expect(attach).toBeGreaterThan(-1);
    expect(store).toBeLessThan(attach);
  });

  it("writes through the caller's own array, never through the owned-row RPC", () => {
    // An inline spec has no row, so the owned path's SetMCPServerSecret (which addresses a row id) cannot
    // be what attaches this credential — the reference goes into the array the worker form owns.
    expect(src).toContain("writeInline(attachSecret(scope.value, specId, key, secret))");
    expect(src).toContain("useSecretList");
    expect(src).toContain("useUpdateSecret");
  });
});
