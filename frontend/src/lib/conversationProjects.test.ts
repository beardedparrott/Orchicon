// conversationProjects.test.ts — the conversations sidebar's PROJECT level, at the seams where it can be wrong.
//
// The operator: "We need to make a second higher level in organization for conversations. It should be another
// drop down where all of the conversations are associated with Projects in a parent category. For every project
// that is created (active or otherwise), there should be a list that can be dragged to and also created from."
//
// The sidebar itself is a route component this test setup cannot render, so the logic that decides WHERE a drop
// lands and HOW conversations group was pulled into lib/conversationProjects. That logic is what these tests
// pin — and the drop routing is the one that matters most, because getting it wrong moves a conversation into
// the wrong thing (or unassigns it) with no error anywhere.

import { describe, expect, it } from "vitest";

import { ProjectStatus } from "@/api/gen/orchicon/api/v1/project_pb";
import {
  PROJECT_DROP_PREFIX,
  groupConversationsByProject,
  isArchivedProject,
  projectDropId,
  projectIdFromDropId,
  projectStatusWord,
} from "@/lib/conversationProjects";

describe("project drop ids", () => {
  it("round-trips a project id", () => {
    expect(projectDropId("p-1")).toBe("proj:p-1");
    expect(projectIdFromDropId(projectDropId("p-1"))).toBe("p-1");
  });

  it("distinguishes the UNASSIGNED project from a non-project target", () => {
    // "" is the unassign gesture — a real, intentional move.
    expect(projectIdFromDropId(projectDropId(""))).toBe("");
    // A category folder id, or the uncategorized zone, is NOT a project: null, so the caller falls through to
    // the category path. Returning "" here would unassign a conversation on every drop onto a folder.
    expect(projectIdFromDropId("cat-abc")).toBeNull();
    expect(projectIdFromDropId("__uncategorized__")).toBeNull();
  });

  it("does not mistake a category id that merely contains the prefix", () => {
    // A prefix match must be a PREFIX match. An id that happens to contain "proj:" mid-string is not a project.
    expect(projectIdFromDropId("cat:proj:x")).toBeNull();
  });

  it("keeps the prefix distinct from the uncategorized sentinel", () => {
    expect(PROJECT_DROP_PREFIX).not.toBe("__uncategorized__");
    expect(projectDropId("")).not.toBe("__uncategorized__");
  });
});

describe("grouping conversations by project", () => {
  it("buckets each conversation under its project, in list order", () => {
    const got = groupConversationsByProject([
      { id: "c1", projectId: "p-1" },
      { id: "c2", projectId: "p-2" },
      { id: "c3", projectId: "p-1" },
    ]);
    expect(got.get("p-1")).toEqual(["c1", "c3"]);
    expect(got.get("p-2")).toEqual(["c2"]);
  });

  it("puts unassigned conversations under the empty key", () => {
    const got = groupConversationsByProject([
      { id: "c1", projectId: "" },
      { id: "c2" }, // absent projectId is the same state as ""
    ]);
    expect(got.get("")).toEqual(["c1", "c2"]);
  });

  it("KEEPS a conversation whose project is gone", () => {
    // The column has no foreign key, so a conversation can outlive its project. If the grouping dropped it, the
    // chat would be unreachable from the sidebar entirely — the worst outcome, and a silent one.
    const got = groupConversationsByProject([{ id: "orphan", projectId: "p-deleted" }]);
    expect(got.get("p-deleted")).toEqual(["orphan"]);
    expect([...got.values()].flat()).toContain("orphan");
  });

  it("handles an empty or missing list", () => {
    expect(groupConversationsByProject([]).size).toBe(0);
    expect(groupConversationsByProject(undefined).size).toBe(0);
  });
});

describe("archived projects", () => {
  // ⚠️ THE NUMERIC ENUM IS THE CASE THAT MATTERS, and the first version of this file did not test it. The
  // client receives a NUMBER (ACTIVE = 2, ARCHIVED = 4), and a string-only comparison classifies 2 as "not
  // active" — every ACTIVE project would have been marked archived in the sidebar. The type error at the call
  // site is what surfaced it; these assertions are what will stop it coming back.
  it("classifies the NUMERIC proto enum correctly", () => {
    expect(isArchivedProject(ProjectStatus.ACTIVE)).toBe(false);
    expect(isArchivedProject(ProjectStatus.ARCHIVED)).toBe(true);
    expect(isArchivedProject(ProjectStatus.PAUSED)).toBe(true);
    expect(isArchivedProject(ProjectStatus.DRAFTING)).toBe(true);
    expect(isArchivedProject(ProjectStatus.DELETED)).toBe(true);
    // UNSPECIFIED must NOT be marked: calling a working project archived is the worse error.
    expect(isArchivedProject(ProjectStatus.UNSPECIFIED)).toBe(false);
  });

  it("still recognises both string spellings", () => {
    // The proto enum's name, and the server's domain word — both reach this layer depending on origin.
    expect(isArchivedProject("PROJECT_STATUS_ARCHIVED")).toBe(true);
    expect(isArchivedProject("archived")).toBe(true);
    expect(isArchivedProject("paused")).toBe(true);
  });

  it("does NOT mark an active or unknown project as archived", () => {
    expect(isArchivedProject("PROJECT_STATUS_ACTIVE")).toBe(false);
    expect(isArchivedProject("active")).toBe(false);
    expect(isArchivedProject("")).toBe(false);
    expect(isArchivedProject(undefined)).toBe(false);
  });

  it("renders the status word the same way for both forms", () => {
    expect(projectStatusWord(ProjectStatus.ACTIVE)).toBe("active");
    expect(projectStatusWord(ProjectStatus.ARCHIVED)).toBe("archived");
    expect(projectStatusWord("PROJECT_STATUS_ARCHIVED")).toBe("archived");
    expect(projectStatusWord("archived")).toBe("archived");
    // Unknown values render as nothing rather than as a word that might be wrong. The cast is deliberate: a
    // server that grew a status this client has not been rebuilt for is exactly the case being guarded, and it
    // is unreachable through the type system by construction — which is why it needs a test.
    expect(projectStatusWord(ProjectStatus.UNSPECIFIED)).toBe("");
    expect(projectStatusWord(999 as ProjectStatus)).toBe("");
    expect(projectStatusWord(undefined)).toBe("");
  });
});
