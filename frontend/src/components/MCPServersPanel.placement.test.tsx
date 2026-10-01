import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

const srcDir = path.join(__dirname, "..");
const read = (p: string) => fs.readFileSync(path.join(srcDir, p), "utf8");

// AC 5 + AC 7: the placements are EXACTLY these, each reachable, and the old
// elements are REMOVED rather than left beside the new one.
describe("MCP + skills placements (child 7, AC 5/7)", () => {
  it("project detail carries its OWN MCP panel and a Skill files browser", () => {
    const p = read("routes/projects_.$id.tsx");
    expect(p).toContain("<MCPServersPanel");
    expect(p).toContain('kind: "project"');
    expect(p).toContain("<FileBrowser");
    expect(p).toContain('title="Skill files"');
  });

  it("worker detail and worker new carry inline MCP + a Skill files browser", () => {
    for (const f of ["routes/workers_.$id.tsx", "routes/workers_.new.tsx"]) {
      const p = read(f);
      expect(p, f).toContain("<MCPServersPanel");
      expect(p, f).toContain('kind: "workerVersion"');
      expect(p, f).toContain("<FileBrowser");
      expect(p, f).toContain('title="Skill files"');
    }
  });

  it("Ask conversation is a HEADER DISCLOSURE beside the Grants disclosure, mirrored in the mobile sheet", () => {
    const p = read("routes/ask-orchicon.tsx");
    expect(p).toContain("<SessionGrants");
    expect(p).toContain("<ConversationScopeDisclosure");
    // The desktop header cluster AND the mobile sheet: two mounts.
    expect(p.match(/<ConversationScopeDisclosure/g)?.length ?? 0).toBeGreaterThanOrEqual(2);
    const d = read("components/ask/ConversationScopeDisclosure.tsx");
    expect(d).toContain("<MCPServersPanel");
    expect(d).toContain('kind: "conversation"');
    expect(d).toContain("<FileBrowser");
    expect(d).toContain('title="Skill files"');
  });

  it("NOTHING in Settings touches MCP any more", () => {
    const p = read("routes/settings.tsx");
    expect(p).not.toContain("MCPServersTab");
    expect(p).not.toContain("MCPServersPanel");
    expect(p).not.toContain('"mcp"');
  });
});

// AC 7: the old components are gone — no file anywhere still references them.
describe("removed GUI elements are gone (child 7, AC 7)", () => {
  const files: string[] = [];
  const walk = (dir: string) => {
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
      if (e.name === "node_modules" || e.name === "gen" || e.name === "dist") continue;
      const full = path.join(dir, e.name);
      if (e.isDirectory()) walk(full);
      else if (/\.tsx?$/.test(e.name) && !/\.test\.tsx?$/.test(e.name)) files.push(full);
    }
  };
  walk(srcDir);

  it("no file contains MCPPicker or MCPServersTab", () => {
    const offenders = files.filter((f) => {
      const s = fs.readFileSync(f, "utf8");
      // The panel's header comment names the retired component on purpose;
      // every real reference is what must be gone.
      return /MCPPicker|MCPServersTab/.test(s) && !f.endsWith("MCPServersPanel.tsx");
    });
    expect(offenders, offenders.join("\n")).toEqual([]);
  });

  it("there is exactly ONE MCP panel component — no second, simpler picker", () => {
    const components = fs
      .readdirSync(path.join(srcDir, "components"))
      .filter((n) => /MCP.*\.tsx$/.test(n) && !n.endsWith(".test.tsx"));
    expect(components).toEqual(["MCPServersPanel.tsx"]);
  });
});
