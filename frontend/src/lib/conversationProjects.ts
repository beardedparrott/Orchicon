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
 * isArchivedProject reports whether a project's status means "not active".
 *
 * The API reports the status as the enum's NAME ("PROJECT_STATUS_ARCHIVED") while the server's domain word is
 * lowercase ("archived"), and both spellings reach the client depending on where a value came from — so both
 * are accepted. An unknown or empty status is NOT archived: marking a working project as archived is a worse
 * error than leaving the marker off.
 */
export function isArchivedProject(status: string | undefined): boolean {
  if (!status) return false;
  return status !== "PROJECT_STATUS_ACTIVE" && status !== "active";
}
