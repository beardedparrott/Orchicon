// conversationProjects.test.ts — the conversations sidebar's PROJECT SCOPE, at the seams where it can be wrong.
//
// The operator, correcting the first attempt:
//
//   "The implementation is wrong. You separated categories out as 'folders' and then you made them two
//    completely different panes. ... I wanted a hierarchy. So a conversation would belong to a project and
//    inside the project it would still have the normal categories we had before. I think the better alternative
//    to what you did would be a dropdown at the top of the conversation bar that allows you to pick a project,
//    and then under that project you would only see THAT PROJECT'S Conversations and Categories. Projects are
//    WORKSPACES essentially."
//
// The sidebar is a route component this test setup cannot render, so the scope logic was pulled into
// lib/conversationProjects as pure functions. These tests pin the two things that decide what the operator
// actually sees: WHICH conversations a scope contains, and WHAT the dropdown offers.

import { describe, expect, it } from "vitest";

import { ProjectStatus } from "@/api/gen/orchicon/api/v1/project_pb";
import {
  ALL_PROJECTS,
  categoriesForScope,
  filterConversationsByScope,
  isArchivedProject,
  projectStatusWord,
  scopeLabel,
  scopeOptions,
} from "@/lib/conversationProjects";

const convs = [
  { id: "c1", projectId: "p-1" },
  { id: "c2", projectId: "p-2" },
  { id: "c3", projectId: "p-1" },
  { id: "c4", projectId: "" },
  { id: "c5" }, // no projectId at all — the same state as ""
  { id: "c6", projectId: "p-gone" }, // its project no longer exists
];

const projects = [
  { id: "p-1", name: "Alpha", status: ProjectStatus.ACTIVE },
  { id: "p-2", name: "Beta", status: ProjectStatus.ACTIVE },
  { id: "p-3", name: "Gamma", status: ProjectStatus.ARCHIVED },
];

describe("filtering conversations by scope", () => {
  it("shows ONLY the scope's conversations — the whole point of a workspace", () => {
    // The operator: "you would only see THAT PROJECT'S Conversations and Categories ... You can't see the other
    // categories and conversations from the other projects unless you click on the drop down again."
    expect(filterConversationsByScope(convs, "p-1").map((c) => c.id)).toEqual(["c1", "c3"]);
    expect(filterConversationsByScope(convs, "p-2").map((c) => c.id)).toEqual(["c2"]);
  });

  it("excludes other projects' conversations from a project scope", () => {
    const got = filterConversationsByScope(convs, "p-1").map((c) => c.id);
    for (const other of ["c2", "c4", "c5", "c6"]) {
      expect(got).not.toContain(other);
    }
  });

  it("treats ALL_PROJECTS as filtering nothing", () => {
    expect(filterConversationsByScope(convs, ALL_PROJECTS)).toHaveLength(convs.length);
  });

  it("treats the EMPTY scope as the unassigned group, not as 'all'", () => {
    // "" is a meaningful scope — unassigned chats are a place things live and must stay reachable. Conflating it
    // with ALL_PROJECTS would make them unfindable in any project scope AND unspecial in the all-project one.
    expect(filterConversationsByScope(convs, "").map((c) => c.id)).toEqual(["c4", "c5"]);
    expect(filterConversationsByScope(convs, "")).not.toHaveLength(convs.length);
  });

  it("keeps a conversation whose project is gone reachable", () => {
    // The column has no foreign key, so a chat can outlive its project. Its scope still lists it.
    expect(filterConversationsByScope(convs, "p-gone").map((c) => c.id)).toEqual(["c6"]);
  });

  it("handles an empty or missing list", () => {
    expect(filterConversationsByScope([], "p-1")).toEqual([]);
    expect(filterConversationsByScope(undefined, "p-1")).toEqual([]);
  });
});

describe("the scope dropdown's options", () => {
  it("offers All projects first, then every project, then No project", () => {
    const got = scopeOptions(projects, convs).map((o) => o.value);
    expect(got[0]).toBe(ALL_PROJECTS);
    expect(got.slice(1, 4)).toEqual(["p-1", "p-2", "p-3"]);
    expect(got[got.length - 1]).toBe("");
  });

  it("LISTS EVERY PROJECT EVEN WITH NO CONVERSATIONS, counted as 0", () => {
    // The operator: "For every project that is created (active or otherwise), there should be a list that can be
    // dragged to and also created from." A project absent from the dropdown could not be chosen as a workspace,
    // and the 0 is information: it is how you know the scope is empty before clicking it.
    const gamma = scopeOptions(projects, convs).find((o) => o.value === "p-3");
    expect(gamma).toBeDefined();
    expect(gamma?.count).toBe(0);
  });

  it("counts each scope's ACTUAL conversations", () => {
    const opts = scopeOptions(projects, convs);
    expect(opts.find((o) => o.value === ALL_PROJECTS)?.count).toBe(6);
    expect(opts.find((o) => o.value === "p-1")?.count).toBe(2);
    expect(opts.find((o) => o.value === "p-2")?.count).toBe(1);
    expect(opts.find((o) => o.value === "")?.count).toBe(2);
  });

  it("marks an archived project so the option can say so", () => {
    const opts = scopeOptions(projects, convs);
    expect(opts.find((o) => o.value === "p-3")?.archived).toBe(true);
    expect(opts.find((o) => o.value === "p-1")?.archived).toBe(false);
  });

  it("gives a project id that is only REFERENCED its own option", () => {
    // Without this the conversation would be unreachable from the sidebar entirely — the worst outcome, and a
    // silent one. It is labelled with the raw id, which is ugly and honest.
    const gone = scopeOptions(projects, convs).find((o) => o.value === "p-gone");
    expect(gone).toBeDefined();
    expect(gone?.label).toContain("p-gone");
    expect(gone?.count).toBe(1);
  });

  it("orders orphaned ids deterministically, so the menu does not reshuffle", () => {
    const twice = [
      scopeOptions(projects, convs).map((o) => o.value),
      scopeOptions(projects, convs).map((o) => o.value),
    ];
    expect(twice[0]).toEqual(twice[1]);
  });

  it("still offers All projects and No project with no projects configured", () => {
    // With nothing configured AND nothing unassigned-but-orphaned, those are the only two scopes there are.
    const unassignedOnly = [{ id: "c1", projectId: "" }];
    expect(scopeOptions([], unassignedOnly).map((o) => o.value)).toEqual([ALL_PROJECTS, ""]);
  });

  it("still surfaces an orphaned id when NO projects are configured at all", () => {
    // The orphan rule does not depend on the project list existing: a conversation referencing a project the
    // tenant cannot see must stay reachable whether or not any other project is configured. With NO projects
    // listed, every referenced id is by definition orphaned — so all of them get an option, sorted.
    //
    // (My first two expectations here were wrong in different ways, and both times the CODE was right: first I
    // asserted the orphan would vanish with the project list, then that only "p-gone" was orphaned. A
    // conversation whose project is simply not in this page's project list is unreachable by name, so it needs
    // its own option too — which is exactly what the function does.)
    const got = scopeOptions([], convs).map((o) => o.value);
    expect(got).toEqual([ALL_PROJECTS, "p-1", "p-2", "p-gone", ""]);
  });
});

describe("scopeLabel", () => {
  it("names the scope from the options", () => {
    const opts = scopeOptions(projects, convs);
    expect(scopeLabel("p-1", opts)).toBe("Alpha");
    expect(scopeLabel("", opts)).toBe("No project");
    expect(scopeLabel(ALL_PROJECTS, opts)).toBe("All projects");
  });

  it("falls back to the raw value for a scope with no option", () => {
    expect(scopeLabel("p-nope", scopeOptions(projects, convs))).toBe("p-nope");
  });
});

// FOLDERS BELONG TO THE WORKSPACE THEY HAVE CONVERSATIONS IN.
//
// The operator: "In the GUI, folders are visible no matter what project you are on. This is wrong. You should
// only see the categories/folders of the currently selected project." They were on "No project" — zero
// conversations — and seeing all five of their folders, whose conversations were all in another project.
describe("categoriesForScope", () => {
  const all = [{ id: "f-automation" }, { id: "f-workflows" }, { id: "f-docs" }];
  // Only two of the three folders hold anything in this scope; the third holds conversations in another project.
  const categorized = new Map<string, string[]>([
    ["f-automation", ["c1"]],
    ["f-workflows", ["c2", "c3"]],
  ]);

  it("hides a folder with nothing in the current scope", () => {
    expect(categoriesForScope(all, categorized, "p-orchicon").map((c) => c.id)).toEqual([
      "f-automation",
      "f-workflows",
    ]);
  });

  it("shows NOTHING in a scope that holds no conversations", () => {
    // THE REPORTED CASE, exactly: five folders on a screen scoped to an empty workspace.
    expect(categoriesForScope(all, new Map(), "")).toEqual([]);
  });

  it("keeps EVERY folder in All projects", () => {
    // This is what lets a folder you just created be visible: creating one assigns no conversations, so a
    // membership rule applied here would make it vanish the instant it was made — and the folder exists to be
    // dragged into. It is also the documented behaviour of the other grouped lists.
    expect(categoriesForScope(all, new Map(), ALL_PROJECTS)).toEqual(all);
  });

  it("preserves the server's category order", () => {
    // The order comes from the categories list (sort_order, then name); this filter must not reshuffle it.
    const ordered = [{ id: "f-docs" }, { id: "f-automation" }, { id: "f-workflows" }];
    expect(categoriesForScope(ordered, categorized, "p-1").map((c) => c.id)).toEqual([
      "f-automation",
      "f-workflows",
    ]);
  });
});

describe("project status", () => {
  // ⚠️ THE NUMERIC ENUM IS THE CASE THAT MATTERS. The client receives a NUMBER (ACTIVE = 2, ARCHIVED = 4), and a
  // string-only comparison classifies 2 as "not active" — every ACTIVE project would be marked archived.
  it("classifies the NUMERIC proto enum correctly", () => {
    expect(isArchivedProject(ProjectStatus.ACTIVE)).toBe(false);
    expect(isArchivedProject(ProjectStatus.ARCHIVED)).toBe(true);
    expect(isArchivedProject(ProjectStatus.PAUSED)).toBe(true);
    expect(isArchivedProject(ProjectStatus.DRAFTING)).toBe(true);
    expect(isArchivedProject(ProjectStatus.DELETED)).toBe(true);
    expect(isArchivedProject(ProjectStatus.UNSPECIFIED)).toBe(false);
  });

  it("still recognises both string spellings", () => {
    expect(isArchivedProject("PROJECT_STATUS_ARCHIVED")).toBe(true);
    expect(isArchivedProject("archived")).toBe(true);
    expect(isArchivedProject("PROJECT_STATUS_ACTIVE")).toBe(false);
    expect(isArchivedProject("active")).toBe(false);
  });

  it("does not mark an unknown or absent status", () => {
    expect(isArchivedProject("")).toBe(false);
    expect(isArchivedProject(undefined)).toBe(false);
  });

  it("renders the status word the same way for both forms", () => {
    expect(projectStatusWord(ProjectStatus.ACTIVE)).toBe("active");
    expect(projectStatusWord(ProjectStatus.ARCHIVED)).toBe("archived");
    expect(projectStatusWord("PROJECT_STATUS_ARCHIVED")).toBe("archived");
    expect(projectStatusWord("archived")).toBe("archived");
    expect(projectStatusWord(ProjectStatus.UNSPECIFIED)).toBe("");
    expect(projectStatusWord(999 as ProjectStatus)).toBe("");
    expect(projectStatusWord(undefined)).toBe("");
  });
});
