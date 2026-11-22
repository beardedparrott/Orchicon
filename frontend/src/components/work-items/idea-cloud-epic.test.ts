// The Idea Cloud container is VISIBLE in the active work-item views, and a promoted child
// hangs from it.
//
// Two earlier iterations of this file encoded the opposite belief — first that a promoted
// child vanished, then that the container should be hidden. The operator's requirement is
// explicit and is the one asserted here: after approving an idea, BOTH the "Idea Cloud" epic
// and the promoted task are visible in the active work items, with the task under the epic.

import { describe, expect, it } from "vitest";

import { WorkItemKind, WorkItemStatus } from "@/api/gen/orchicon/api/v1/work_item_pb";
import { IDEA_CLOUD_EPIC_TITLE } from "@/components/work-items/idea-cloud-epic";
import { ALL_KIND_VALUES, ALL_STATUS_VALUES } from "@/components/work-items/work-item-meta";
import { buildTreeData, findOrphanIds } from "@/components/work-items/dependency-utils";

function item(partial: Record<string, unknown> & { id: string; title: string }) {
  return {
    parentId: "",
    kind: WorkItemKind.TASK,
    status: WorkItemStatus.PENDING,
    description: "",
    ...partial,
  } as never;
}

describe("Idea Cloud container identity", () => {
  it("matches the server's title exactly (Go: workitem.IdeaCloudEpicTitle)", () => {
    // The identity the frontend mirrors must be the one the server resolves-or-creates,
    // or a rename silently changes which row the UI treats as the container.
    expect(IDEA_CLOUD_EPIC_TITLE).toBe("Idea Cloud");
  });
});

describe("after promotion, both rows are visible and correctly linked", () => {
  // The shape PromoteIdea now writes: the promoted item is a pending TASK parented to a
  // pending top-level EPIC titled "Idea Cloud".
  const container = item({
    id: "ic",
    title: IDEA_CLOUD_EPIC_TITLE,
    kind: WorkItemKind.EPIC,
    status: WorkItemStatus.PENDING,
  });
  const promoted = item({
    id: "p",
    title: "Promoted idea",
    kind: WorkItemKind.TASK,
    status: WorkItemStatus.PENDING,
    parentId: "ic",
  });
  const items = [container, promoted];

  it("the container is NOT filtered out of the active views", () => {
    // Requirement: the epic shows up in work items alongside the task.
    const data = buildTreeData(items as never, ALL_KIND_VALUES, ALL_STATUS_VALUES, "");
    const ids = data.treeItems.map((i) => (i as { id: string }).id);
    expect(ids).toContain("ic");
    expect(ids).toContain("p");
  });

  it("the promoted task is reachable UNDER the epic (not an orphan root)", () => {
    const orphans = findOrphanIds(items as never);
    // Its parent is present, so it must not be treated as an orphan.
    expect(orphans.has("p")).toBe(false);
    expect(orphans.size).toBe(0);
  });

  it("the tree renders the promoted task as a child of the container", () => {
    const data = buildTreeData(items as never, ALL_KIND_VALUES, ALL_STATUS_VALUES, "");
    const roots = data.treeItems.filter((i) => !(i as { parentId?: string }).parentId);
    const rootIds = roots.map((i) => (i as { id: string }).id);
    // The epic is the only root; the task is nested beneath it.
    expect(rootIds).toEqual(["ic"]);
    const child = data.treeItems.find((i) => (i as { id: string }).id === "p") as {
      parentId?: string;
    };
    expect(child?.parentId).toBe("ic");
  });
});

describe("legacy ideas with no parent still render (regression guard)", () => {
  it("an idea-state orphan promoted before the container existed is a root, not dropped", () => {
    // Ideas spawned before the container existed have parent_id NULL. If one of those is
    // promoted and the server's anchoring is bypassed, it must still be VISIBLE rather than
    // silently missing — the failure the operator reported first.
    const lonely = item({ id: "l", title: "Legacy promoted", parentId: "" });
    const data = buildTreeData([lonely] as never, ALL_KIND_VALUES, ALL_STATUS_VALUES, "");
    expect(data.treeItems.map((i) => (i as { id: string }).id)).toContain("l");
  });
});
