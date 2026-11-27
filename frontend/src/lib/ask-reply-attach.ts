// ask-reply-attach.ts — SHOULD THIS REPLY VERDICT ARM A STREAM SLOT FOR A NEW TURN?
//
// `ReplyPermissionAsk` answers a consent card, and the two adapters answer through different server
// paths:
//
//   - the orchicon adapter's ask is answered INSIDE the turn that asked it (the turn is still running
//     and this client is already consuming its stream), so the server starts nothing and names no turn.
//   - an MCP-served ask (claude/opencode/antigravity) is answered by CONTINUING the conversation: the
//     asking turn had already ended holding the awaiting marker, so the reply arrives on a brand-new
//     turn THIS CLIENT NEVER OPENED.
//
// The response's `assistantMessageId` names that new turn (empty on every path that starts none). The
// decision below is a pure function of the verdict so it can be tested without rendering the route: the
// operator's report — "the reply only shows up after clicking away from the conversation and back" when
// the reply was short — is exactly the case where nothing attached to the turn the answer started.
//
// The GUI leg of the same fix. The TUI's is internal/tui/app.go attachAnsweredTurn.

/** The slice of a ReplyPermissionAskResponse this decision reads. */
export interface AnsweredAskVerdict {
  /** applied is the server saying the reply was routed / the answer was accepted. */
  applied: boolean;
  /** expired means the ask was no longer open — nothing was sent, so no turn started. */
  expired: boolean;
  /** assistantMessageId names the new turn the answer started ("" when it started none). */
  assistantMessageId: string;
}

/**
 * Returns the assistant message id of the turn this verdict started, or "" when it started none and
 * there is nothing to attach to.
 *
 * An EXPIRED / unapplied verdict starts no turn however it is shaped, so it can never arm a slot — and
 * arming one would leave the UI claiming a reply that will never arrive.
 */
export function answeredTurnToAttach(v: AnsweredAskVerdict): string {
  if (!v.applied || v.expired) return "";
  return v.assistantMessageId ?? "";
}
