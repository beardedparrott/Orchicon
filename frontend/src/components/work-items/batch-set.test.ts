// Unit tests for the bulk `workflow_id` + `runtime_image` set (batch-set.ts).
//
// The load-bearing property, proved rather than assumed, is INDEPENDENCE: an untouched key is
// OMITTED from the request, so a workflow-only set cannot clobber a runtime image (and vice versa).
// `UpdateWorkItemRequest`'s two fields are `optional` — unset means unchanged, empty means clear.

import { describe, expect, it } from "vitest";

import type {
  Workflow,
  WorkflowVersion,
} from "@/api/gen/orchicon/api/v1/workflow_pb";
import { WorkflowStatus } from "@/api/gen/orchicon/api/v1/workflow_pb";
import {
  SET_CLEAR,
  SET_SKIP,
  buildSetTarget,
  bulkSetConfirmText,
  formatSetResult,
  isRunnableWorkflow,
} from "@/components/work-items/batch-set";

function wf(id: string, status: WorkflowStatus = WorkflowStatus.PUBLISHED): Workflow {
  return { id, name: id, status } as unknown as Workflow;
}

function ver(steps: string): WorkflowVersion {
  return { id: "v1", workflowId: "w", version: 1, steps } as unknown as WorkflowVersion;
}

describe("buildSetTarget", () => {
  it("omits the untouched field entirely (a workflow-only set cannot touch runtime images)", () => {
    const target = buildSetTarget("wf-1", SET_SKIP);
    expect(target.workflowId).toBe("wf-1");
    expect("runtimeImage" in target).toBe(false);
    expect(Object.keys(target)).toEqual(["workflowId"]);
  });

  it("omits the workflow when only the image is set (the mirror image)", () => {
    const target = buildSetTarget(SET_SKIP, "orchicon/go:1.2");
    expect(target.runtimeImage).toBe("orchicon/go:1.2");
    expect("workflowId" in target).toBe(false);
    expect(Object.keys(target)).toEqual(["runtimeImage"]);
  });

  it("clearing sends the documented empty value for BOTH fields", () => {
    const both = buildSetTarget(SET_CLEAR, SET_CLEAR);
    expect(both).toEqual({ workflowId: "", runtimeImage: "" });
    // present-and-empty, NOT absent: absent would mean "leave it alone".
    expect("workflowId" in both).toBe(true);
    expect("runtimeImage" in both).toBe(true);
  });

  it("sends an empty target when both pickers are left unchanged (nothing to write)", () => {
    const target = buildSetTarget(SET_SKIP, SET_SKIP);
    expect(target).toEqual({});
    expect(Object.keys(target)).toHaveLength(0);
  });

  it("sets both when both are chosen", () => {
    expect(buildSetTarget("wf-1", "orchicon/go:1.2")).toEqual({
      workflowId: "wf-1",
      runtimeImage: "orchicon/go:1.2",
    });
  });
});

describe("isRunnableWorkflow", () => {
  it("rejects a DRAFT workflow even with steps", () => {
    expect(isRunnableWorkflow(wf("a", WorkflowStatus.DRAFT), ver('[{"id":"s1"}]'))).toBe(false);
  });

  it("rejects a PUBLISHED workflow with no steps", () => {
    expect(isRunnableWorkflow(wf("a"), ver("[]"))).toBe(false);
    expect(isRunnableWorkflow(wf("a"), ver(""))).toBe(false);
    expect(isRunnableWorkflow(wf("a"), undefined)).toBe(false);
  });

  it("rejects a malformed steps document (over-restriction is the safe direction)", () => {
    expect(isRunnableWorkflow(wf("a"), ver("{not json"))).toBe(false);
  });

  it("accepts PUBLISHED and DEPRECATED workflows that have steps", () => {
    expect(isRunnableWorkflow(wf("a"), ver('[{"id":"s1"}]'))).toBe(true);
    expect(
      isRunnableWorkflow(wf("b", WorkflowStatus.DEPRECATED), ver('[{"id":"s1"}]')),
    ).toBe(true);
  });
});

describe("formatSetResult", () => {
  it("counts the partial failure rather than claiming a clean sweep", () => {
    const msg = formatSetResult(3, 5, [
      { title: "Alpha", reason: "not found" },
      { title: "Beta", reason: "permission denied" },
    ]);
    expect(msg).toContain("set 3 of 5");
    expect(msg).toContain("2 rejected");
    expect(msg).toContain('"Alpha"');
  });

  it("says so plainly when nothing failed", () => {
    expect(formatSetResult(4, 4, [])).toBe("set 4 of 4 — all applied");
  });
});

describe("bulkSetConfirmText", () => {
  it("names the count and the values", () => {
    const text = bulkSetConfirmText(
      3,
      { workflowId: "wf-1", runtimeImage: "orchicon/go:1.2" },
      "Fanout",
      0,
    );
    expect(text).toContain("3 work items");
    expect(text).toContain("Fanout");
    expect(text).toContain("orchicon/go:1.2");
  });

  it("names a clearing as a clearing, not as a value", () => {
    const text = bulkSetConfirmText(2, { workflowId: "", runtimeImage: "" }, "", 0);
    expect(text).toContain("cleared (unbound)");
    expect(text).toContain("cleared (base image)");
  });

  it("calls out that a sequence parent's own binding is INERT", () => {
    const text = bulkSetConfirmText(4, { workflowId: "wf-1" }, "Fanout", 2);
    expect(text).toContain("SEQUENCE PARENTS");
    expect(text).toContain("INERT");
    // …and says nothing about parents when there are none.
    expect(bulkSetConfirmText(4, { workflowId: "wf-1" }, "Fanout", 0)).not.toContain("INERT");
  });
});
