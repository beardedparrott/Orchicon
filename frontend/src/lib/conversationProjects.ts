// conversationProjects.ts — the two pieces of logic behind the conversations sidebar's PROJECT level.
//
// They live here, as pure functions, for one reason: the sidebar is a 2900-line route component that cannot be
// rendered in this project's test setup (there is no renderHook/`@testing-library/react`), so logic left inline
// there is logic nothing can assert. Pulled out, the drop routing — which decides whether a drag lands in a
// project, a category folder or the unassigned zone, and therefore which RPC runs — becomes something a test
// can pin.
//
// The operator: "We need to make a second higher level in organization for conversations. It should be another
// drop down where all of the conversations are associated with Projects in a parent category. For every project
// that is created (active or otherwise), there should be a list that can be dragged to and also created from."

import { ProjectStatus } from "@/api/gen/orchicon/api/v1/project_pb";

/**
 * PROJECT_DROP_PREFIX namespaces a project folder's droppable id.
 *
 * It exists because three kinds of drop target share one DndContext: project folders, category folders, and the
 * uncategorized zone. A category id is opaque, so without a prefix a project and a category could collide — and
 * the collision would silently move a conversation into the wrong thing.
 */
export const PROJECT_DROP_PREFIX = "proj:";

/** projectDropId is the droppable id for a project folder. The unassigned group uses the bare prefix. */
export function projectDropId(projectId: string): string {
  return PROJECT_DROP_PREFIX + projectId;
}

/**
 * projectIdFromDropId returns the project a drop id addresses, or null when it is not a project folder.
 *
 * The null case is load-bearing: the caller must be able to tell "this is a project, and its id is the empty
 * string (unassign)" from "this is a category folder". Both are otherwise falsy-ish, and treating the second
 * as the first would unassign a conversation on every drop onto a folder.
 */
export function projectIdFromDropId(dropId: string): string | null {
  if (!dropId.startsWith(PROJECT_DROP_PREFIX)) return null;
  return dropId.slice(PROJECT_DROP_PREFIX.length);
}

/** The minimum a conversation must expose to be grouped. Structural, so the proto type satisfies it. */
export interface ProjectGroupable {
  id: string;
  projectId?: string;
}

/**
 * groupConversationsByProject buckets conversation ids by project, with "" holding the unassigned ones.
 *
 * IT GROUPS THE CONVERSATIONS, NOT THE PROJECT LIST. A conversation whose project has been deleted still has to
 * appear somewhere — an orphaned chat that vanished from the sidebar would be unreachable — so the buckets come
 * from the conversations themselves and the project folders are rendered from the project list beside them. A
 * project id that matches nothing still yields a bucket, which the sidebar shows as an "unknown project" folder
 * rather than dropping the chat.
 */
export function groupConversationsByProject(
  conversations: readonly ProjectGroupable[] | undefined,
): Map<string, string[]> {
  const out = new Map<string, string[]>();
  for (const c of conversations ?? []) {
    const key = c.projectId ?? "";
    const list = out.get(key);
    if (list) list.push(c.id);
    else out.set(key, [c.id]);
  }
  return out;
}

/**
 * projectStatusWord renders a project's status as the domain word the server uses for it.
 *
 * ⚠️ THE CLIENT RECEIVES A NUMBER. `ProjectStatus` is a NUMERIC proto enum (ACTIVE = 2, ARCHIVED = 4, …), so a
 * helper that only compares STRINGS silently mis-classifies the wire value: comparing 2 against
 * "PROJECT_STATUS_ACTIVE" is true, and an ACTIVE project would be reported as archived. That is precisely the
 * bug this function replaced — it compiled because the type error was at the call site, and the first version
 * of these tests missed it because they only exercised the string spellings.
 *
 * Both forms are accepted so one helper serves every caller, including one that already normalized. UNSPECIFIED
 * and anything unrecognised map to "" — a status the client cannot name is better rendered as nothing than as a
 * word that might be wrong.
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
 * conversation, and the one fact worth showing is that it is ARCHIVED. Every non-active status qualifies
 * (drafting, paused, archived, deleted) because none of them means "ready to work in"; UNSPECIFIED does not,
 * because calling a working project archived is the worse error. See projectStatusWord for the numeric trap.
 */
export function isArchivedProject(status: ProjectStatus | string | undefined): boolean {
  const word = projectStatusWord(status);
  return word !== "" && word !== "active";
}
