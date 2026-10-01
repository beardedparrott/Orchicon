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
