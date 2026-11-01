import { MessageDeleteMode } from "@/api/askOrchicon";

// ask-message-actions — the ONE mapping from the operator's verb to the server's enum.
//
// IT IS A SEPARATE PURE MODULE SO IT CAN BE TESTED FOR REAL. The frontend suite is source-scan only (no jsdom,
// no testing-library), so a mapping inlined in a component could only be asserted by grepping the file — and a
// grep cannot tell you that Undo sends ONLY rather than AND_FOLLOWING. These two verbs destroy different amounts
// of history, so an inverted mapping would silently delete a conversation's tail when the operator asked to
// forget one message.
//
// (The same class of mistake already reached the field once: a protobuf message field assigned a plain object
// literal instead of a constructed message, which no source scan would have caught. Pure functions are the part
// of this UI that CAN be checked behaviourally, so the risky decisions live here.)

export const MessageAction = {
  /** Remove exactly one message; its reply stays. */
  Undo: "undo",
  /** Remove that message and everything after it. */
  Revert: "revert",
} as const;

export type MessageActionValue = (typeof MessageAction)[keyof typeof MessageAction];

/**
 * modeForAction maps the operator's verb onto the wire enum.
 *
 * Undo is the NARROW one: it removes the single message and leaves later turns alone, which is the whole point
 * of having two verbs rather than one.
 */
export function modeForAction(action: MessageActionValue): number {
  return action === MessageAction.Undo ? MessageDeleteMode.ONLY : MessageDeleteMode.AND_FOLLOWING;
}

/** isUndo reports whether the action is the narrow one, for the confirmation copy. */
export function isUndo(action: MessageActionValue): boolean {
  return action === MessageAction.Undo;
}

/**
 * confirmationTitle / confirmationBody describe exactly how much is about to be removed.
 *
 * The copy names what goes AND what survives, because "Revert" alone does not tell an operator whether the
 * message they clicked is kept — and the answer differs between the two verbs.
 */
export function confirmationTitle(action: MessageActionValue): string {
  return isUndo(action) ? "Undo this message?" : "Revert to this message?";
}

/**
 * restoresComposer reports whether this verb hands the message's text back to the composer.
 *
 * ONLY REVERT DOES. It removes the message, so without the restore the text is gone from the interface and the
 * operator retypes a question they had already written. Undo leaves the REPLY in place, so restoring the question
 * would set up a duplicate — the transcript would already read as asked and answered.
 *
 * It lives here, beside the verb→mode mapping, because it is the same kind of decision: a per-verb rule that a
 * component must not re-derive. An inverted answer is silent — the text simply does not appear.
 */
export function restoresComposer(action: MessageActionValue): boolean {
  return !isUndo(action);
}

export function confirmationBody(action: MessageActionValue): string {
  return isUndo(action)
    ? "This message is removed from the transcript and from the model's context. Its reply stays, and nothing else changes."
    : "This message and everything after it are removed from the transcript and from the model's context. " +
        "The conversation ends at the turn before it, and this message's text goes back into the composer so " +
        "you can send it again.";
}
