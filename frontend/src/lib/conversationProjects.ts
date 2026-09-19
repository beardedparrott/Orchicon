// conversationProjects.ts — the PROJECT SCOPE of the conversations sidebar, as pure functions.
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
// So a project is a SCOPE, not a sibling view: you pick one, and the categories reappear inside it holding only
// that project's conversations. The category machinery is untouched — it just receives a narrower set of items.
//
// These live here as pure functions because the sidebar is a 2900-line route component this project's test setup
// cannot render (there is no `@testing-library/react`, so no renderHook). Logic left inline there is logic
// nothing can assert.

import { ProjectStatus } from "@/api/gen/orchicon/api/v1/project_pb";

/**
 * ALL_PROJECTS is the scope that filters nothing.
 *
 * It is the DEFAULT, and that is deliberate rather than lazy: the project column is additive and defaults to
 * empty, so every conversation that existed before this feature is unassigned. A default that hid them would
 * make an operator's entire chat history appear to have been deleted on first load.
 */
export const ALL_PROJECTS = "__all__";

/** The minimum a conversation must expose to be scoped. Structural, so the proto type satisfies it. */
export interface ScopedConversation {
  id: string;
  projectId?: string;
}

/** The minimum a project must expose to be an option. */
export interface ScopedProject {
  id: string;
  name: string;
  status?: ProjectStatus | string;
}

/**
 * filterConversationsByScope returns the conversations visible in a scope.
 *
 * A scope is a project id, "" for the unassigned ones, or ALL_PROJECTS. Note that "" is a MEANINGFUL scope and
 * not the same as ALL_PROJECTS: unassigned conversations are a group you can look at, move things into, and
 * need to find. Conflating the two would leave them unreachable.
 */
export function filterConversationsByScope<T extends ScopedConversation>(
  conversations: readonly T[] | undefined,
  scope: string,
): T[] {
  const all = conversations ?? [];
  if (scope === ALL_PROJECTS) return [...all];
  return all.filter((c) => (c.projectId ?? "") === scope);
}

/**
 * projectStatusWord renders a project's status as the domain word the server uses for it.
 *
 * ⚠️ THE CLIENT RECEIVES A NUMBER. `ProjectStatus` is a NUMERIC proto enum (ACTIVE = 2, ARCHIVED = 4, …), so a
 * helper that only compares STRINGS silently mis-classifies the wire value: comparing 2 against
 * "PROJECT_STATUS_ACTIVE" is true, and an ACTIVE project would be reported as archived. That was a real bug in
 * the first version of this file; it compiled because the type error surfaced at the call site instead.
 *
 * Both forms are accepted so one helper serves every caller. UNSPECIFIED and anything unrecognised map to "" —
 * a status the client cannot name is better rendered as nothing than as a word that might be wrong.
 */
export function projectStatusWord(status: ProjectStatus | string | undefined): string {
  if (typeof status === "number") {
    switch (status) {
      case ProjectStatus.DRAFTING:
        return "drafting";
      case ProjectStatus.ACTIVE:
        return "active";
      case ProjectStatus.PAUSED:
        return "paused";
      case ProjectStatus.ARCHIVED:
        return "archived";
      case ProjectStatus.DELETED:
        return "deleted";
      default:
        return ""; // UNSPECIFIED, or a value this client does not know
    }
  }
  if (typeof status === "string") {
    const s = status.trim().toLowerCase();
    if (s === "") return "";
    // The enum's NAME form, which some serializations produce (bypassing the numeric enum).
    if (s.startsWith("project_status_")) {
      const word = s.slice("project_status_".length);
      return word === "unspecified" ? "" : word;
    }
    return s;
  }
  return "";
}

/**
 * isArchivedProject reports whether a project's status means "not active".
 *
 * The association rule is the operator's "active or otherwise": an archived project is a valid home for a
 * conversation, and the one fact worth showing is that it is not active. UNSPECIFIED does not qualify — calling
 * a working project archived is the worse error. See projectStatusWord for the numeric trap.
 */
export function isArchivedProject(status: ProjectStatus | string | undefined): boolean {
  const word = projectStatusWord(status);
  return word !== "" && word !== "active";
}

/** A scope as it appears in the dropdown. */
export interface ScopeOption {
  /** The scope value: a project id, "" for unassigned, or ALL_PROJECTS. */
  value: string;
  label: string;
  /** How many conversations the scope holds — a count of the ACTUAL items, not of the project's rows. */
  count: number;
  /** True when the project is not active, so the option can say so. */
  archived: boolean;
}

/**
 * scopeOptions builds the dropdown's contents: All projects, every project, any project id still referenced by
 * a conversation, and No project — the last first-class because it is a place conversations live.
 *
 * AN UNKNOWN ID GETS AN OPTION. The project column carries no foreign key, so a conversation can outlive its
 * project; without an option for that id the chat would be unreachable from the sidebar entirely, which is the
 * worst outcome and a silent one. It is labelled with the raw id, which is ugly and honest.
 *
 * EVERY PROJECT IS LISTED even with zero conversations — the operator's "for every project that is created
 * (active or otherwise)". The count then reads 0, which is information rather than an omission: it is how you
 * know the scope is empty before clicking it.
 */
export function scopeOptions(
  projects: readonly ScopedProject[] | undefined,
  conversations: readonly ScopedConversation[] | undefined,
): ScopeOption[] {
  const counts = new Map<string, number>();
  for (const c of conversations ?? []) {
    const k = c.projectId ?? "";
    counts.set(k, (counts.get(k) ?? 0) + 1);
  }
  const known = new Set<string>();
  const out: ScopeOption[] = [
    { value: ALL_PROJECTS, label: "All projects", count: (conversations ?? []).length, archived: false },
  ];
  for (const p of projects ?? []) {
    known.add(p.id);
    out.push({
      value: p.id,
      label: p.name,
      count: counts.get(p.id) ?? 0,
      archived: isArchivedProject(p.status),
    });
  }
  // Referenced-but-missing project ids, in a stable order so the menu does not reshuffle between renders.
  const orphaned = [...counts.keys()].filter((k) => k !== "" && !known.has(k)).sort();
  for (const id of orphaned) {
    out.push({ value: id, label: `unknown project ${id}`, count: counts.get(id) ?? 0, archived: true });
  }
  out.push({ value: "", label: "No project", count: counts.get("") ?? 0, archived: false });
  return out;
}

/** scopeLabel is the name of a scope, for the dropdown's closed state. */
export function scopeLabel(scope: string, options: readonly ScopeOption[]): string {
  return options.find((o) => o.value === scope)?.label ?? scope;
}

/**
 * categoriesForScope returns the FOLDERS a scope should show.
 *
 * The operator, on the first build that had a project dropdown:
 *
 *   "In the GUI, folders are visible no matter what project you are on. This is wrong. You should only see the
 *    categories/folders of the currently selected project."
 *
 * They were looking at "No project" — a scope holding ZERO conversations — and seeing all five of their folders,
 * because the sidebar rendered every category it knew about rather than the ones the scope actually used. The
 * folders were not empty; their conversations were simply in another project.
 *
 * ALL PROJECTS IS EXEMPT, and that exemption is load-bearing rather than a special case. That scope filters
 * nothing, so it is where a folder you JUST created lives: creating one assigns no conversations (the dialog
 * takes only a name and a description), so hiding empty folders everywhere would make a brand-new folder vanish
 * the instant it was made — and the drag-into-it gesture it exists for would be impossible. It is also the
 * documented behaviour the other grouped lists already have (see the TUI's categoryGroupsFor: "EMPTY CATEGORIES
 * ARE INCLUDED, which is the GUI's behaviour ... so an operator who created 'Frontend' sees the folder even
 * before anything is in it").
 *
 * In a SPECIFIC scope the opposite is true: a folder with nothing in it THERE is not part of that workspace, and
 * showing it is what the operator reported as wrong. A folder that holds conversations in another project is
 * exactly the case they saw — five folders, all of them belonging to Orchicon, on a screen scoped elsewhere.
 *
 * FILTERING BY MEMBERSHIP IS SAFE FOR THE UNDERLYING GROUPING, which is why this can be a display rule rather
 * than a change to the grouping itself: a dropped folder has no members IN THE SCOPE by definition, so no scoped
 * conversation can be misfiled into "Uncategorized" by its removal.
 */
export function categoriesForScope<T extends { id: string }>(
  categories: readonly T[],
  categorized: ReadonlyMap<string, string[]>,
  scope: string,
): T[] {
  if (scope === ALL_PROJECTS) return [...categories];
  return categories.filter((c) => (categorized.get(c.id)?.length ?? 0) > 0);
}
