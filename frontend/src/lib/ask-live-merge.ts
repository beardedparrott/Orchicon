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
  messageId: string,
  pendingReplyId: string | null,
  isStreaming: boolean,
  hasLiveItems: boolean,
): boolean {
  if (!isStreaming) return false; // no live stream here: the row IS the view (reload / other tab)
  if (!pendingReplyId) return false; // no acked id: cannot tell which row is the mirror
  if (messageId !== pendingReplyId) return false; // only the in-flight row is superseded
  // No live items to draw would make this a BLANK turn, so the mirror is kept as the fallback.
  return hasLiveItems;
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
  return messages.filter((m) => !mirrorRowSuperseded(m.id, pendingReplyId, isStreaming, hasLiveItems));
}

/** hasLiveTextItems reports whether the grouped stream holds text/reasoning worth drawing. */
export function hasLiveTextItems(items: readonly { kind: string }[]): boolean {
  return items.some((i) => i.kind === "text" || i.kind === "reasoning");
}
