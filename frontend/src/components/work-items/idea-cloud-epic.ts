// The Idea Cloud container epic — the per-project anchor every automation-spawned
// idea hangs from (server side: internal/workitem/idea_epic.go).
//
// It exists so that promoting an idea can never orphan it. Ideas are spawned under this
// epic rather than under whatever parent a research run invented, so when an operator
// promotes one child the parent is already a normal, visible work item and the child's
// hierarchy still resolves.
//
// DUPLICATED FROM THE GO CONSTANT ON PURPOSE, and pinned by a test: the frontend cannot
// import Go, and the alternative — filtering "any top-level epic" or matching a
// localStorage-cached id — would either hide real epics or drift the moment the server
// renames the container. The identity here (kind epic + no parent + this exact title)
// mirrors ResolveIdeaCloudEpic's lookup exactly.
export const IDEA_CLOUD_EPIC_TITLE = "Idea Cloud"; // workitem.IdeaCloudEpicTitle
