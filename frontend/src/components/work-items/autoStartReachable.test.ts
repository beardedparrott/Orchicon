// AUTO-START IS NOT GATED ON A WORKFLOW — in either client, on either path.
//
// The operator, after three failed attempts to make this validation go away:
//
//	"We shouldn't have the validation at all no matter what the circumstance is. If it is a
//	 feature, epic, or even a task that has children then it shouldnd't be blocking me. Workflows
//	 need to be empty for a sequential workflow to kick off. We just need to remove that validation
//	 from creating new items AND editing current items that already exist."
//
// He is right, and the rule was wrong at its PREMISE rather than at its edges: the plane asks "is a
// workflow bound?" only at a TRANSITION into a runnable status (ready/assigned/scheduled/running),
// and it exempts a sequence parent even then. Both clients applied it to EVERY save, so they refused
// writes that transitioned nothing — which is why each earlier fix (exempt the parent, then exempt
// create) moved the boundary instead of removing it.
//
// These assert the rule is ABSENT, because the regression risk now is a re-introduction: three
// separate mechanisms expressed it (a save guard, a disabled control, and a coupling that cleared
// the value), and any one of them coming back would silently re-block the operator.
//
// The assertions are SOURCE-LEVEL, deliberately: the failure is a missing/extra attribute and a
// guard clause inside a large page component, and catching it through rendering would need a full
// router + query harness for rules decidable from the source.

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
const META = "src/components/work-items/work-item-meta.ts";

describe("the GUI does not gate auto-start on a workflow", () => {
  it("the save path has no auto-start guard", () => {
    const c = code(read(PAGE));
    expect(c).not.toContain("autoStartBlocked");
    expect(c).not.toContain("AUTO_START_NEEDS_WORKFLOW");
  });

  it("the auto-start checkbox is NEVER disabled", () => {
    const c = code(read(PAGE));
    // The old hard block, in every form it took.
    expect(c, "the checkbox is disabled somewhere").not.toContain("disabled={!editWorkflowId}");
    expect(c).not.toContain("disabled={!editWorkflowId && !hasChildren}");
    // It must still be a real controlled checkbox — absence of a disabled attr, not absence of
    // the control.
    expect(c).toContain('id="autoStart"');
  });

  it("the workflow select does NOT clear auto-start when emptied", () => {
    // The THIRD mechanism, and the one I missed on my first inventory: the binding and
    // auto-start are independent. Clearing one must not clear the other, because a
    // workflow-less parent is exactly how a sequential workflow is kicked off.
    //
    // SCOPED TO THE SELECT'S HANDLER on purpose. `setEditAutoStartWorkflow(false)` also appears
    // when the editor OPENS, resetting the box so a save never kicks off a run the operator did
    // not ask for — that is correct opt-in behaviour and must stay. What must not exist is the
    // coupling: clearing the value because the WORKFLOW was emptied.
    const c = code(read(PAGE));
    const sel = c.slice(c.indexOf('id="editWorkflow"') >= 0 ? c.indexOf('id="editWorkflow"') : c.indexOf("value={editWorkflowId}"));
    const handler = sel.slice(0, sel.indexOf("className"));
    expect(
      handler.includes("setEditAutoStartWorkflow"),
      `emptying the workflow still clears auto-start:\n${handler}`,
    ).toBe(false);
    // …and the handler still does its real job.
    expect(handler).toContain("setEditWorkflowId(e.target.value)");
  });

  it("no advisory tells the operator a workflow is required", () => {
    const c = code(read(PAGE));
    expect(c).not.toContain("Auto-start needs a workflow");
  });

  it("the rule's exports are gone from the metadata module", () => {
    const c = code(read(META));
    expect(c).not.toContain("autoStartBlocked");
    expect(c).not.toContain("AUTO_START_NEEDS_WORKFLOW");
  });
});

describe("the TUI does not gate auto-start on a workflow either", () => {
  it("has no refusal, no message and no coupling", () => {
    const c = code(read("../internal/tui/screens/work/workitems.go"));
    expect(c, "the refusal is back").not.toContain("autoStartRefusal");
    expect(c, "the refusal message is back").not.toContain("autoStartUnboundMsg");
    expect(c, "the coupling is back — it CLEARS auto-start when the workflow empties, which is "
      + "exactly the state a sequential workflow needs").not.toContain("clearAutoStartWhenUnbound");
  });

  it("the message text survives nowhere in the client", () => {
    const c = code(read("../internal/tui/screens/work/workitems.go"));
    expect(c).not.toContain("auto-start needs a workflow");
  });
});

// THE SCHEDULE CAN BE CLEARED — a value or an explicit clear, never a silent drop.
//
// The operator: "There is no way to clear a schedule on a work item." scheduledStartAt is an optional
// proto field, so an EMPTY input meant "unchanged": the page seeded the input from the item and an
// emptied field sent nothing at all, so the stored schedule round-tripped on every save. The save must
// now carry clearScheduledStartAt whenever the input is empty.
describe("the GUI can clear a schedule", () => {
  it("the save sends clearScheduledStartAt when the input is empty", () => {
    const c = code(read(PAGE));
    expect(c, "the save never sends clearScheduledStartAt — an emptied date is indistinguishable from 'unchanged'").toContain(
      "clearScheduledStartAt",
    );
    // The pair: a value when set, the clear when empty.
    expect(c).toContain("clearScheduledStartAt: editScheduledStartAt ? false : true");
  });

  it("the scheduled-start input is seeded from the item (so an untouched save cannot clear it)", () => {
    const c = code(read(PAGE));
    expect(c, "the schedule input must be seeded from item.scheduledStartAt").toContain("setEditScheduledStartAt(");
  });
});
