// ask-live-merge.ts — WHICH OF THE TWO COPIES OF THE RUNNING REPLY IS DRAWN.
//
// THE SAME TEXT ARRIVES TWICE, AND THAT IS BY DESIGN. While a turn runs, the server both
//   (a) streams the reply as it is written (TextChunk / Reasoning), and
//   (b) upserts the turn's collected text into the ACKED assistant row every 250ms (the partial
//       MIRROR) so a client that lost the stream can watch the reply grow through ListMessages.
// See internal/askorchicon/chat.go (the delta arm + upsertPartialMessage).
//
// A transcript that draws both shows the reply TWICE — measured in a real browser against the
// shared fixture plane (which emits TextChunk AND seeds the mirror row): two <p> elements, each
// carrying the whole reply. Drawing one of them is therefore a decision, not a rendering detail.
//
// THE STREAM WINS WHILE IT IS LIVE, for the reason the mirror exists at all: it is the same text
// arriving sooner, chunk by chunk, so it is the copy that can actually grow. The mirror row is the
// FALLBACK — a client with no live stream (a reload, a second tab, a turn started elsewhere) has
// empty live items and draws the row, which is exactly how that case was already handled. When the
// turn ends the live items are cleared, the row becomes the only copy, and the finalize has written
// the complete reply into it.
//
// THIS IS A SEAM, NOT A HIDDEN ROW: the row is suppressed ONLY while it is the in-flight one
// (`pendingReplyId`, the id TurnStarted acked) AND this client holds live text/reasoning to show
// instead. A completed turn has no live items, so the durable transcript is never withheld.

export interface MirrorRowMessage {
  id: string;
  /**
   * toolCalls is what makes the mirror row more than a copy of the live text.
   *
   * THE BUG THIS FIELD FIXES. The supersession rule drops the in-flight row while live items are drawing
   * the reply, and that was right for TEXT — the live copy is the same text, drawn sooner. But the row also
   * carries the turn's TOOL CALLS, and a recorded `ask_user` call is what renders the clarifying card
   * (MessageBubble: `message.toolCalls.find(isAskUserToolCall)`). So dropping the row dropped the card:
   * the operator saw "Orchicon is weighing… · no output for 123s" with a card sitting unrendered, and a
   * page reload showed it — because a fresh load has NO live items, so the row (and its card) rendered.
   * Reloading was not "refreshing stale state"; it was the only path that drew the row at all.
   *
   * The live stream does not carry tool calls, so nothing else can render them. The row is therefore kept
   * whenever it carries any.
   */
  toolCalls?: readonly unknown[] | null;
}

/**
 * mirrorRowSuperseded answers whether the in-flight assistant row should be SKIPPED because the
 * live stream is already drawing the same reply.
 *
 * @param messageId     the row being considered for the transcript
 * @param pendingReplyId the acked in-flight assistant message id (null when no turn is known)
 * @param isStreaming   this client's own live slot for the conversation
 * @param hasLiveItems  this client holds live text/reasoning items to render instead
 */
export function mirrorRowSuperseded(
  message: MirrorRowMessage,
  pendingReplyId: string | null,
  isStreaming: boolean,
  hasLiveItems: boolean,
): boolean {
  if (!isStreaming) return false; // no live stream here: the row IS the view (reload / other tab)
  if (!pendingReplyId) return false; // no acked id: cannot tell which row is the mirror
  if (message.id !== pendingReplyId) return false; // only the in-flight row is superseded
  // No live items to draw would make this a BLANK turn, so the mirror is kept as the fallback.
  if (!hasLiveItems) return false;
  // AND A ROW CARRYING TOOL CALLS IS NEVER SUPERSEDED, even though its TEXT is a duplicate of the live
  // copy. The live stream carries text and reasoning only — never tool calls — so this row is the ONLY
  // thing that can render the turn's card, its tool ledger, or a recorded ask_user call. Dropping it for
  // the sake of the text would take those with it, which is exactly what happened: the card existed in
  // the transcript and could not be drawn, and only a reload (which has no live items) revealed it.
  if ((message.toolCalls ?? []).length > 0) return false;
  return true;
}

/**
 * dropSupersededMirror removes the in-flight assistant row from a transcript when the live stream
 * is already drawing it, so the reply appears exactly once. Order and every other row are
 * untouched.
 */
export function dropSupersededMirror<T extends MirrorRowMessage>(
  messages: readonly T[],
  pendingReplyId: string | null,
  isStreaming: boolean,
  hasLiveItems: boolean,
): T[] {
  if (!isStreaming || !hasLiveItems || !pendingReplyId) return [...messages];
  return messages.filter((m) => !mirrorRowSuperseded(m, pendingReplyId, isStreaming, hasLiveItems));
}

/** hasLiveTextItems reports whether the grouped stream holds text/reasoning worth drawing. */
export function hasLiveTextItems(items: readonly { kind: string }[]): boolean {
  return items.some((i) => i.kind === "text" || i.kind === "reasoning");
}
