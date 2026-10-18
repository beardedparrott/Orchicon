// Where the local-mode disclosure is WIRED. The copy lives once (LocalModeDisclosure), but a
// surface that forgets to render it silently loses the disclosure exactly where the operator
// makes the choice — so the three call sites are pinned by source assertion (the repo idiom
// for route-level contracts; see -diff-rail-width-wiring.test.ts).
import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

const create = fs.readFileSync(path.join(__dirname, "projects_.new.tsx"), "utf8");
const edit = fs.readFileSync(path.join(__dirname, "projects_.$id.tsx"), "utf8");
const workItem = fs.readFileSync(path.join(__dirname, "work-items_.new.tsx"), "utf8");

describe("local mode disclosure is rendered where the mode is chosen", () => {
  it("project create renders it for a local selection", () => {
    expect(create).toContain('import { LocalModeDisclosure } from "@/components/LocalModeDisclosure"');
    expect(create).toContain('{executionMode === "local" && <LocalModeDisclosure />}');
  });

  it("project edit renders it from the DRAFT mode, before saving", () => {
    expect(edit).toContain('import { LocalModeDisclosure } from "@/components/LocalModeDisclosure"');
    expect(edit).toContain('{draftExecutionMode === "local" && <LocalModeDisclosure />}');
    // The consequence must be visible pre-save, not only once persisted: the render site
    // sits above the handler that commits the mode.
    expect(edit.indexOf("{draftExecutionMode === \"local\" && <LocalModeDisclosure />}")).toBeLessThan(
      edit.indexOf("setSavingRuntime(true)"),
    );
  });

  it("the work-item form renders it when the selected project is local", () => {
    expect(workItem).toContain('import { LocalModeDisclosure } from "@/components/LocalModeDisclosure"');
    expect(workItem).toContain('protoToExecutionMode(selectedProject?.executionMode) === "local"');
    expect(workItem).toContain("<LocalModeDisclosure />");
  });

  it("the mode control never labels local as contained or fenced", () => {
    for (const src of [create, edit]) {
      expect(src).not.toContain("in-process (fenced)");
      expect(src).toContain("local — host process, not contained");
    }
  });
});
