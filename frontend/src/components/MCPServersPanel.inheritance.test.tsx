import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// AC 6: INHERITANCE IS VISIBLE. Under pure union a conversation or worker
// silently gains the project's servers, so the panel renders them READ-ONLY,
// labelled with their source, above the scope's own editable additions. At the
// project scope (the root) only its own are shown.
describe("MCPServersPanel inheritance (child 7, AC 6)", () => {
  const src = fs.readFileSync(path.join(__dirname, "MCPServersPanel.tsx"), "utf8");

  it("renders the project's contributed servers read-only, labelled with the source", () => {
    expect(src).toContain("InheritedServers");
    expect(src).toMatch(/Inherited from project/);
    expect(src).toContain("inheritedFrom");
    // Read-only: the inherited list is NOT the editable panel.
    const i = src.indexOf("function InheritedServers");
    expect(i).toBeGreaterThan(-1);
    const body = src.slice(i, src.indexOf("\n}\n", i));
    expect(body).not.toContain("useCreateMCPServer");
    expect(body).not.toContain("useUpdateMCPServer");
    expect(body).not.toContain("useDeleteMCPServer");
  });

  it("shows it only when inheritedFrom is set, and never at the project root", () => {
    expect(src).toContain("inheritedFrom && scope.kind !== \"project\"");
    expect(src).toMatch(/\{inheritedFrom && scope\.kind !== "project" && \(/);
  });

  it("the owner filter IS the inheritance query — no client-side union", () => {
    // The inherited list is useMCPServerList({ projectId }) and nothing else:
    // a client-side union would be a second resolution rule beside the server.
    const i = src.indexOf("function InheritedServers");
    const body = src.slice(i, src.indexOf("\n}\n", i));
    expect(body).toContain("useMCPServerList({ projectId })");
  });
});

// AC 6 ALSO COVERS SKILL FILES. The project's contributed skill files are rendered
// read-only and named at the scopes that inherit them, ABOVE the scope's own — the same rule
// the MCP panel follows, and the same component everywhere.
describe("inherited project SKILL FILES are visible (child 7, AC 6)", () => {
  const read = (p: string) => fs.readFileSync(path.join(__dirname, "..", p), "utf8");
  const inherited = fs.readFileSync(path.join(__dirname, "InheritedSkillFiles.tsx"), "utf8");

  it("the component is read-only: no mutating hook, no selection control", () => {
    expect(inherited).toMatch(/Inherited skill files from project/);
    expect(inherited).toMatch(/read-only at this scope/);
    // It names its source and is NOT the FileBrowser (no picker is written).
    expect(inherited).not.toContain("useMutation");
    expect(inherited).not.toContain("FileBrowser");
    expect(inherited).not.toContain("onChange");
  });

  it("the conversation scope renders it ABOVE its own skill files", () => {
    const d = read("components/ask/ConversationScopeDisclosure.tsx");
    expect(d).toContain("<InheritedSkillFiles");
    expect(d.indexOf("<InheritedSkillFiles")).toBeLessThan(d.indexOf("<FileBrowser"));
  });

  it("both worker-version placements render it ABOVE their own skill files", () => {
    for (const f of ["routes/workers_.$id.tsx", "routes/workers_.new.tsx"]) {
      const p = read(f);
      expect(p, f).toContain("<InheritedSkillFiles");
      expect(p.indexOf("<InheritedSkillFiles"), f).toBeLessThan(p.indexOf("<FileBrowser"));
    }
  });

  it("the project scope (the root) does NOT render an inherited block", () => {
    const p = read("routes/projects_.$id.tsx");
    expect(p).not.toContain("InheritedSkillFiles");
  });
});
