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
    const html = renderProject();
    // SEARCHABLE, and the options are the store's own names: the report asked for exactly this — "you
    // should be able to search through current available secrets or 'create new secret'".
    expect(html).toContain("Search stored secrets…");
    expect(html).toContain("GITHUB_TOKEN");
    expect(html).toContain("MCP_GITHUB_PAT");
    // Creating one is its own row, not a free-text field the control has to guess about.
    expect(html).toContain("Create a new secret…");
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

  it("is reachable FROM THE ADD/EDIT FORM, which is where the report came from", () => {
    // The follow-up report, verbatim: "Credentials are still something you type in. You should be able to
    // search through current available secrets or 'create new secret' from the MCP edit page in projects
    // and workers" — and again: "We need the ability to select/create the secret credentials from the MCP
    // edit page".
    //
    // Source-pinned because the placement is the claim: the card is rendered INSIDE the form block, so the
    // env field is no longer the only place a credential can go while a server is being defined. (The
    // other Credentials card sits below the server list, which does not exist yet at that moment.)
    const formStart = src.indexOf("{!readOnly && showForm && (");
    const formEnd = src.indexOf("Enabled", formStart);
    expect(formStart, "the add/edit form block").toBeGreaterThan(-1);
    expect(formEnd, "the form's Enabled control" ).toBeGreaterThan(formStart);
    const formBlock = src.slice(formStart, formEnd);
    expect(formBlock, "the credential control is not inside the add/edit form").toContain("<CredentialCard");
    expect(formBlock).toContain("compact");
    // …and the form's own env/headers fields stop advertising a hand-written reference, so the two do not
    // give opposite instructions.
    expect(formBlock).not.toContain("values may be ${SECRET_NAME}");
    expect(formBlock).toContain("non-secret values");
  });

  it("keeps the catalog provenance, so a declared key is offered and the row stays installable", () => {
    // A catalog pick must REMEMBER its slug: the credential control reads the entry's requiredEnv from it
    // (the declared key as the default), and the create must SEND it — the plane derives a row's
    // required_secrets, its Install control and its secrets-stored badge from catalog_slug. The panel used
    // to discard it on prefill and never send it, which left a catalog-added server indistinguishable from
    // a hand-written one.
    expect(src).toContain("catalogSlug: slug");
    expect(src).toContain("catalogSlug: form.catalogSlug");
    expect(src).toContain("catalogSlug: r.catalogSlug");
    expect(src).toMatch(/requiredEnv/);
  });

  it("searches the store rather than offering a free-text name", () => {
    // "you should be able to search through current available secrets or 'create new secret'". The
    // combobox's options are only ever REAL names; creating one is its own explicit row.
    expect(src).toContain("Search stored secrets…");
    expect(src).toContain("Create a new secret");
    expect(src).toContain("function SecretCombobox(");
  });
});
