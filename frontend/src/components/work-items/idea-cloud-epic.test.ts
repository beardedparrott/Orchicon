// The Idea Cloud container must not appear in the active work-item views, and hiding it
// must NOT hide a promoted child whose parent it is.
//
// This is the regression the operator hit twice: first a promoted item vanished from the
// tree, then — with the anchor in place — the container itself became the un-actionable
// row nobody expects to see in a project's hierarchy.

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

// Mirrors routes/work-items.tsx's predicate exactly.
function isIdeaCloudContainer(i: { kind: number; title: string; parentId?: string }) {
  return (
    i.kind === WorkItemKind.EPIC &&
    !i.parentId &&
    i.title.trim().toLowerCase() === IDEA_CLOUD_EPIC_TITLE.toLowerCase()
  );
}

describe("Idea Cloud container identity", () => {
  it("matches the server's title exactly (Go: workitem.IdeaCloudEpicTitle)", () => {
    // A drift here would make the filter target a different row than the one the
    // server resolves-or-creates, which is how a container re-appears in the list.
    expect(IDEA_CLOUD_EPIC_TITLE).toBe("Idea Cloud");
  });

  it("recognizes the container only as a TOP-LEVEL EPIC with that title", () => {
    const container = item({
      id: "ic",
      title: IDEA_CLOUD_EPIC_TITLE,
      kind: WorkItemKind.EPIC,
    });
    expect(isIdeaCloudContainer(container)).toBe(true);

    // A real epic that happens to be top-level is NOT the container.
    expect(
      isIdeaCloudContainer(item({ id: "e", title: "Checkout rewrite", kind: WorkItemKind.EPIC })),
    ).toBe(false);
    // A task named "Idea Cloud" is not the container.
    expect(
      isIdeaCloudContainer(item({ id: "t", title: IDEA_CLOUD_EPIC_TITLE, kind: WorkItemKind.TASK })),
    ).toBe(false);
    // An epic with that title but PARENTED is not the container (the container is top-level).
    expect(
      isIdeaCloudContainer(
        item({ id: "x", title: IDEA_CLOUD_EPIC_TITLE, kind: WorkItemKind.EPIC, parentId: "p" }),
      ),
    ).toBe(false);
    // Case/whitespace tolerant, matching the server's EqualFold+TrimSpace.
    expect(
      isIdeaCloudContainer(item({ id: "y", title: "  idea cloud ", kind: WorkItemKind.EPIC })),
    ).toBe(true);
  });
});

describe("hiding the container keeps promoted children visible", () => {
  const container = item({ id: "ic", title: IDEA_CLOUD_EPIC_TITLE, kind: WorkItemKind.EPIC });
  const promoted = item({ id: "p", title: "Promoted idea", parentId: "ic" });
  const realEpic = item({ id: "e", title: "Real epic", kind: WorkItemKind.EPIC });
  const realChild = item({ id: "c", title: "Real child", parentId: "e" });
  const all = [container, promoted, realEpic, realChild];

  const backlog = all.filter((i) => !isIdeaCloudContainer(i as never));

  it("drops the container but keeps its promoted child", () => {
    const ids = backlog.map((i) => (i as { id: string }).id);
    expect(ids).not.toContain("ic");
    expect(ids).toContain("p");
  });

  it("the promoted child becomes an ORPHAN — rendered as a root, never dropped", () => {
    // Hiding the container is exactly the situation the orphan rule exists for: the
    // child's parent is absent from the rendered set, so without findOrphanIds the tree
    // would lose it again (roots were `!parentId` and nothing else).
    const orphans = findOrphanIds(backlog as never);
    expect(orphans.has("p")).toBe(true);
    // A genuinely parented child of a VISIBLE epic is not an orphan.
    expect(orphans.has("c")).toBe(false);
  });

  it("the tree still renders the promoted item as a reachable row", () => {
    const data = buildTreeData(backlog as never, ALL_KIND_VALUES, ALL_STATUS_VALUES, "");
    const ids = data.treeItems.map((i) => (i as { id: string }).id);
    expect(ids).toContain("p");
    expect(ids).toContain("c");
    expect(ids).not.toContain("ic");
    // Nothing is present-but-unreachable.
    const present = new Set(ids);
    const orphans = findOrphanIds(data.treeItems as never);
    for (const i of data.treeItems as unknown as { id: string; parentId?: string }[]) {
      if (!i.parentId) continue;
      expect(present.has(i.parentId) || orphans.has(i.id)).toBe(true);
    }
  });
});
