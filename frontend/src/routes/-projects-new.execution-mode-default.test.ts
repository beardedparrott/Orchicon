import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

// Local is the default execution mode: containers are opt-in. The form's
// displayed default must equal the server's, or a user is shown one value and
// given another. These source-level pins guard the three places the create form
// decides the mode, plus the detail page's pre-fetch state.
describe("projects/new execution-mode default is local", () => {
  const src = fs.readFileSync(path.join(__dirname, "projects_.new.tsx"), "utf8");

  it("the schema defaults to local", () => {
    expect(src).toMatch(/executionMode:\s*z\.enum\(\["runtime",\s*"local"\]\)\.default\("local"\)/);
  });

  it("the form's defaultValues use local", () => {
    expect(src).toMatch(/executionMode:\s*"local"\s*as const/);
  });

  it("the select's fallback value is local", () => {
    expect(src).toMatch(/value=\{executionMode \?\? "local"\}/);
  });

  it("local is the option labelled default, runtime the opt-in", () => {
    expect(src).toMatch(/<option value="local">local — host process, not contained \(default\)<\/option>/);
    expect(src).toMatch(/<option value="runtime">runtime — always-container \(opt-in\)<\/option>/);
    expect(src).not.toMatch(/runtime — always-container \(default\)/);
  });
});

describe("projects/$id pre-fetch execution-mode state is local", () => {
  const src = fs.readFileSync(path.join(__dirname, "projects_.$id.tsx"), "utf8");

  it("draftExecutionMode starts at local", () => {
    expect(src).toMatch(/useState<"runtime" \| "local">\("local"\)/);
  });

  it("executionModeLabel delegates to the one UI mapping (local fallback)", () => {
    expect(src).toMatch(/function executionModeLabel\(mode: number \| undefined\): string \{\s*return protoToExecutionMode\(mode\);\s*\}/);
    expect(src).not.toMatch(/if \(mode === 2\) return "local";/);
  });

  it("the detail select labels local the default, runtime the opt-in", () => {
    expect(src).toMatch(/<option value="local">local — host process, not contained \(default\)<\/option>/);
    expect(src).toMatch(/<option value="runtime">runtime — always-container \(opt-in\)<\/option>/);
    expect(src).not.toMatch(/runtime — always-container \(default\)/);
  });
});
