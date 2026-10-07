// MCPServersPanel is mounted INSIDE a <form> on both worker routes
// (routes/workers_.$id.tsx renders it inside the draft-version form, whose
// onSubmit saves the version and calls setEditing(false)).
//
// A bare <button> with no `type` attribute is a SUBMIT button, so every one of
// the panel's controls used to submit the host form: the reported bug was the
// Registry catalog "Add" bouncing the operator out of edit mode with no server
// added (MCPServersPanel.tsx's Add/Create/Cancel/Install/Edit/Delete/catalog
// Add/Save-secret are all `<Button>`, i.e. `<button>` from components/ui/button).
//
// The invariant pinned here is the general one, because the panel is ONE of the
// components in that form: NOTHING this component renders may submit a form it
// did not author. The repo ships no jsdom (see SessionGrants.test.tsx), so the
// contract is asserted on the markup that server rendering emits — which is
// exactly where `type` lives.

import { describe, expect, it, vi } from "vitest";
import { createElement, type ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import fs from "node:fs";
import path from "node:path";

import type { MCPScope } from "./MCPServersPanel";

// The panel's data hooks are mocked so the render is deterministic and offline.
// The catalog carries an entry WITH a required secret, because the catalog "Add"
// (the control in the report) only renders when the catalog is non-empty.
vi.mock("@/api/mcpServers", () => {
  const idle = () => ({ mutateAsync: async () => ({}), isPending: false });
  return {
    useMCPServerList: () => ({ data: [], isLoading: false, error: null }),
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
    usePrefillMCPCatalogEntry: () => ({
      mutateAsync: async () => ({ prefill: { name: "GitHub", command: "npx", args: [] } }),
      isPending: false,
    }),
  };
});

// The tenant secrets store, so the worker-version placement's credential card renders (it is one of the
// components in this form, and its Attach button has to be covered by the invariant below).
vi.mock("@/api/secrets", () => {
  const idle = () => ({ mutateAsync: async () => ({}), isPending: false });
  return {
    useSecretList: () => ({ data: [{ id: "sec-gh", name: "MCP_GITHUB_TOKEN", description: "" }], error: null }),
    useCreateSecret: idle,
    useUpdateSecret: idle,
  };
});

const { MCPServersPanel } = await import("./MCPServersPanel");
const { Button } = await import("./ui/button");
const workerRoute = fs.readFileSync(
  path.join(__dirname, "../routes/workers_.$id.tsx"),
  "utf8",
);
const workerNewRoute = fs.readFileSync(
  path.join(__dirname, "../routes/workers_.new.tsx"),
  "utf8",
);

// renderInsideForm is the placement that matters: the panel's controls as a
// browser sees them on a worker page.
function renderInsideForm(node: ReactNode): string {
  return renderToStaticMarkup(
    createElement("form", { id: "draftForm", onSubmit: () => {} }, node),
  );
}

// buttonTags returns every emitted <button …> opening tag.
function buttonTags(html: string): string[] {
  return html.match(/<button\b[^>]*>/g) ?? [];
}

describe("MCPServersPanel inside a host form", () => {
  it("is actually mounted inside the worker routes' form (the premise of this suite)", () => {
    for (const route of [workerRoute, workerNewRoute]) {
      const formStart = route.indexOf("<form");
      const formEnd = route.indexOf("</form>");
      const panel = route.indexOf("<MCPServersPanel");
      expect(formStart, "worker route has a form").toBeGreaterThan(-1);
      expect(formEnd, "worker route closes its form").toBeGreaterThan(formStart);
      expect(panel).toBeGreaterThan(formStart);
      expect(panel).toBeLessThan(formEnd);
    }
  });

  it("emits the catalog Add control this suite is guarding", () => {
    const html = renderInsideForm(
      createElement(MCPServersPanel, {
        scope: { kind: "workerVersion", value: [], onChange: () => {} },
      }),
    );
    expect(html).toContain("Registry catalog");
    expect(buttonTags(html).length).toBeGreaterThan(0);
  });

  it("renders NO submit button — nothing here may submit the draft form", () => {
    // Every placement the panel has: the worker-version scope is the one the
    // report came from, and the other two prove the fix is not worker-only.
    // The worker-version scope carries a SPEC, because its credential card (an inline reference into the
    // spec's own env) renders only when there is something to attach one to — and that card's Attach
    // button is a button in this form like any other.
    const scopes: MCPScope[] = [
      {
        kind: "workerVersion",
        value: [{ id: "gh", type: "stdio", command: ["npx", "-y", "server-github"], env: { TOKEN: "" } }],
        onChange: () => {},
      },
      { kind: "project", projectId: "proj-1" },
      { kind: "conversation", conversationId: "conv-1" },
    ];
    for (const scope of scopes) {
      const html = renderInsideForm(createElement(MCPServersPanel, { scope }));
      expect(html).not.toContain('type="submit"');
      // …and positively: every button is explicitly a non-submit button, so a
      // future control added here is safe by construction.
      for (const tag of buttonTags(html)) {
        expect(tag, `button without type=button: ${tag}`).toContain('type="button"');
      }
    }
  });

  it("keeps every branch safe: the Button primitive defaults to a non-submit type", () => {
    // The render above only covers the branches the three scopes reach (the add
    // form, the row actions and the Credentials card are conditional). The panel
    // is safe here because the PRIMITIVE is: an untyped <Button> is type="button",
    // and a submit has to be asked for by name — which every form in this app
    // already does.
    const bare = renderToStaticMarkup(createElement(Button, null, "Create"));
    expect(bare).toContain('type="button"');
    const submit = renderToStaticMarkup(
      createElement(Button, { type: "submit" }, "Save"),
    );
    expect(submit).toContain('type="submit"');
  });

  it("no <Button> anywhere relies on HTML's implicit submit default", () => {
    // A same-file <form> is the only place an untyped <Button> could be a submit
    // today: a CROSS-file one (this panel inside the worker form) is exactly what
    // the tests above cover. A Button that MEANS to submit must say so; one that
    // sits in a form without saying so is the defect this suite exists for.
    const offenders: string[] = [];
    for (const rel of tsxFiles()) {
      const body = fs.readFileSync(path.join(__dirname, "..", rel), "utf8");
      let depth = 0;
      for (const m of body.matchAll(/<form\b|<\/form>|<Button\b/g)) {
        if (m[0] === "<form") depth += 1;
        else if (m[0] === "</form>") depth -= 1;
        else if (depth > 0) {
          const tag = openingTag(body, m.index ?? 0);
          if (!/\btype=/.test(tag)) {
            offenders.push(`${rel}:${body.slice(0, m.index).split("\n").length}`);
          }
        }
      }
    }
    expect(offenders).toEqual([]);
  });
});

// openingTag returns the whole `<Button ...>` opening tag that starts at pos.
function openingTag(body: string, pos: number): string {
  let i = pos + "<Button".length;
  let braces = 0;
  for (; i < body.length; i += 1) {
    const c = body[i];
    if (c === "{") braces += 1;
    else if (c === "}") braces -= 1;
    else if (c === ">" && braces === 0) break;
  }
  return body.slice(pos, i + 1);
}

// tsxFiles lists every source .tsx under src/ (tests and generated code excluded).
function tsxFiles(): string[] {
  const out: string[] = [];
  const walk = (dir: string) => {
    for (const e of fs.readdirSync(path.join(__dirname, "..", dir), { withFileTypes: true })) {
      if (["node_modules", "gen", "dist"].includes(e.name)) continue;
      const rel = dir === "" ? e.name : `${dir}/${e.name}`;
      if (e.isDirectory()) walk(rel);
      else if (e.name.endsWith(".tsx") && !e.name.endsWith(".test.tsx")) out.push(rel);
    }
  };
  walk("");
  return out;
}
