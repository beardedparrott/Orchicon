// -recurring-items-execution-mode.test.ts
//
// THE GAP THIS PINS. A recurring item is a work item, and a work item carries a per-item
// execution-mode override ("run this one locally" vs "run it in a container"). The regular work-item
// create and edit forms both offer it. The recurring forms offered only a RUNTIME IMAGE — so an
// automation could be pointed at a container while having no way to say "actually, run this one on my
// machine", and every automation was silently created with no override, inheriting its project.
//
// Worse, the read-only detail view printed `Runtime image: default (base)` with no mention of the mode,
// which describes a container that a local run never creates.
//
// These assertions are source-level on purpose: the pages are large route components wired to
// connect-rpc hooks, and what regressed was the PRESENCE of a control and its presence in the SAVE
// PAYLOAD — both visible in the source and cheap to assert, neither covered by rendering the page.

import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

const NEW_FORM = path.resolve(__dirname, "recurring-items_.new.tsx");
const EDIT_FORM = path.resolve(__dirname, "recurring-items_.$id.tsx");

const read = (p: string) => fs.readFileSync(p, "utf8");

describe("recurring item forms expose the execution mode", () => {
  it("the create form renders an ExecutionModeSelect", () => {
    const src = read(NEW_FORM);
    expect(src).toContain("ExecutionModeSelect");
    // Rendered, not merely imported.
    expect(src).toMatch(/<ExecutionModeSelect[\s\S]{0,200}onChange=\{setExecutionMode\}/);
  });

  it("the create form SENDS execution mode on create", () => {
    // A control whose value never reaches the request is worse than no control: the operator changes
    // it, saves, and nothing happens.
    expect(read(NEW_FORM)).toContain("executionMode: executionModeToProto(executionMode)");
  });

  it("the edit form renders an ExecutionModeSelect", () => {
    const src = read(EDIT_FORM);
    expect(src).toMatch(/<ExecutionModeSelect[\s\S]{0,300}onChange=\{setExecutionMode\}/);
  });

  it("the edit form SENDS execution mode on save", () => {
    expect(read(EDIT_FORM)).toContain("executionMode: executionModeToProto(executionMode)");
  });

  it("the edit form HYDRATES the existing mode when editing starts", () => {
    // Without this the control would open on "inherit" every time and silently clear a local override
    // the moment anything else on the form was saved.
    expect(read(EDIT_FORM)).toContain("setExecutionMode(executionModeChoiceOf(item))");
  });
});

describe("the read-only detail view tells the truth about the mode", () => {
  it("names the execution mode", () => {
    expect(read(EDIT_FORM)).toMatch(/Execution mode/);
  });

  it("says the image is unused for a local run", () => {
    // "default (base)" beside a local run describes a container that will never be created.
    const src = read(EDIT_FORM);
    expect(src).toMatch(/not used — a local run executes on the host/);
  });
});
