// THE EXEMPTION MUST BE REACHABLE, in both clients.
//
// The operator: "I tried to kick off a feature and it denied me in the TUI saying that it has to
// have a workflow set, but that is incorrect. Parents should not have a workflow set in order to
// fire off the children."
//
// The rule was wrong in THREE places, and each one hid the next:
//
//   1. the server exempts a parent — it always did, and said why: "A sequence PARENT with children
//      is exempt: it is a container that contributes ordering only and never executes itself";
//   2. the GUI's checkbox was `disabled={!editWorkflowId}` — a HARD BLOCK, so on a parent the
//      operator could not tick it at all and the exemption could never run;
//   3. the TUI refused on the CREATE path, which the server takes no gate on — "The CREATE path
//      needs no gate — new items always start pending."
//
// A disabled control and a refusal look identical to the operator (the thing cannot be done), so
// both are asserted here: `autoStartBlocked` being permissive is worthless if the checkbox that
// feeds it cannot be ticked.
//
// The assertions are SOURCE-LEVEL, deliberately: the failure is an attribute expression and a
// conditional inside a large page component, and catching it through rendering would need a full
// router + query harness for a rule that is decidable from the source.

import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { describe, expect, it } from "vitest";

const read = (rel: string) => readFileSync(resolve(process.cwd(), rel), "utf8");

/** Strip comments so a MENTION in a rationale is not read as code. */
function code(src: string): string {
  return src
    .replace(/\{\/\*[\s\S]*?\*\/\}/g, "")
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/^\s*\/\/.*$/gm, "");
}

const PAGE = "src/routes/work-items_.$id.tsx";

describe("the GUI's auto-start control is reachable on a parent", () => {
  it("the checkbox is not disabled solely because no workflow is bound", () => {
    const c = code(read(PAGE));
    // The old hard block, verbatim. A parent must be able to tick the box.
    expect(
      c.includes("disabled={!editWorkflowId}"),
      "the auto-start checkbox is hard-disabled without a workflow — a parent can never tick it, " +
        "so the parent exemption can never run",
    ).toBe(false);
    // …and it must still be gated by the child fact, so a LEAF with no workflow stays blocked.
    expect(c).toContain("disabled={!editWorkflowId && !hasChildren}");
  });

  it("the advisory does not tell a parent it needs a workflow", () => {
    const c = code(read(PAGE));
    // The advisory is for a LEAF only; a parent gets its own explanation.
    expect(c).toContain("{!editWorkflowId && !hasChildren && (");
    expect(c).toContain("parent");
  });
});

describe("the rule itself matches the server in both clients", () => {
  it("the GUI's autoStartBlocked exempts a parent", () => {
    const c = code(read("src/components/work-items/work-item-meta.ts"));
    expect(c).toContain("hasChildren = false");
    expect(c).toContain("if (hasChildren) return false;");
  });

  it("the TUI's refusal takes the create path and the child fact", () => {
    const c = code(read("../internal/tui/screens/work/workitems.go"));
    // Create takes no gate; a parent is exempt; a leaf still refuses.
    expect(c).toContain("func autoStartRefusal(v map[string]string, isCreate, hasChildren bool) error");
    expect(c).toContain("if isCreate {");
    expect(c).toContain("if hasChildren {");
  });
});
